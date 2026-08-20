package calculated

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Evaluation benchmarks.
//
// The figure to keep in mind is the round interval, on the order of a second:
// every expression of every channel is evaluated inside one state transition, so
// per-expression cost multiplies by the channel count.
//
// TWAP is the one to watch. It takes a logarithm per observed bucket and an
// exponential per bucket in the window, and all of those serialize on the
// process-wide transcendental lock (see decimalmath.go), so its cost does not
// parallelize across the plugin instances sharing a process.

func benchSeries(depth int, intervalSeconds int) Series {
	values := make([]decimal.Decimal, 0, depth)
	timestamps := make([]uint64, 0, depth)
	for i := range depth {
		values = append(values, decimal.New(int64(110000000000000000+i), -8))
		timestamps = append(timestamps, uint64((i+1)*intervalSeconds)*uint64(time.Second))
	}
	s, err := NewSeries(values, timestamps)
	if err != nil {
		panic(err)
	}
	return s
}

func BenchmarkWindowFunctions(b *testing.B) {
	for _, depth := range []int{10, 300, 1024} {
		window := benchSeries(depth, 1)

		for name, fn := range map[string]func(any) (decimal.Decimal, error){
			"Count":     Count,
			"Median":    Median,
			"Variance":  Variance,
			"Stddev":    Stddev,
			"PctChange": PctChange,
			"Spread":    Spread,
		} {
			b.Run(fmt.Sprintf("%s/depth=%d", name, depth), func(b *testing.B) {
				for range b.N {
					if _, err := fn(window); err != nil {
						b.Fatal(err)
					}
				}
			})
		}

		for name, fn := range map[string]func(any, any) (decimal.Decimal, error){
			"SMA": SMA, "WMA": WMA, "EMA": EMA,
		} {
			b.Run(fmt.Sprintf("%s/depth=%d", name, depth), func(b *testing.B) {
				for range b.N {
					if _, err := fn(window, depth/2); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkTWAP measures the settlement-window sizes an operator would actually
// configure.
func BenchmarkTWAP(b *testing.B) {
	for _, windowSeconds := range []int{60, 300, 900} {
		// One record per second, fully covering the window.
		window := benchSeries(windowSeconds, 1)
		anchorNs := uint64(windowSeconds+1) * uint64(time.Second)
		cfg := map[string]any{
			"window":         time.Duration(windowSeconds) * time.Second,
			"minSamples":     windowSeconds / 2,
			"maxHeadGap":     windowSeconds,
			"maxInteriorGap": windowSeconds,
			"maxTailGap":     windowSeconds,
		}
		twap := twapFunc(anchorNs)

		b.Run(fmt.Sprintf("window=%ds", windowSeconds), func(b *testing.B) {
			for range b.N {
				if _, err := twap(window, cfg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTWAPSparse is the worst case for the filling strategy: only interior
// interpolation needs log space, so cost scales with how much of the window is
// missing. A window observed every Nth second is the expensive shape.
func BenchmarkTWAPSparse(b *testing.B) {
	const windowSeconds = 300

	for _, everyNth := range []int{1, 2, 5, 30} {
		depth := windowSeconds / everyNth
		window := benchSeries(depth, everyNth)
		anchorNs := uint64(windowSeconds+1) * uint64(time.Second)
		cfg := map[string]any{
			"window":         time.Duration(windowSeconds) * time.Second,
			"minSamples":     1,
			"maxHeadGap":     windowSeconds,
			"maxInteriorGap": windowSeconds,
			"maxTailGap":     windowSeconds,
		}
		twap := twapFunc(anchorNs)

		b.Run(fmt.Sprintf("observedEvery=%ds", everyNth), func(b *testing.B) {
			for range b.N {
				if _, err := twap(window, cfg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTWAPRealisticGaps measures the worst case a production acceptance rule
// actually admits.
//
// The permissive thresholds in BenchmarkTWAPSparse exist to force interpolation
// and show where the cost lives; they are not deployable. With the spec's example
// thresholds (minSamples 240 of 300, maxInteriorGap 10) a window can be missing at
// most 60 buckets, so interpolation is bounded no matter how the gaps fall.
func BenchmarkTWAPRealisticGaps(b *testing.B) {
	const windowSeconds = 300
	const minSamples = 240

	// 240 observations in a 300-second window, with the 60 missing buckets spread
	// as 20 interior gaps of 3 — within a maxInteriorGap of 10.
	values := make([]decimal.Decimal, 0, minSamples)
	timestamps := make([]uint64, 0, minSamples)
	second := 0
	for len(values) < minSamples && second < windowSeconds {
		if second%15 >= 12 { // 3 missing out of every 15
			second++
			continue
		}
		values = append(values, decimal.New(int64(110000000000000000+second), -8))
		timestamps = append(timestamps, uint64(second+1)*uint64(time.Second))
		second++
	}
	window, err := NewSeries(values, timestamps)
	if err != nil {
		b.Fatal(err)
	}

	cfg := map[string]any{
		"window":         time.Duration(windowSeconds) * time.Second,
		"minSamples":     len(values),
		"maxHeadGap":     10,
		"maxInteriorGap": 10,
		"maxTailGap":     10,
	}
	twap := twapFunc(uint64(windowSeconds+1) * uint64(time.Second))

	b.ReportMetric(float64(windowSeconds-len(values)), "missingBuckets")
	b.ResetTimer()
	for range b.N {
		if _, err := twap(window, cfg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTWAPParallel shows what the transcendental lock costs when several
// plugin instances evaluate TWAP at once. Compare ns/op against the serial
// benchmark: no speedup means the lock is the limit.
func BenchmarkTWAPParallel(b *testing.B) {
	const windowSeconds = 300
	window := benchSeries(windowSeconds, 1)
	anchorNs := uint64(windowSeconds+1) * uint64(time.Second)
	cfg := map[string]any{
		"window":         time.Duration(windowSeconds) * time.Second,
		"minSamples":     windowSeconds / 2,
		"maxHeadGap":     windowSeconds,
		"maxInteriorGap": windowSeconds,
		"maxTailGap":     windowSeconds,
	}
	twap := twapFunc(anchorNs)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := twap(window, cfg); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkProcessCalculatedStreams measures a whole round's expression work,
// which is what the round budget actually has to absorb.
func BenchmarkProcessCalculatedStreams(b *testing.B) {
	for _, tc := range []struct {
		name       string
		expression string
		depth      uint32
	}{
		{"scalar", "Add(s1, s2)", 0},
		{"avg/depth=300", "Avg(History(s1, 300))", 300},
		{"ema/depth=300", "EMA(History(s1, 300), 20)", 300},
		{"twap/window=300", `TWAP(History(s1, 300), {window: Duration("5m"), minSamples: 150, maxHeadGap: 300, maxInteriorGap: 300, maxTailGap: 300})`, 300},
	} {
		for _, channels := range []int{1, 32} {
			b.Run(fmt.Sprintf("%s/channels=%d", tc.name, channels), func(b *testing.B) {
				defs := llotypes.ChannelDefinitions{}
				for c := range channels {
					defs[llotypes.ChannelID(c+1)] = llotypes.ChannelDefinition{
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
							{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(fmt.Sprintf(
							`{"abi":[{"type":"int256","expression":%q,"expressionStreamID":%d}]}`,
							tc.expression, 900+c)),
					}
				}
				cache := protocol.NewOptsCache()
				cache.ResetTo(defs)
				lggr := logger.Test(b)

				var reader HistoryReader
				if tc.depth > 0 {
					reader = benchReader{window: benchSeries(int(tc.depth), 1)}
				}
				// The anchor must sit just after the newest record or a
				// window-relative function sees an empty window.
				anchorNs := uint64(int(tc.depth)+1) * uint64(time.Second)

				b.ResetTimer()
				for range b.N {
					aggregates := protocol.StreamAggregates{
						1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
						2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(4))},
					}
					ProcessCalculatedStreams(lggr, defs, aggregates, anchorNs, cache, reader)
					if len(aggregates[900]) == 0 {
						b.Fatal("expected a calculated aggregate")
					}
				}
			})
		}
	}
}

// benchReader serves one fixed window for every request.
type benchReader struct{ window Series }

func (r benchReader) Series(_ llotypes.StreamID, _ llotypes.Aggregator, count uint32, _ Field) (Series, error) {
	if uint32(r.window.Len()) < count {
		return Series{}, ErrInsufficientHistory
	}
	return r.window, nil
}
