package llo

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_MedianAggregator(t *testing.T) {
	values := []StreamValue{
		ToDecimal(decimal.NewFromFloat(1.1)),
		ToDecimal(decimal.NewFromFloat(4.4)),
		ToDecimal(decimal.NewFromFloat(2.2)),
		ToDecimal(decimal.NewFromFloat(3.3)),
		ToDecimal(decimal.NewFromFloat(6.6)),
		ToDecimal(decimal.NewFromFloat(5.5)),
	}

	f := 1

	t.Run("returns median with even number of values", func(t *testing.T) {
		sv, err := MedianAggregator(values, f)
		require.NoError(t, err)
		assert.IsType(t, &Decimal{}, sv)
		assert.Equal(t, "4.4", sv.(*Decimal).String())
	})

	t.Run("returns higher value with odd number of values", func(t *testing.T) {
		sv, err := MedianAggregator(values[:5], f)
		require.NoError(t, err)
		assert.IsType(t, &Decimal{}, sv)
		assert.Equal(t, "3.3", sv.(*Decimal).String())
	})

	t.Run("for stream values of type *Quote, uses the Benchmark value", func(t *testing.T) {
		mixedValues := []StreamValue{
			&Quote{Benchmark: decimal.NewFromFloat(1.1)},
			&Quote{Benchmark: decimal.NewFromFloat(4.4)},
			&Quote{Benchmark: decimal.NewFromFloat(2.2)},
			&Quote{Benchmark: decimal.NewFromFloat(3.3)},
			ToDecimal(decimal.NewFromFloat(6.6)),
			ToDecimal(decimal.NewFromFloat(5.5)),
		}

		sv, err := MedianAggregator(mixedValues, f)
		require.NoError(t, err)
		assert.IsType(t, &Decimal{}, sv)
		assert.Equal(t, "4.4", sv.(*Decimal).String())
	})

	t.Run("for stream values of type *TimestampedStreamValue, applies median to the StreamValue values and timestamps separately", func(t *testing.T) {
		// TODO: This is a simplistic approach but has problems; we may need to revisit
		mixedValues := []StreamValue{
			&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.NewFromFloat(2.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 102, StreamValue: ToDecimal(decimal.NewFromFloat(10.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 104, StreamValue: ToDecimal(decimal.NewFromFloat(4.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 95, StreamValue: ToDecimal(decimal.NewFromFloat(6.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 106, StreamValue: ToDecimal(decimal.NewFromFloat(5.6))},
		}

		sv, err := MedianAggregator(mixedValues, f)
		require.NoError(t, err)
		assert.IsType(t, &TimestampedStreamValue{}, sv)
		assert.Equal(t, "5.6", sv.(*TimestampedStreamValue).StreamValue.(*Decimal).String())
		assert.Equal(t, uint64(102), sv.(*TimestampedStreamValue).ObservedAtNanoseconds)
	})

	t.Run("fails with fewer than f+1 values", func(t *testing.T) {
		_, err := MedianAggregator(values[:2], 3)
		assert.EqualError(t, err, "not enough observations to calculate median, expected at least f+1, got 2")
	})

	t.Run("fails with unsupported StreamValue type", func(t *testing.T) {
		_, err := MedianAggregator([]StreamValue{nil, nil, nil}, 1)
		assert.EqualError(t, err, "not enough observations to calculate median, expected at least f+1, got 0")
	})
}

func Test_ModeAggregator(t *testing.T) {
	tcs := []struct {
		name   string
		values []StreamValue
		f      int
		output StreamValue
		errStr string
	}{
		{
			name: "returns mode value with 3f+1 values in agreement",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
			},
			f:      1,
			output: ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name: "returns mode value with 3f values in agreement",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(2.2)),
			},
			f:      1,
			output: ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name: "returns mode value using tie-breaker with split agreement",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(2.2)),
				ToDecimal(decimal.NewFromFloat(2.2)),
			},
			f:      1,
			output: ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name:   "returns error if not enough observations",
			values: []StreamValue{},
			f:      1,
			errStr: "not enough observations in agreement to calculate mode, expected at least f+1, most common value had 0",
		},
		{
			name: "returns error if less than f in agreement",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.2)),
				ToDecimal(decimal.NewFromFloat(2.2)),
				ToDecimal(decimal.NewFromFloat(3.2)),
			},
			f:      1,
			errStr: "not enough observations in agreement to calculate mode, expected at least f+1, most common value had 1",
		},
		{
			name: "handles mixed types, tie-breaking on first type",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				&Quote{Benchmark: decimal.NewFromFloat(1.2)},
				&Quote{Benchmark: decimal.NewFromFloat(1.2)},
			},
			f:      1,
			output: ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name: "handles mixed types where Quote is most common",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				&Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
				&Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
				&Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
			},
			f:      1,
			output: &Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
		},
		{
			name: "nils are not counted",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				nil,
				nil,
				nil,
				nil,
			},
			f:      2,
			output: ToDecimal(decimal.NewFromFloat(1.1)),
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			sv, err := ModeAggregator(tc.values, tc.f)
			if tc.errStr == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.output, sv)
			} else {
				assert.EqualError(t, err, tc.errStr)
			}
		})
	}
}

func Test_QuoteAggregator(t *testing.T) {
	t.Run("returns median values for bid, benchmark and ask", func(t *testing.T) {
		values := []StreamValue{
			&Quote{Bid: (decimal.NewFromFloat(9.99)), Benchmark: (decimal.NewFromFloat(10.0)), Ask: (decimal.NewFromFloat(10.14))},
			&Quote{Bid: (decimal.NewFromFloat(9.88)), Benchmark: (decimal.NewFromFloat(10.12)), Ask: (decimal.NewFromFloat(10.13))},
			&Quote{Bid: (decimal.NewFromFloat(1.1)), Benchmark: (decimal.NewFromFloat(9.98)), Ask: (decimal.NewFromFloat(10))},
			&Quote{Bid: (decimal.NewFromFloat(10.01)), Benchmark: (decimal.NewFromFloat(10.03)), Ask: (decimal.NewFromFloat(10.10))},
		}

		sv, err := QuoteAggregator(values, 1)
		require.NoError(t, err)
		assert.IsType(t, &Quote{}, sv)
		q := sv.(*Quote)
		assert.Equal(t, "9.99", q.Bid.String())
		assert.Equal(t, "10.03", q.Benchmark.String())
		assert.Equal(t, "10.13", q.Ask.String())
	})

	t.Run("ignores invalid (invariant violation) quote values", func(t *testing.T) {
		values := []StreamValue{
			&Quote{Bid: (decimal.NewFromFloat(1.1)), Benchmark: (decimal.NewFromFloat(2.2)), Ask: (decimal.NewFromFloat(3.3))},
			&Quote{Bid: (decimal.NewFromFloat(4.4)), Benchmark: (decimal.NewFromFloat(5.5)), Ask: (decimal.NewFromFloat(6.6))},
			&Quote{Bid: (decimal.NewFromFloat(7.7)), Benchmark: (decimal.NewFromFloat(8.8)), Ask: (decimal.NewFromFloat(8.7))},       // invalid
			&Quote{Bid: (decimal.NewFromFloat(12.12)), Benchmark: (decimal.NewFromFloat(11.11)), Ask: (decimal.NewFromFloat(12.12))}, // invalid
		}
		sv, err := QuoteAggregator(values, 1)
		require.NoError(t, err)
		assert.IsType(t, &Quote{}, sv)
		q := sv.(*Quote)
		assert.Equal(t, "4.4", q.Bid.String())
		assert.Equal(t, "5.5", q.Benchmark.String())
		assert.Equal(t, "6.6", q.Ask.String())
	})

	t.Run("fails with fewer than f+1 values", func(t *testing.T) {
		_, err := QuoteAggregator([]StreamValue{&Quote{}, &Quote{}}, 2)
		assert.EqualError(t, err, "not enough valid observations to aggregate quote, expected at least f+1, got 2")
	})

	t.Run("ignores non-Quote type", func(t *testing.T) {
		values := []StreamValue{
			&Quote{Bid: (decimal.NewFromFloat(1.1)), Benchmark: (decimal.NewFromFloat(2.2)), Ask: (decimal.NewFromFloat(3.3))},
			&Quote{Bid: (decimal.NewFromFloat(4.4)), Benchmark: (decimal.NewFromFloat(5.5)), Ask: (decimal.NewFromFloat(6.6))},
			ToDecimal(decimal.NewFromFloat(7.7)),
			ToDecimal(decimal.NewFromFloat(8.8)),
		}
		sv, err := QuoteAggregator(values, 1)
		require.NoError(t, err)
		assert.IsType(t, &Quote{}, sv)
		q := sv.(*Quote)
		assert.Equal(t, "4.4", q.Bid.String())
		assert.Equal(t, "5.5", q.Benchmark.String())
		assert.Equal(t, "6.6", q.Ask.String())
	})
}
