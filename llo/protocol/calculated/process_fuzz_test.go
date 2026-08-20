package calculated

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// fuzzAnchorNs anchors the round. It matches the anchor FuzzEvaluateExpression
// uses so the two fuzzers see windows positioned the same way.
const fuzzAnchorNs = 1_000 * uint64(1_000_000_000)

// fuzzStreamValue is the observed value every stream carries. The values are not
// what is under test here — the round is — so one fixed, well behaved number
// keeps the fuzzer spending its budget on expression text.
var fuzzStreamValue = decimal.NewFromInt(3)

// fuzzRoundChannels is wide enough to put the worker pool to work and to place
// several channels on each worker.
const fuzzRoundChannels = 16

// fuzzRound builds a round of channels that all evaluate the same expression.
// The expression is the fuzzed input; the shape around it is fixed, because what
// is under test is the round, not the channel definition decoder.
func fuzzRound(expression string) (llotypes.ChannelDefinitions, protocol.StreamAggregates) {
	defs := llotypes.ChannelDefinitions{}
	for c := range fuzzRoundChannels {
		defs[llotypes.ChannelID(c+1)] = llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams:      medianStreams(1, 2),
			Opts: fmt.Appendf(nil,
				`{"abi":[{"type":"int256","expression":%q,"expressionStreamID":%d}]}`,
				expression, 1000+c),
		}
	}
	return defs, protocol.StreamAggregates{
		1: {llotypes.AggregatorMedian: protocol.ToDecimal(fuzzStreamValue)},
		2: {llotypes.AggregatorMedian: protocol.ToDecimal(fuzzStreamValue)},
	}
}

// FuzzProcessCalculatedStreams fuzzes a whole round rather than one expression:
// preparation, concurrent evaluation and the sequential apply that commits the
// results.
//
// FuzzEvaluateExpression covers what one expression does to the function
// library. This covers what a round of them does to the shared structures around
// it — the environment pool, the analysis and program caches, the aggregates map
// every channel writes into — none of which the single-expression path touches
// under concurrency.
//
// Two properties are asserted. Nothing panics: a panic in an oracle's state
// transition takes the node down, and an expression arrives from replicated
// channel definitions, so it is not trusted to be sensible. And the round is
// deterministic: evaluating the same input twice must produce the same
// aggregates, whatever order the workers finished in. A result that depended on
// scheduling would diverge between oracles and stall the protocol, which is a
// far worse failure than an expression that simply cannot be evaluated.
func FuzzProcessCalculatedStreams(f *testing.F) {
	for _, expression := range []string{
		"Add(s1, s2)",
		"Div(s1, s2)",
		"Pow(s1, s2)",
		"Ln(s1)",
		"Avg(History(s1, 3))",
		"EMA(History(s1, 3), 2)",
		"SMA(History(s1, 3), 2)",
		"Stddev(History(s1, 3))",
		`TWAP(History(s1, 3), {window: Duration("3s"), minSamples: 1, maxHeadGap: 3, maxInteriorGap: 3, maxTailGap: 3})`,
		// Deeper than the reader serves: the whole round takes the warmup gate.
		"Avg(History(s1, 64))",
		// Two windows in one expression, so binding order is exercised.
		"Add(Avg(History(s1, 2)), Avg(History(s2, 2)))",
		"",
		"((((",
	} {
		// withHistory false covers the v30 shape, where the reader is nil and
		// any expression using History must fail closed rather than evaluate.
		f.Add(expression, true)
		f.Add(expression, false)
	}

	f.Fuzz(func(t *testing.T, expression string, withHistory bool) {
		var reader HistoryReader
		if withHistory {
			// Typed nil would defeat the nil check in resolveHistory, so the
			// reader is only assigned when one is wanted.
			reader = fuzzHistoryReader{seed: 3, scale: 0}
		}

		first := runFuzzRound(t, expression, reader)
		second := runFuzzRound(t, expression, reader)
		require.Equal(t, first, second, "round was not deterministic for %q", expression)
	})
}

// runFuzzRound evaluates one round and returns what it committed: the calculated
// aggregates, and the streams each channel definition ended up carrying. Both are
// outcome state, so both must be identical across runs.
func runFuzzRound(t *testing.T, expression string, reader HistoryReader) map[string]string {
	t.Helper()

	defs, aggregates := fuzzRound(expression)
	// Nop rather than Test: a round of unevaluable expressions logs one error per
	// channel, and the fuzzer runs a great many rounds.
	ProcessCalculatedStreams(logger.Nop(), defs, aggregates, fuzzAnchorNs, protocol.NewOptsCache(), reader)

	out := make(map[string]string, len(aggregates)+len(defs))
	for sid, byAggregator := range aggregates {
		for aggregator, value := range byAggregator {
			// Assert rather than format: %v on a StreamValue prints a pointer,
			// which differs between runs and would pass this test for the wrong
			// reason.
			d, ok := value.(*protocol.Decimal)
			require.True(t, ok, "unexpected stream value type %T", value)
			out[fmt.Sprintf("aggregate/%d/%d", sid, aggregator)] = d.Decimal().String()
		}
	}
	for cid, cd := range defs {
		out[fmt.Sprintf("streams/%d", cid)] = fmt.Sprint(cd.Streams)
	}
	return out
}
