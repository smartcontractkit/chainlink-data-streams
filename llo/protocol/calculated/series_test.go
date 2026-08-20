package calculated

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// --- test doubles ---

// stubHistoryReader serves fixed windows and counts reads, so tests can assert
// both the values an expression saw and that a shared window is not read twice.
type stubHistoryReader struct {
	windows map[histRequest]Series
	err     map[histRequest]error
	reads   map[histRequest]int
}

type histRequest struct {
	streamID   llotypes.StreamID
	aggregator llotypes.Aggregator
	count      uint32
	field      Field
}

func newStubHistoryReader() *stubHistoryReader {
	return &stubHistoryReader{
		windows: map[histRequest]Series{},
		err:     map[histRequest]error{},
		reads:   map[histRequest]int{},
	}
}

func (r *stubHistoryReader) set(streamID llotypes.StreamID, aggregator llotypes.Aggregator, count uint32, field Field, values ...int64) {
	decimals := make([]decimal.Decimal, 0, len(values))
	timestamps := make([]uint64, 0, len(values))
	for i, v := range values {
		decimals = append(decimals, decimal.NewFromInt(v))
		timestamps = append(timestamps, uint64(i+1)*1_000)
	}
	series, err := NewSeries(decimals, timestamps)
	if err != nil {
		panic(err)
	}
	r.windows[histRequest{streamID, aggregator, count, field}] = series
}

func (r *stubHistoryReader) setErr(streamID llotypes.StreamID, aggregator llotypes.Aggregator, count uint32, field Field, err error) {
	r.err[histRequest{streamID, aggregator, count, field}] = err
}

func (r *stubHistoryReader) Series(streamID llotypes.StreamID, aggregator llotypes.Aggregator, count uint32, field Field) (Series, error) {
	key := histRequest{streamID, aggregator, count, field}
	r.reads[key]++
	if err, ok := r.err[key]; ok {
		return Series{}, err
	}
	series, ok := r.windows[key]
	if !ok {
		return Series{}, fmt.Errorf("%w: no window for stream %d aggregator %d depth %d field %s",
			ErrInsufficientHistory, streamID, aggregator, count, field)
	}
	return series, nil
}

var _ HistoryReader = (*stubHistoryReader)(nil)

// --- Series ---

func TestNewSeries(t *testing.T) {
	t.Parallel()

	s, err := NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(2)},
		[]uint64{10, 20},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, s.Len())

	newest, err := s.Newest()
	require.NoError(t, err)
	assert.True(t, decimal.NewFromInt(2).Equal(newest))

	oldest, err := s.Oldest()
	require.NoError(t, err)
	assert.True(t, decimal.NewFromInt(1).Equal(oldest))

	_, err = NewSeries([]decimal.Decimal{decimal.NewFromInt(1)}, []uint64{1, 2})
	require.ErrorContains(t, err, "1 values but 2 timestamps")

	// Timestamps must be strictly increasing: a duplicate would mean two values
	// observed at the same instant, which the append rule makes impossible.
	_, err = NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(2)},
		[]uint64{10, 10},
	)
	require.ErrorContains(t, err, "strictly increasing")

	_, err = NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(2)},
		[]uint64{20, 10},
	)
	require.ErrorContains(t, err, "strictly increasing")
}

func TestSeries_EmptyAccessors(t *testing.T) {
	t.Parallel()

	var s Series
	assert.Equal(t, 0, s.Len())
	_, err := s.Newest()
	require.ErrorIs(t, err, ErrInsufficientHistory)
	_, err = s.Oldest()
	require.ErrorIs(t, err, ErrInsufficientHistory)
}

func TestSeriesFromRecords(t *testing.T) {
	t.Parallel()

	quote := &protocol.Quote{
		Bid:       decimal.NewFromInt(10),
		Benchmark: decimal.NewFromInt(11),
		Ask:       decimal.NewFromInt(12),
	}
	quoteRecords := []protocol.StreamHistoryRecord{
		{ObservedAtNanoseconds: 1_000, Value: quote},
		{ObservedAtNanoseconds: 2_000, Value: quote},
	}

	// One stored window serves every field.
	for field, want := range map[Field]int64{
		FieldBid:       10,
		FieldBenchmark: 11,
		FieldAsk:       12,
	} {
		s, err := SeriesFromRecords(quoteRecords, field)
		require.NoError(t, err, "field %s", field)
		require.Equal(t, 2, s.Len())
		assert.True(t, decimal.NewFromInt(want).Equal(s.Values()[0]), "field %s", field)
		assert.Equal(t, []uint64{1_000, 2_000}, s.Timestamps())
	}

	// A quote has no bare value: picking one of the three silently would be
	// worse than refusing.
	_, err := SeriesFromRecords(quoteRecords, FieldValue)
	require.ErrorContains(t, err, "use the _bid, _ask or _benchmark field")

	decimalRecords := []protocol.StreamHistoryRecord{
		{ObservedAtNanoseconds: 1_000, Value: protocol.ToDecimal(decimal.NewFromInt(7))},
	}
	s, err := SeriesFromRecords(decimalRecords, FieldValue)
	require.NoError(t, err)
	assert.True(t, decimal.NewFromInt(7).Equal(s.Values()[0]))

	_, err = SeriesFromRecords(decimalRecords, FieldBid)
	require.ErrorContains(t, err, "has no bid field")

	// A timestamped value is unwrapped: the timestamp is already carried per
	// record.
	timestampedRecords := []protocol.StreamHistoryRecord{
		{ObservedAtNanoseconds: 5_000, Value: &protocol.TimestampedStreamValue{
			ObservedAtNanoseconds: 4_999,
			StreamValue:           protocol.ToDecimal(decimal.NewFromInt(3)),
		}},
	}
	s, err = SeriesFromRecords(timestampedRecords, FieldValue)
	require.NoError(t, err)
	assert.True(t, decimal.NewFromInt(3).Equal(s.Values()[0]))
	assert.Equal(t, []uint64{5_000}, s.Timestamps(), "the record timestamp wins, not the wrapped one")

	_, err = SeriesFromRecords([]protocol.StreamHistoryRecord{{ObservedAtNanoseconds: 1, Value: nil}}, FieldValue)
	require.ErrorIs(t, err, protocol.ErrNilStreamValue)

	empty, err := SeriesFromRecords(nil, FieldValue)
	require.NoError(t, err)
	assert.Equal(t, 0, empty.Len())
}

func TestCount(t *testing.T) {
	t.Parallel()

	s, err := NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(2), decimal.NewFromInt(3)},
		[]uint64{1, 2, 3},
	)
	require.NoError(t, err)

	got, err := Count(s)
	require.NoError(t, err)
	assert.True(t, decimal.NewFromInt(3).Equal(got))

	_, err = Count(decimal.NewFromInt(1))
	require.ErrorContains(t, err, "expects a history window")
}

// TestToDecimalRejectsSeries is the backstop for a bypassed static check: a
// window must never be coerced into a number.
func TestToDecimalRejectsSeries(t *testing.T) {
	t.Parallel()

	s, err := NewSeries([]decimal.Decimal{decimal.NewFromInt(5)}, []uint64{1})
	require.NoError(t, err)

	_, err = toDecimal(s)
	require.ErrorIs(t, err, ErrSeriesAsScalar)
	assert.Contains(t, err.Error(), "length 1")
}

// --- ProcessCalculatedStreams with history ---

func historyChannel(streams []llotypes.Stream, expression string) llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Streams:      streams,
		Opts:         []byte(fmt.Sprintf(`{"abi":[{"type":"int256","expression":%q,"expressionStreamID":999}]}`, expression)),
	}
}

func medianStreams(ids ...llotypes.StreamID) []llotypes.Stream {
	streams := make([]llotypes.Stream, 0, len(ids))
	for _, id := range ids {
		streams = append(streams, llotypes.Stream{StreamID: id, Aggregator: llotypes.AggregatorMedian})
	}
	return streams
}

func TestProcessCalculatedStreams_History(t *testing.T) {
	t.Parallel()

	t.Run("evaluates against a bound window", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Count(History(s1, 3))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		reader := newStubHistoryReader()
		reader.set(1, llotypes.AggregatorMedian, 3, FieldValue, 10, 20, 30)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		got := aggregates[999][llotypes.AggregatorCalculated]
		require.NotNil(t, got, "expected a calculated aggregate")
		value, ok := got.(*protocol.Decimal)
		require.True(t, ok)
		assert.True(t, decimal.NewFromInt(3).Equal(value.Decimal()), "got %s", value.Decimal())
	})

	t.Run("insufficient history leaves no aggregate", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Count(History(s1, 300))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		reader := newStubHistoryReader()
		reader.setErr(1, llotypes.AggregatorMedian, 300, FieldValue, ErrInsufficientHistory)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		assert.Empty(t, aggregates[999], "warmup must not write an aggregate")
	})

	t.Run("nil reader fails closed", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Count(History(s1, 3))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
		}

		// This is the v30 path: no replicated state, so no history.
		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), nil)

		assert.Empty(t, aggregates[999], "history must not be evaluated when unavailable")
	})

	t.Run("history-free expressions are unaffected by a nil reader", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1, 2), "Add(s1, s2)"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
			2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(4))},
		}

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), nil)

		value, ok := aggregates[999][llotypes.AggregatorCalculated].(*protocol.Decimal)
		require.True(t, ok)
		assert.True(t, decimal.NewFromInt(7).Equal(value.Decimal()))
	})

	t.Run("reads the aggregator the channel declares", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel([]llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMode}}, "Count(History(s1, 2))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMode: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		reader := newStubHistoryReader()
		// Only the mode window exists; a median read would miss.
		reader.set(1, llotypes.AggregatorMode, 2, FieldValue, 1, 2)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		require.NotEmpty(t, aggregates[999])
		assert.Equal(t, 1, reader.reads[histRequest{1, llotypes.AggregatorMode, 2, FieldValue}])
		assert.Zero(t, reader.reads[histRequest{1, llotypes.AggregatorMedian, 2, FieldValue}])
	})

	t.Run("quote field is projected", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Count(History(s1_bid, 2))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: &protocol.Quote{
				Bid:       decimal.NewFromInt(1),
				Benchmark: decimal.NewFromInt(2),
				Ask:       decimal.NewFromInt(3),
			}},
		}
		reader := newStubHistoryReader()
		reader.set(1, llotypes.AggregatorMedian, 2, FieldBid, 7, 8)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		require.NotEmpty(t, aggregates[999])
		assert.Equal(t, 1, reader.reads[histRequest{1, llotypes.AggregatorMedian, 2, FieldBid}])
	})

	t.Run("a repeated window is read once per expression", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Add(Count(History(s1, 2)), Count(History(s1, 2)))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		reader := newStubHistoryReader()
		reader.set(1, llotypes.AggregatorMedian, 2, FieldValue, 1, 2)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		value, ok := aggregates[999][llotypes.AggregatorCalculated].(*protocol.Decimal)
		require.True(t, ok)
		assert.True(t, decimal.NewFromInt(4).Equal(value.Decimal()))
		assert.Equal(t, 1, reader.reads[histRequest{1, llotypes.AggregatorMedian, 2, FieldValue}],
			"duplicate references are deduplicated by analysis")
	})

	t.Run("window on a stream the channel does not observe", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Count(History(s2, 2))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		reader := newStubHistoryReader()
		reader.set(2, llotypes.AggregatorMedian, 2, FieldValue, 1, 2)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		assert.Empty(t, aggregates[999], "the channel must declare the stream it reads history for")
	})

	t.Run("ambiguous aggregator skips the channel", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel([]llotypes.Stream{
				{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
				{StreamID: 1, Aggregator: llotypes.AggregatorMode},
			}, "Count(History(s1, 2))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {
				llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5)),
				llotypes.AggregatorMode:   protocol.ToDecimal(decimal.NewFromInt(6)),
			},
		}
		reader := newStubHistoryReader()
		reader.set(1, llotypes.AggregatorMedian, 2, FieldValue, 1, 2)
		reader.set(1, llotypes.AggregatorMode, 2, FieldValue, 3, 4)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		assert.Empty(t, aggregates[999], "ambiguous aggregation must not be guessed at")
		assert.Empty(t, reader.reads, "an ambiguous channel must not read history at all")
	})

	t.Run("a short window from the reader is rejected", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Count(History(s1, 5))"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		reader := newStubHistoryReader()
		// Reader misbehaves and returns fewer records than asked for.
		reader.set(1, llotypes.AggregatorMedian, 5, FieldValue, 1, 2)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		assert.Empty(t, aggregates[999], "a differently sized window would change the result")
	})

	t.Run("a statically invalid expression is not evaluated", func(t *testing.T) {
		t.Parallel()

		defs := llotypes.ChannelDefinitions{
			1: historyChannel(medianStreams(1), "Add(History(s1, 2), 1)"),
		}
		aggregates := protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(5))},
		}
		reader := newStubHistoryReader()
		reader.set(1, llotypes.AggregatorMedian, 2, FieldValue, 1, 2)

		ProcessCalculatedStreams(logger.Test(t), defs, aggregates, 1_000, protocol.NewOptsCache(), reader)

		assert.Empty(t, aggregates[999])
		assert.Empty(t, reader.reads, "analysis rejects before any state is read")
	})
}

// TestProcessCalculatedStreams_ReleaseStripsWindows checks the pooled
// environment does not leak a window into a later round, where it would be a
// stale value bound under a name the expression trusts.
func TestProcessCalculatedStreams_ReleaseStripsWindows(t *testing.T) {
	t.Parallel()

	env := NewEnv(1_000)
	s, err := NewSeries([]decimal.Decimal{decimal.NewFromInt(1)}, []uint64{1})
	require.NoError(t, err)
	env["s1__h1"] = s
	env.release()

	next := NewEnv(1_000)
	defer next.release()
	assert.NotContains(t, next, "s1__h1")
}

// TestProcessCalculatedStreamsDryRun_History checks History expressions can be
// validated offline, where no persisted state exists.
func TestProcessCalculatedStreamsDryRun_History(t *testing.T) {
	t.Parallel()

	require.NoError(t, ProcessCalculatedStreamsDryRun("Count(History(s1, 10))"))
	require.NoError(t, ProcessCalculatedStreamsDryRun("Count(History(s1_bid, 10))"))
	require.NoError(t, ProcessCalculatedStreamsDryRun("Add(Count(History(s1, 3)), Count(History(s2, 4)))"))

	// Static rejections still apply offline.
	require.Error(t, ProcessCalculatedStreamsDryRun("Add(History(s1, 10), 1)"))
	require.Error(t, ProcessCalculatedStreamsDryRun("Count(History(s1, 0))"))
	require.Error(t, ProcessCalculatedStreamsDryRun("Count(History(s1_timestamp, 10))"))
	require.Error(t, ProcessCalculatedStreamsDryRun(fmt.Sprintf("Count(History(s1, %d))", protocol.MaxHistoryRecordsPerPair+1)))
}
