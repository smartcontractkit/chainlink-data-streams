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
