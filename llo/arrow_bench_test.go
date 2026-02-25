package llo

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// BenchmarkMedianDecimalBatch benchmarks the median aggregation.
func BenchmarkMedianDecimalBatch(b *testing.B) {
	sizes := []int{10, 100, 1000, 10000}

	for _, size := range sizes {
		values := make([]decimal.Decimal, size)
		for i := 0; i < size; i++ {
			values[i] = decimal.NewFromFloat(float64(i))
		}

		b.Run(fmt.Sprintf("size_%d", len(values)), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = MedianDecimalBatch(values, 1)
			}
		})
	}
}

// BenchmarkStreamValuesToArrowRecord benchmarks conversion to Arrow.
func BenchmarkStreamValuesToArrowRecord(b *testing.B) {
	pool := NewArrowBuilderPool(0)

	sizes := []int{100, 1000, 10000}

	for _, size := range sizes {
		values := make(map[llotypes.StreamID]StreamValue, size)
		for i := 0; i < size; i++ {
			values[llotypes.StreamID(i)] = ToDecimal(decimal.NewFromFloat(float64(i)))
		}

		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				record, _ := StreamValuesToArrowRecord(values, pool)
				if record != nil {
					record.Release()
				}
			}
		})
	}
}

// BenchmarkArrowRecordToStreamValues benchmarks conversion from Arrow.
func BenchmarkArrowRecordToStreamValues(b *testing.B) {
	pool := NewArrowBuilderPool(0)

	sizes := []int{100, 1000, 10000}

	for _, size := range sizes {
		values := make(map[llotypes.StreamID]StreamValue, size)
		for i := 0; i < size; i++ {
			values[llotypes.StreamID(i)] = ToDecimal(decimal.NewFromFloat(float64(i)))
		}

		record, _ := StreamValuesToArrowRecord(values, pool)

		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = ArrowRecordToStreamValues(record)
			}
		})

		record.Release()
	}
}

// BenchmarkArrowAggregator_MedianAggregate benchmarks median aggregation.
func BenchmarkArrowAggregator_MedianAggregate(b *testing.B) {
	pool := NewArrowBuilderPool(0)
	agg := NewArrowAggregator(pool, nil)

	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		obs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals:  make([]decimal.Decimal, size),
		}
		for i := 0; i < size; i++ {
			obs.decimals[i] = decimal.NewFromFloat(float64(i))
		}

		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = agg.medianAggregate(obs, 1)
			}
		})
	}
}

// BenchmarkArrowAggregator_QuoteAggregate benchmarks quote aggregation.
func BenchmarkArrowAggregator_QuoteAggregate(b *testing.B) {
	pool := NewArrowBuilderPool(0)
	agg := NewArrowAggregator(pool, nil)

	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		obs := &streamObservations{
			valueType: StreamValueTypeQuote,
			quotes:    make([]*Quote, size),
		}
		for i := 0; i < size; i++ {
			obs.quotes[i] = &Quote{
				Bid:       decimal.NewFromFloat(float64(i) - 0.5),
				Benchmark: decimal.NewFromFloat(float64(i)),
				Ask:       decimal.NewFromFloat(float64(i) + 0.5),
			}
		}

		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = agg.quoteAggregate(obs, 1)
			}
		})
	}
}

// BenchmarkDecimalConversion benchmarks decimal to/from bytes conversion.
func BenchmarkDecimalConversion(b *testing.B) {
	d := decimal.NewFromFloat(123456789.123456789)

	b.Run("to_bytes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = DecimalToBytes(d)
		}
	})

	bytes, _ := DecimalToBytes(d)
	b.Run("from_bytes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = BytesToDecimal(bytes)
		}
	})
}

// BenchmarkBuilderPool benchmarks the builder pool.
func BenchmarkBuilderPool(b *testing.B) {
	pool := NewArrowBuilderPool(0)

	b.Run("get_put", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			builder := pool.GetObservationBuilder()
			pool.PutObservationBuilder(builder)
		}
	})

	b.Run("get_build_put", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			builder := pool.GetObservationBuilder()
			// Simulate some work
			_ = builder.NewRecord()
			pool.PutObservationBuilder(builder)
		}
	})
}

// BenchmarkMemoryPoolAllocation benchmarks memory pool allocations.
func BenchmarkMemoryPoolAllocation(b *testing.B) {
	pool := NewLLOMemoryPool(0)

	sizes := []int{64, 256, 1024, 4096, 16384}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				buf := pool.Allocate(size)
				pool.Free(buf)
			}
		})
	}
}

// BenchmarkExistingMedianAggregator benchmarks the existing implementation for comparison.
func BenchmarkExistingMedianAggregator(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		values := make([]StreamValue, size)
		for i := 0; i < size; i++ {
			values[i] = ToDecimal(decimal.NewFromFloat(float64(i)))
		}

		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = MedianAggregator(values, 1)
			}
		})
	}
}

// BenchmarkExistingQuoteAggregator benchmarks the existing implementation for comparison.
func BenchmarkExistingQuoteAggregator(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		values := make([]StreamValue, size)
		for i := 0; i < size; i++ {
			values[i] = &Quote{
				Bid:       decimal.NewFromFloat(float64(i) - 0.5),
				Benchmark: decimal.NewFromFloat(float64(i)),
				Ask:       decimal.NewFromFloat(float64(i) + 0.5),
			}
		}

		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = QuoteAggregator(values, 1)
			}
		})
	}
}
