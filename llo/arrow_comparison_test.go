package llo

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// ============================================================================
// BEFORE/AFTER COMPARISON BENCHMARKS
// ============================================================================
//
// These benchmarks compare the original implementation with the new Arrow-based
// implementation. Run with:
//
//   go test ./llo/... -bench=Comparison -benchmem -count=5
//
// The "Before_Original" benchmarks use the existing implementation.
// The "After_Arrow" benchmarks use the new Arrow-based implementation.
// ============================================================================

// BenchmarkComparison_MedianAggregation compares median aggregation performance.
func BenchmarkComparison_MedianAggregation(b *testing.B) {
	sizes := []int{10, 100, 1000, 5000, 10000}

	for _, size := range sizes {
		name := fmt.Sprintf("%d_observations", size)

		// Setup: create test data for original implementation
		originalValues := make([]StreamValue, size)
		for i := 0; i < size; i++ {
			originalValues[i] = ToDecimal(decimal.NewFromFloat(float64(rand.Intn(10000))))
		}

		// Setup: create test data for Arrow implementation
		pool := NewArrowBuilderPool(0)
		agg := NewArrowAggregator(pool, nil)
		arrowObs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals:  make([]decimal.Decimal, size),
		}
		for i := 0; i < size; i++ {
			arrowObs.decimals[i] = decimal.NewFromFloat(float64(rand.Intn(10000)))
		}

		b.Run("Before_Original/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = MedianAggregator(originalValues, 1)
			}
		})

		b.Run("After_Arrow/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = agg.medianAggregate(arrowObs, 1)
			}
		})
	}
}

// BenchmarkComparison_QuoteAggregation compares quote aggregation performance.
func BenchmarkComparison_QuoteAggregation(b *testing.B) {
	sizes := []int{10, 100, 1000, 5000}

	for _, size := range sizes {
		name := fmt.Sprintf("%d_observations", size)

		// Setup: create test data for original implementation
		originalValues := make([]StreamValue, size)
		for i := 0; i < size; i++ {
			base := float64(rand.Intn(10000))
			originalValues[i] = &Quote{
				Bid:       decimal.NewFromFloat(base - 0.5),
				Benchmark: decimal.NewFromFloat(base),
				Ask:       decimal.NewFromFloat(base + 0.5),
			}
		}

		// Setup: create test data for Arrow implementation
		pool := NewArrowBuilderPool(0)
		agg := NewArrowAggregator(pool, nil)
		arrowObs := &streamObservations{
			valueType: StreamValueTypeQuote,
			quotes:    make([]*Quote, size),
		}
		for i := 0; i < size; i++ {
			base := float64(rand.Intn(10000))
			arrowObs.quotes[i] = &Quote{
				Bid:       decimal.NewFromFloat(base - 0.5),
				Benchmark: decimal.NewFromFloat(base),
				Ask:       decimal.NewFromFloat(base + 0.5),
			}
		}

		b.Run("Before_Original/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = QuoteAggregator(originalValues, 1)
			}
		})

		b.Run("After_Arrow/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = agg.quoteAggregate(arrowObs, 1)
			}
		})
	}
}

// BenchmarkComparison_ModeAggregation compares mode aggregation performance.
func BenchmarkComparison_ModeAggregation(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, size := range sizes {
		name := fmt.Sprintf("%d_observations", size)

		// Create values with some repetition for mode to work
		numUnique := size / 5 // 20% unique values
		if numUnique < 3 {
			numUnique = 3
		}

		// Setup: create test data for original implementation
		originalValues := make([]StreamValue, size)
		for i := 0; i < size; i++ {
			originalValues[i] = ToDecimal(decimal.NewFromInt(int64(i % numUnique)))
		}

		// Setup: create test data for Arrow implementation
		pool := NewArrowBuilderPool(0)
		agg := NewArrowAggregator(pool, nil)
		arrowObs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals:  make([]decimal.Decimal, size),
		}
		for i := 0; i < size; i++ {
			arrowObs.decimals[i] = decimal.NewFromInt(int64(i % numUnique))
		}

		b.Run("Before_Original/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = ModeAggregator(originalValues, 1)
			}
		})

		b.Run("After_Arrow/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = agg.modeAggregate(arrowObs, 1)
			}
		})
	}
}

// BenchmarkComparison_StreamValuesConversion compares map vs Arrow record operations.
func BenchmarkComparison_StreamValuesConversion(b *testing.B) {
	sizes := []int{100, 1000, 5000, 10000}

	for _, size := range sizes {
		name := fmt.Sprintf("%d_streams", size)

		// Setup: create test data
		values := make(map[llotypes.StreamID]StreamValue, size)
		for i := 0; i < size; i++ {
			values[llotypes.StreamID(i)] = ToDecimal(decimal.NewFromFloat(float64(i)))
		}

		pool := NewArrowBuilderPool(0)

		b.Run("Before_Original_MapIteration/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Simulate iterating over map (common operation)
				count := 0
				for _, v := range values {
					if v != nil {
						count++
					}
				}
				_ = count
			}
		})

		b.Run("After_Arrow_ToRecord/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				record, _ := StreamValuesToArrowRecord(values, pool)
				if record != nil {
					record.Release()
				}
			}
		})

		// Create Arrow record for FromRecord benchmark
		record, _ := StreamValuesToArrowRecord(values, pool)
		defer record.Release()

		b.Run("After_Arrow_FromRecord/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = ArrowRecordToStreamValues(record)
			}
		})
	}
}

// BenchmarkComparison_MemoryAllocation measures memory allocation patterns.
func BenchmarkComparison_MemoryAllocation(b *testing.B) {
	sizes := []int{100, 1000, 10000}

	for _, size := range sizes {
		name := fmt.Sprintf("%d_values", size)

		b.Run("Before_Original_MapAllocation/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				values := make(map[llotypes.StreamID]StreamValue, size)
				for j := 0; j < size; j++ {
					values[llotypes.StreamID(j)] = ToDecimal(decimal.NewFromFloat(float64(j)))
				}
				_ = values
			}
		})

		pool := NewArrowBuilderPool(0)

		b.Run("After_Arrow_PooledAllocation/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				builder := pool.GetObservationBuilder()
				// Simulate adding data - must fill all fields for valid record
				for j := 0; j < size; j++ {
					builder.Field(ObsColObserverID).(*array.Uint8Builder).Append(1)
					builder.Field(ObsColStreamID).(*array.Uint32Builder).Append(uint32(j))
					builder.Field(ObsColValueType).(*array.Uint8Builder).Append(StreamValueTypeDecimal)
					builder.Field(ObsColDecimalValue).(*array.BinaryBuilder).AppendNull()
					builder.Field(ObsColQuoteBid).(*array.BinaryBuilder).AppendNull()
					builder.Field(ObsColQuoteBenchmark).(*array.BinaryBuilder).AppendNull()
					builder.Field(ObsColQuoteAsk).(*array.BinaryBuilder).AppendNull()
					builder.Field(ObsColObservedAtNs).(*array.Uint64Builder).Append(0)
					builder.Field(ObsColTimestampNs).(*array.Uint64Builder).Append(0)
				}
				record := builder.NewRecord()
				record.Release()
				pool.PutObservationBuilder(builder)
			}
		})
	}
}

// BenchmarkComparison_EndToEnd simulates full aggregation pipeline.
func BenchmarkComparison_EndToEnd(b *testing.B) {
	sizes := []int{100, 500, 1000}
	numObservers := 5 // Simulate 5 nodes

	for _, size := range sizes {
		name := fmt.Sprintf("%d_streams_%d_observers", size, numObservers)

		// Setup: simulate observations from multiple nodes
		allObservations := make([][]StreamValue, numObservers)
		for obs := 0; obs < numObservers; obs++ {
			allObservations[obs] = make([]StreamValue, size)
			for i := 0; i < size; i++ {
				// Add some variance between observers
				allObservations[obs][i] = ToDecimal(decimal.NewFromFloat(float64(i) + float64(obs)*0.1))
			}
		}

		b.Run("Before_Original/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Merge observations (simple concatenation for streams)
				merged := make([]StreamValue, 0, size*numObservers)
				for _, obs := range allObservations {
					merged = append(merged, obs...)
				}
				// Aggregate per stream (simplified: just take all)
				_, _ = MedianAggregator(merged[:size], 1)
			}
		})

		pool := NewArrowBuilderPool(0)
		agg := NewArrowAggregator(pool, nil)

		// Pre-build Arrow observations
		arrowObs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals:  make([]decimal.Decimal, size*numObservers),
		}
		idx := 0
		for obs := 0; obs < numObservers; obs++ {
			for i := 0; i < size; i++ {
				arrowObs.decimals[idx] = decimal.NewFromFloat(float64(i) + float64(obs)*0.1)
				idx++
			}
		}

		b.Run("After_Arrow/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = agg.medianAggregate(arrowObs, 1)
			}
		})
	}
}

// ============================================================================
// RESULT EQUIVALENCE TESTS
// ============================================================================
// These tests verify that the Arrow implementation produces the same results
// as the original implementation.
// ============================================================================

// TestResultEquivalence_MedianAggregator verifies Arrow median matches original.
func TestResultEquivalence_MedianAggregator(t *testing.T) {
	sizes := []int{5, 10, 21, 100}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			// Create identical test data
			values := make([]decimal.Decimal, size)
			for i := 0; i < size; i++ {
				values[i] = decimal.NewFromFloat(float64(i * 10))
			}

			// Original implementation
			originalInput := make([]StreamValue, size)
			for i := 0; i < size; i++ {
				originalInput[i] = ToDecimal(values[i])
			}
			originalResult, err := MedianAggregator(originalInput, 1)
			require.NoError(t, err)

			// Arrow implementation
			pool := NewArrowBuilderPool(0)
			agg := NewArrowAggregator(pool, nil)
			arrowObs := &streamObservations{
				valueType: StreamValueTypeDecimal,
				decimals:  values,
			}
			arrowResult, err := agg.medianAggregate(arrowObs, 1)
			require.NoError(t, err)

			// Compare results
			originalDec := originalResult.(*Decimal).Decimal()
			arrowDec := arrowResult.(*Decimal).Decimal()
			assert.True(t, originalDec.Equal(arrowDec),
				"Results differ: original=%s, arrow=%s", originalDec, arrowDec)
		})
	}
}

// TestResultEquivalence_QuoteAggregator verifies Arrow quote matches original.
func TestResultEquivalence_QuoteAggregator(t *testing.T) {
	sizes := []int{5, 10, 21}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			// Create identical test data
			quotes := make([]*Quote, size)
			for i := 0; i < size; i++ {
				base := float64(i * 10)
				quotes[i] = &Quote{
					Bid:       decimal.NewFromFloat(base - 1),
					Benchmark: decimal.NewFromFloat(base),
					Ask:       decimal.NewFromFloat(base + 1),
				}
			}

			// Original implementation
			originalInput := make([]StreamValue, size)
			for i := 0; i < size; i++ {
				originalInput[i] = quotes[i]
			}
			originalResult, err := QuoteAggregator(originalInput, 1)
			require.NoError(t, err)

			// Arrow implementation
			pool := NewArrowBuilderPool(0)
			agg := NewArrowAggregator(pool, nil)
			arrowObs := &streamObservations{
				valueType: StreamValueTypeQuote,
				quotes:    quotes,
			}
			arrowResult, err := agg.quoteAggregate(arrowObs, 1)
			require.NoError(t, err)

			// Compare results
			originalQuote := originalResult.(*Quote)
			arrowQuote := arrowResult.(*Quote)

			assert.True(t, originalQuote.Bid.Equal(arrowQuote.Bid),
				"Bid differs: original=%s, arrow=%s", originalQuote.Bid, arrowQuote.Bid)
			assert.True(t, originalQuote.Benchmark.Equal(arrowQuote.Benchmark),
				"Benchmark differs: original=%s, arrow=%s", originalQuote.Benchmark, arrowQuote.Benchmark)
			assert.True(t, originalQuote.Ask.Equal(arrowQuote.Ask),
				"Ask differs: original=%s, arrow=%s", originalQuote.Ask, arrowQuote.Ask)
		})
	}
}

// TestResultEquivalence_ModeAggregator verifies Arrow mode matches original.
func TestResultEquivalence_ModeAggregator(t *testing.T) {
	t.Run("clear_mode", func(t *testing.T) {
		// Create data with a clear mode
		values := []decimal.Decimal{
			decimal.NewFromInt(10),
			decimal.NewFromInt(20),
			decimal.NewFromInt(20),
			decimal.NewFromInt(20),
			decimal.NewFromInt(30),
		}

		// Original implementation
		originalInput := make([]StreamValue, len(values))
		for i, v := range values {
			originalInput[i] = ToDecimal(v)
		}
		originalResult, err := ModeAggregator(originalInput, 2)
		require.NoError(t, err)

		// Arrow implementation
		pool := NewArrowBuilderPool(0)
		agg := NewArrowAggregator(pool, nil)
		arrowObs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals:  values,
		}
		arrowResult, err := agg.modeAggregate(arrowObs, 2)
		require.NoError(t, err)

		// Compare results
		originalDec := originalResult.(*Decimal).Decimal()
		arrowDec := arrowResult.(*Decimal).Decimal()
		assert.True(t, originalDec.Equal(arrowDec),
			"Results differ: original=%s, arrow=%s", originalDec, arrowDec)
		assert.True(t, decimal.NewFromInt(20).Equal(arrowDec), "Expected mode to be 20")
	})
}
