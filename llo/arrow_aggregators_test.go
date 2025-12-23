package llo

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func TestDecimalToBytes(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"zero", "0"},
		{"positive", "123.456"},
		{"negative", "-789.012"},
		{"large", "999999999999999999.999999999999999999"},
		{"small", "0.000000000000000001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := decimal.NewFromString(tt.value)
			require.NoError(t, err)

			// Convert to bytes
			bytes, err := DecimalToBytes(d)
			require.NoError(t, err)
			assert.NotEmpty(t, bytes)

			// Convert back
			result, err := BytesToDecimal(bytes)
			require.NoError(t, err)
			assert.True(t, d.Equal(result), "expected %s, got %s", d, result)
		})
	}
}

func TestStreamValueToArrow(t *testing.T) {
	pool := NewArrowBuilderPool(0)
	builder := pool.GetCacheBuilder()
	defer pool.PutCacheBuilder(builder)

	valueTypeBuilder := builder.Field(CacheColValueType).(*array.Uint8Builder)
	decimalBuilder := builder.Field(CacheColDecimalValue).(*array.BinaryBuilder)
	bidBuilder := builder.Field(CacheColQuoteBid).(*array.BinaryBuilder)
	benchmarkBuilder := builder.Field(CacheColQuoteBenchmark).(*array.BinaryBuilder)
	askBuilder := builder.Field(CacheColQuoteAsk).(*array.BinaryBuilder)
	observedAtBuilder := builder.Field(CacheColObservedAtNs).(*array.Uint64Builder)

	t.Run("decimal", func(t *testing.T) {
		d := decimal.NewFromFloat(123.456)
		sv := ToDecimal(d)

		valueType, err := StreamValueToArrow(sv, valueTypeBuilder, decimalBuilder,
			bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
		require.NoError(t, err)
		assert.Equal(t, StreamValueTypeDecimal, valueType)
	})

	t.Run("quote", func(t *testing.T) {
		q := &Quote{
			Bid:       decimal.NewFromFloat(99.5),
			Benchmark: decimal.NewFromFloat(100.0),
			Ask:       decimal.NewFromFloat(100.5),
		}

		valueType, err := StreamValueToArrow(q, valueTypeBuilder, decimalBuilder,
			bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
		require.NoError(t, err)
		assert.Equal(t, StreamValueTypeQuote, valueType)
	})

	t.Run("timestamped", func(t *testing.T) {
		tsv := &TimestampedStreamValue{
			ObservedAtNanoseconds: 1234567890,
			StreamValue:           ToDecimal(decimal.NewFromFloat(42.0)),
		}

		valueType, err := StreamValueToArrow(tsv, valueTypeBuilder, decimalBuilder,
			bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
		require.NoError(t, err)
		assert.Equal(t, StreamValueTypeTimestampd, valueType)
	})

	t.Run("nil", func(t *testing.T) {
		valueType, err := StreamValueToArrow(nil, valueTypeBuilder, decimalBuilder,
			bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
		require.NoError(t, err)
		assert.Equal(t, uint8(0), valueType)
	})
}

func TestMedianDecimalBatch(t *testing.T) {
	t.Run("odd count", func(t *testing.T) {
		values := []decimal.Decimal{
			decimal.NewFromFloat(10),
			decimal.NewFromFloat(20),
			decimal.NewFromFloat(30),
			decimal.NewFromFloat(40),
			decimal.NewFromFloat(50),
		}

		result, err := MedianDecimalBatch(values, 1)
		require.NoError(t, err)
		assert.True(t, decimal.NewFromFloat(30).Equal(result))
	})

	t.Run("even count", func(t *testing.T) {
		values := []decimal.Decimal{
			decimal.NewFromFloat(10),
			decimal.NewFromFloat(20),
			decimal.NewFromFloat(30),
			decimal.NewFromFloat(40),
		}

		// With even count, we take the higher middle value (rank-k median)
		result, err := MedianDecimalBatch(values, 1)
		require.NoError(t, err)
		assert.True(t, decimal.NewFromFloat(30).Equal(result))
	})

	t.Run("unsorted input", func(t *testing.T) {
		values := []decimal.Decimal{
			decimal.NewFromFloat(50),
			decimal.NewFromFloat(10),
			decimal.NewFromFloat(40),
			decimal.NewFromFloat(20),
			decimal.NewFromFloat(30),
		}

		result, err := MedianDecimalBatch(values, 1)
		require.NoError(t, err)
		assert.True(t, decimal.NewFromFloat(30).Equal(result))
	})

	t.Run("not enough values", func(t *testing.T) {
		values := []decimal.Decimal{
			decimal.NewFromFloat(10),
		}

		_, err := MedianDecimalBatch(values, 1)
		require.Error(t, err)
	})
}

func TestArrowAggregator_MedianAggregate(t *testing.T) {
	pool := NewArrowBuilderPool(0)
	agg := NewArrowAggregator(pool, nil)

	t.Run("decimal values", func(t *testing.T) {
		obs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals: []decimal.Decimal{
				decimal.NewFromFloat(10),
				decimal.NewFromFloat(20),
				decimal.NewFromFloat(30),
				decimal.NewFromFloat(40),
				decimal.NewFromFloat(50),
			},
		}

		result, err := agg.medianAggregate(obs, 1)
		require.NoError(t, err)
		require.NotNil(t, result)

		dec, ok := result.(*Decimal)
		require.True(t, ok)
		assert.True(t, decimal.NewFromFloat(30).Equal(dec.Decimal()))
	})

	t.Run("quote values uses benchmark", func(t *testing.T) {
		obs := &streamObservations{
			valueType: StreamValueTypeQuote,
			quotes: []*Quote{
				{Bid: decimal.NewFromFloat(9), Benchmark: decimal.NewFromFloat(10), Ask: decimal.NewFromFloat(11)},
				{Bid: decimal.NewFromFloat(19), Benchmark: decimal.NewFromFloat(20), Ask: decimal.NewFromFloat(21)},
				{Bid: decimal.NewFromFloat(29), Benchmark: decimal.NewFromFloat(30), Ask: decimal.NewFromFloat(31)},
			},
		}

		result, err := agg.medianAggregate(obs, 1)
		require.NoError(t, err)
		require.NotNil(t, result)

		dec, ok := result.(*Decimal)
		require.True(t, ok)
		assert.True(t, decimal.NewFromFloat(20).Equal(dec.Decimal()))
	})

	t.Run("timestamped values", func(t *testing.T) {
		obs := &streamObservations{
			valueType: StreamValueTypeTimestampd,
			timestamps: []uint64{
				1000,
				2000,
				3000,
			},
			innerValues: []decimal.Decimal{
				decimal.NewFromFloat(10),
				decimal.NewFromFloat(20),
				decimal.NewFromFloat(30),
			},
		}

		result, err := agg.medianAggregate(obs, 1)
		require.NoError(t, err)
		require.NotNil(t, result)

		tsv, ok := result.(*TimestampedStreamValue)
		require.True(t, ok)
		assert.Equal(t, uint64(2000), tsv.ObservedAtNanoseconds)

		dec, ok := tsv.StreamValue.(*Decimal)
		require.True(t, ok)
		assert.True(t, decimal.NewFromFloat(20).Equal(dec.Decimal()))
	})
}

func TestArrowAggregator_QuoteAggregate(t *testing.T) {
	pool := NewArrowBuilderPool(0)
	agg := NewArrowAggregator(pool, nil)

	t.Run("quote aggregation", func(t *testing.T) {
		obs := &streamObservations{
			valueType: StreamValueTypeQuote,
			quotes: []*Quote{
				{Bid: decimal.NewFromFloat(95), Benchmark: decimal.NewFromFloat(100), Ask: decimal.NewFromFloat(105)},
				{Bid: decimal.NewFromFloat(96), Benchmark: decimal.NewFromFloat(101), Ask: decimal.NewFromFloat(106)},
				{Bid: decimal.NewFromFloat(97), Benchmark: decimal.NewFromFloat(102), Ask: decimal.NewFromFloat(107)},
				{Bid: decimal.NewFromFloat(98), Benchmark: decimal.NewFromFloat(103), Ask: decimal.NewFromFloat(108)},
				{Bid: decimal.NewFromFloat(99), Benchmark: decimal.NewFromFloat(104), Ask: decimal.NewFromFloat(109)},
			},
		}

		result, err := agg.quoteAggregate(obs, 1)
		require.NoError(t, err)
		require.NotNil(t, result)

		quote, ok := result.(*Quote)
		require.True(t, ok)
		assert.True(t, decimal.NewFromFloat(97).Equal(quote.Bid))
		assert.True(t, decimal.NewFromFloat(102).Equal(quote.Benchmark))
		assert.True(t, decimal.NewFromFloat(107).Equal(quote.Ask))
	})
}

func TestArrowAggregator_ModeAggregate(t *testing.T) {
	pool := NewArrowBuilderPool(0)
	agg := NewArrowAggregator(pool, nil)

	t.Run("decimal mode", func(t *testing.T) {
		obs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals: []decimal.Decimal{
				decimal.NewFromFloat(10),
				decimal.NewFromFloat(20),
				decimal.NewFromFloat(20),
				decimal.NewFromFloat(20),
				decimal.NewFromFloat(30),
			},
		}

		result, err := agg.modeAggregate(obs, 2)
		require.NoError(t, err)
		require.NotNil(t, result)

		dec, ok := result.(*Decimal)
		require.True(t, ok)
		assert.True(t, decimal.NewFromFloat(20).Equal(dec.Decimal()))
	})

	t.Run("not enough agreement", func(t *testing.T) {
		obs := &streamObservations{
			valueType: StreamValueTypeDecimal,
			decimals: []decimal.Decimal{
				decimal.NewFromFloat(10),
				decimal.NewFromFloat(20),
				decimal.NewFromFloat(30),
				decimal.NewFromFloat(40),
				decimal.NewFromFloat(50),
			},
		}

		_, err := agg.modeAggregate(obs, 2)
		require.Error(t, err)
	})
}

func TestStreamValuesToArrowRecord(t *testing.T) {
	pool := NewArrowBuilderPool(0)

	values := map[llotypes.StreamID]StreamValue{
		1: ToDecimal(decimal.NewFromFloat(100.5)),
		2: &Quote{
			Bid:       decimal.NewFromFloat(99),
			Benchmark: decimal.NewFromFloat(100),
			Ask:       decimal.NewFromFloat(101),
		},
		3: &TimestampedStreamValue{
			ObservedAtNanoseconds: 1234567890,
			StreamValue:           ToDecimal(decimal.NewFromFloat(42.0)),
		},
	}

	record, err := StreamValuesToArrowRecord(values, pool)
	require.NoError(t, err)
	require.NotNil(t, record)
	defer record.Release()

	assert.Equal(t, int64(3), record.NumRows())
}

func TestArrowRecordToStreamValues(t *testing.T) {
	pool := NewArrowBuilderPool(0)

	original := map[llotypes.StreamID]StreamValue{
		1: ToDecimal(decimal.NewFromFloat(100.5)),
		2: &Quote{
			Bid:       decimal.NewFromFloat(99),
			Benchmark: decimal.NewFromFloat(100),
			Ask:       decimal.NewFromFloat(101),
		},
	}

	// Convert to Arrow record
	record, err := StreamValuesToArrowRecord(original, pool)
	require.NoError(t, err)
	require.NotNil(t, record)
	defer record.Release()

	// Convert back
	result, err := ArrowRecordToStreamValues(record)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify values match
	assert.Len(t, result, 2)

	dec1, ok := result[1].(*Decimal)
	require.True(t, ok)
	assert.True(t, decimal.NewFromFloat(100.5).Equal(dec1.Decimal()))

	quote2, ok := result[2].(*Quote)
	require.True(t, ok)
	assert.True(t, decimal.NewFromFloat(99).Equal(quote2.Bid))
	assert.True(t, decimal.NewFromFloat(100).Equal(quote2.Benchmark))
	assert.True(t, decimal.NewFromFloat(101).Equal(quote2.Ask))
}

func TestArrowBuilderPool(t *testing.T) {
	pool := NewArrowBuilderPool(1024 * 1024) // 1MB limit

	t.Run("get and put observation builder", func(t *testing.T) {
		builder := pool.GetObservationBuilder()
		require.NotNil(t, builder)

		// Add some data
		builder.Field(ObsColObserverID).(*array.Uint8Builder).Append(1)
		builder.Field(ObsColStreamID).(*array.Uint32Builder).Append(100)

		pool.PutObservationBuilder(builder)
	})

	t.Run("memory stats", func(t *testing.T) {
		allocated, allocs, releases := pool.MemoryStats()
		assert.GreaterOrEqual(t, allocated, int64(0))
		assert.GreaterOrEqual(t, allocs, int64(0))
		assert.GreaterOrEqual(t, releases, int64(0))
	})
}
