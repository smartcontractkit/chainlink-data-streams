package calculated

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// fuzzHistoryReader serves a window of the requested depth built from a seed, so
// evaluation is exercised against varying values rather than one fixed series.
type fuzzHistoryReader struct {
	seed  int64
	scale int32
}

func (r fuzzHistoryReader) Series(_ llotypes.StreamID, _ llotypes.Aggregator, count uint32, _ Field) (Series, error) {
	values := make([]decimal.Decimal, 0, count)
	timestamps := make([]uint64, 0, count)
	for i := range count {
		values = append(values, decimal.New(r.seed+int64(i), r.scale))
		timestamps = append(timestamps, uint64(i+1)*uint64(1_000_000_000))
	}
	return NewSeries(values, timestamps)
}

// FuzzEvaluateExpression fuzzes the whole evaluation path — analysis, the AST
// rewrite, compilation and the function library — over arbitrary expression text
// and arbitrary window contents.
//
// Expressions come from channel definitions, which are replicated state, and the
// window values come from agreed aggregates. Neither is trusted to be sensible:
// what matters is that no input reaches a panic, and that a failure is always a
// returned error. A panic in an oracle's state transition takes the node down.
//
// The unit tests cover meaning; this covers survival.
func FuzzEvaluateExpression(f *testing.F) {
	for _, expression := range []string{
		"Add(s1, s2)",
		"Div(s1, s2)",
		"Count(History(s1, 3))",
		"Avg(History(s1, 3))",
		"Median(History(s1, 3))",
		"Stddev(History(s1, 3))",
		"PctChange(History(s1, 3))",
		"Spread(History(s1, 3))",
		"Delta(History(s1, 3))",
		"SMA(History(s1, 3), 2)",
		"WMA(History(s1, 3), 2)",
		"EMA(History(s1, 3), 2)",
		"Sum(History(s1, 3))",
		"Min(History(s1, 3))",
		"Max(History(s1, 3))",
		`TWAP(History(s1, 3), {window: Duration("3s"), minSamples: 1, maxHeadGap: 3, maxInteriorGap: 3, maxTailGap: 3})`,
		"Ln(s1)",
		"Log(s1, s2)",
		"Pow(s1, s2)",
		"Sqrt(s1)",
		"Round(Div(s1, s2), 2)",
		"Add(Avg(History(s1, 2)), Avg(History(s2, 2)))",
		"",
		"((((",
		"Avg(History(s1, 0))",
	} {
		// Seeds span a zero value, a negative one and a large exponent, since
		// those are what the arithmetic tends to trip over.
		f.Add(expression, int64(1), int32(0))
		f.Add(expression, int64(0), int32(0))
		f.Add(expression, int64(-5), int32(-8))
		f.Add(expression, int64(1), int32(30))
	}

	f.Fuzz(func(t *testing.T, expression string, seed int64, scale int32) {
		// Bound the exponent: a decimal with an enormous scale makes the
		// arithmetic legitimately slow rather than incorrect, and the protocol
		// bounds it separately on decode.
		if scale > 64 || scale < -64 {
			return
		}

		env := NewEnv(1_000 * uint64(1_000_000_000))
		defer env.release()
		for _, id := range []llotypes.StreamID{1, 2} {
			require.NoError(t, env.SetStreamValue(id, protocol.ToDecimal(decimal.New(seed, scale))))
		}

		reader := fuzzHistoryReader{seed: seed, scale: scale}
		aggByStream := map[llotypes.StreamID]llotypes.Aggregator{
			1: llotypes.AggregatorMedian,
			2: llotypes.AggregatorMedian,
		}

		// Analysis first: a statically invalid expression is never evaluated, so
		// evaluating one here would test a path the plugin cannot reach.
		if err := ValidateExpression(expression); err != nil {
			return
		}
		window, err := resolveHistory(expression, reader, aggByStream)
		if err != nil {
			return
		}
		for _, bound := range window {
			env[bound.name] = bound.series
		}

		// The result is unconstrained; only the absence of a panic matters, and
		// that any failure came back as an error.
		_, _ = evalDecimal(expression, env)
	})
}
