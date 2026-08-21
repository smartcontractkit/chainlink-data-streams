package calculated

import (
	"fmt"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// concurrentAnchorNs sits just past the newest synthesized record so that
// window-relative functions see a populated window.
const concurrentAnchorNs = uint64(1_000_000)

// countingHistoryReader serves a fixed window and records how many times each
// pair was read, without any locking of its own: the reader is documented as
// being driven from one goroutine, so a concurrent read is both a race the
// detector will flag and a violation of the one-read-per-pair contract.
type countingHistoryReader struct {
	window Series
	reads  map[llotypes.StreamID]int
}

func (r *countingHistoryReader) Series(sid llotypes.StreamID, _ llotypes.Aggregator, count uint32, _ Field) (Series, error) {
	r.reads[sid]++
	if uint32(r.window.Len()) < count {
		return Series{}, ErrInsufficientHistory
	}
	return r.window, nil
}

// concurrentFixture builds a channel set wide enough to exercise the worker
// pool, mixing scalar, history and deliberately failing channels so that the
// error paths are scheduled alongside the successful ones.
func concurrentFixture(t testing.TB, channels int) (llotypes.ChannelDefinitions, protocol.StreamAggregates, *countingHistoryReader) {
	t.Helper()

	defs := llotypes.ChannelDefinitions{}
	for c := range channels {
		cid := llotypes.ChannelID(c + 1)
		expressionStreamID := llotypes.StreamID(1000 + c)

		var expression string
		switch c % 4 {
		case 0:
			expression = "Add(s1, s2)"
		case 1:
			expression = "Avg(History(s1, 4))"
		case 2:
			// Deeper than the reader holds: fails the warmup gate.
			expression = "Avg(History(s2, 64))"
		case 3:
			// Evaluation failure: division by zero.
			expression = "Div(s1, Sub(s2, s2))"
		}

		defs[cid] = llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams:      medianStreams(1, 2),
			Opts: fmt.Appendf(nil,
				`{"abi":[{"type":"int256","expression":%q,"expressionStreamID":%d}]}`,
				expression, expressionStreamID),
		}
	}

	aggregates := protocol.StreamAggregates{
		1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
		2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(4))},
	}

	values := make([]decimal.Decimal, 0, 4)
	timestamps := make([]uint64, 0, 4)
	for i := range 4 {
		values = append(values, decimal.NewFromInt(int64(i+1)))
		timestamps = append(timestamps, uint64(i+1))
	}
	window, err := NewSeries(values, timestamps)
	require.NoError(t, err)

	return defs, aggregates, &countingHistoryReader{window: window, reads: map[llotypes.StreamID]int{}}
}

// snapshot flattens the aggregates into a comparable form. Comparing the map
// directly would compare pointers, which say nothing about the values computed.
func snapshot(t testing.TB, aggregates protocol.StreamAggregates) map[llotypes.StreamID]string {
	t.Helper()

	out := make(map[llotypes.StreamID]string, len(aggregates))
	for sid, byAggregator := range aggregates {
		value, ok := byAggregator[llotypes.AggregatorCalculated]
		if !ok {
			continue
		}
		d, ok := value.(*protocol.Decimal)
		require.True(t, ok)
		out[sid] = d.Decimal().String()
	}
	return out
}

// TestProcessCalculatedStreams_Deterministic is the property concurrency must
// not cost: the same inputs must produce the same outcome every time, whatever
// order the workers happened to finish in. Run it with -race to also cover the
// shared maps the phases write.
func TestProcessCalculatedStreams_Deterministic(t *testing.T) {
	t.Parallel()

	var want map[llotypes.StreamID]string
	for round := range 32 {
		defs, aggregates, reader := concurrentFixture(t, 64)
		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, concurrentAnchorNs, protocol.NewOptsCache(), reader)

		got := snapshot(t, aggregates)
		require.NotEmpty(t, got, "expected calculated aggregates")
		if round == 0 {
			want = got
			continue
		}
		assert.Equal(t, want, got, "round %d differed", round)
	}
}

// TestProcessCalculatedStreams_HistoryReadOncePerPair pins the reader contract
// the sequential prepare phase exists to honour: however many channels ask for
// a pair, evaluation reads it once per channel and never from more than one
// goroutine. A reader driven concurrently would trip the race detector here.
func TestProcessCalculatedStreams_HistoryReadOncePerPair(t *testing.T) {
	t.Parallel()

	defs, aggregates, reader := concurrentFixture(t, 64)
	ProcessCalculatedStreams(logger.Test(t), defs, aggregates, concurrentAnchorNs, protocol.NewOptsCache(), reader)

	// 64 channels, one in four using each history expression.
	assert.Equal(t, 16, reader.reads[1])
	assert.Equal(t, 16, reader.reads[2])
}

// TestProcessCalculatedStreams_ParallelRounds covers several plugin instances
// evaluating at once, which is what the process-wide analysis cache, the
// environment pool and the compiled-program cache are shared across.
func TestProcessCalculatedStreams_ParallelRounds(t *testing.T) {
	t.Parallel()

	const rounds = 8

	results := make([]map[llotypes.StreamID]string, rounds)
	var wg sync.WaitGroup
	wg.Add(rounds)
	for i := range rounds {
		defs, aggregates, reader := concurrentFixture(t, 32)
		go func() {
			defer wg.Done()
			ProcessCalculatedStreams(logger.Test(t), defs, aggregates, concurrentAnchorNs, protocol.NewOptsCache(), reader)
			results[i] = snapshot(t, aggregates)
		}()
	}
	wg.Wait()

	for i := 1; i < rounds; i++ {
		assert.Equal(t, results[0], results[i], "round %d differed", i)
	}
}

// TestProcessCalculatedStreams_ChannelIsolation checks that a channel that
// cannot be evaluated costs only itself, even when its neighbours are evaluated
// on other workers.
func TestProcessCalculatedStreams_ChannelIsolation(t *testing.T) {
	t.Parallel()

	defs, aggregates, reader := concurrentFixture(t, 64)
	ProcessCalculatedStreams(logger.Test(t), defs, aggregates, concurrentAnchorNs, protocol.NewOptsCache(), reader)

	for c := range 64 {
		sid := llotypes.StreamID(1000 + c)
		_, ok := aggregates[sid][llotypes.AggregatorCalculated]
		switch c % 4 {
		case 0:
			assert.True(t, ok, "scalar channel %d should have produced a value", c+1)
		case 1:
			assert.True(t, ok, "history channel %d should have produced a value", c+1)
		case 2:
			assert.False(t, ok, "channel %d is short of history and must not report", c+1)
		case 3:
			assert.False(t, ok, "channel %d fails evaluation and must not report", c+1)
		}
	}
}
