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

	t.Run("fails with fewer than f+1 values", func(t *testing.T) {
		_, err := MedianAggregator(values[:2], 3)
		assert.EqualError(t, err, "not enough observations to calculate median, expected at least f+1, got 2")
	})

	t.Run("fails with unsupported StreamValue type", func(t *testing.T) {
		_, err := MedianAggregator([]StreamValue{&Quote{}, &Quote{}, &Quote{}}, 1)
		assert.EqualError(t, err, "not enough observations to calculate median, expected at least f+1, got 0")
	})
}
func Test_ModeAggregator(t *testing.T) {
	t.Run("not implemented, returns error", func(t *testing.T) {
		_, err := ModeAggregator(nil, 1)
		assert.EqualError(t, err, "not implemented")
	})
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
