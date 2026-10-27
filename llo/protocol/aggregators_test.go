package protocol

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
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

	// Contribution floor, expressed directly: v3.0 passes F+1, v3.1 passes
	// 2*AggregationFaultTolerance+1.
	minContributions := 2

	t.Run("returns median with even number of values", func(t *testing.T) {
		sv, err := MedianAggregator(values, minContributions)
		require.NoError(t, err)
		assert.IsType(t, &Decimal{}, sv)
		assert.Equal(t, "4.4", sv.(*Decimal).String())
	})

	t.Run("returns higher value with odd number of values", func(t *testing.T) {
		sv, err := MedianAggregator(values[:5], minContributions)
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

		sv, err := MedianAggregator(mixedValues, minContributions)
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

		sv, err := MedianAggregator(mixedValues, minContributions)
		require.NoError(t, err)
		assert.IsType(t, &TimestampedStreamValue{}, sv)
		assert.Equal(t, "5.6", sv.(*TimestampedStreamValue).StreamValue.(*Decimal).String())
		assert.Equal(t, uint64(102), sv.(*TimestampedStreamValue).ObservedAtNanoseconds)
	})

	t.Run("for *TimestampedStreamValue with skipped inner values", func(t *testing.T) {
		quote := &Quote{Bid: decimal.NewFromInt(1), Benchmark: decimal.NewFromInt(2), Ask: decimal.NewFromInt(3)}
		mixedValues := []StreamValue{
			&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.NewFromFloat(2.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 102, StreamValue: ToDecimal(decimal.NewFromFloat(4.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 104, StreamValue: ToDecimal(decimal.NewFromFloat(6.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 999, StreamValue: quote},
			&TimestampedStreamValue{ObservedAtNanoseconds: 999, StreamValue: quote},
			&TimestampedStreamValue{ObservedAtNanoseconds: 999, StreamValue: quote},
			&TimestampedStreamValue{ObservedAtNanoseconds: 999, StreamValue: quote},
		}

		t.Run("v3.0 counts a zero timestamp for each skipped value", func(t *testing.T) {
			sv, err := MedianAggregator(mixedValues, 3)
			require.NoError(t, err)
			assert.Equal(t, "4.6", sv.(*TimestampedStreamValue).StreamValue.(*Decimal).String())
			assert.Equal(t, uint64(0), sv.(*TimestampedStreamValue).ObservedAtNanoseconds)
		})

		t.Run("v3.1 takes the median timestamp over used values only", func(t *testing.T) {
			sv, err := MedianAggregatorV31(mixedValues, 3)
			require.NoError(t, err)
			assert.Equal(t, "4.6", sv.(*TimestampedStreamValue).StreamValue.(*Decimal).String())
			assert.Equal(t, uint64(102), sv.(*TimestampedStreamValue).ObservedAtNanoseconds)
		})

		t.Run("v3.1 still enforces the floor on used values", func(t *testing.T) {
			_, err := MedianAggregatorV31(mixedValues, 4)
			require.EqualError(t, err, "not enough contributions to calculate median: got 3, need 4")
		})
	})

	t.Run("for *TimestampedStreamValue skips nil values", func(t *testing.T) {
		nilValues := []StreamValue{
			&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.NewFromFloat(2.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 102, StreamValue: ToDecimal(decimal.NewFromFloat(4.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 104, StreamValue: ToDecimal(decimal.NewFromFloat(6.6))},
			&TimestampedStreamValue{ObservedAtNanoseconds: 999},
			(*TimestampedStreamValue)(nil),
		}

		sv, err := MedianAggregatorV31(nilValues, 3)
		require.NoError(t, err)
		assert.Equal(t, "4.6", sv.(*TimestampedStreamValue).StreamValue.(*Decimal).String())
		assert.Equal(t, uint64(102), sv.(*TimestampedStreamValue).ObservedAtNanoseconds)
	})

	t.Run("fails below the contribution floor", func(t *testing.T) {
		_, err := MedianAggregator(values[:2], 4)
		require.EqualError(t, err, "not enough contributions to calculate median: got 2, need 4")
	})

	t.Run("fails with unsupported StreamValue type", func(t *testing.T) {
		_, err := MedianAggregator([]StreamValue{nil, nil, nil}, 2)
		require.EqualError(t, err, "not enough contributions to calculate median: got 0, need 2")
	})
}

func Test_ModeAggregator(t *testing.T) {
	tcs := []struct {
		name             string
		values           []StreamValue
		minContributions int
		output           StreamValue
		errStr           string
	}{
		{
			name: "returns mode value with 3f+1 values in agreement",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
			},
			minContributions: 2,
			output:           ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name: "returns mode value with 3f values in agreement",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(2.2)),
			},
			minContributions: 2,
			output:           ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name: "returns mode value using tie-breaker with split agreement",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(2.2)),
				ToDecimal(decimal.NewFromFloat(2.2)),
			},
			minContributions: 2,
			output:           ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name:             "returns error if not enough observations",
			values:           []StreamValue{},
			minContributions: 2,
			errStr:           "not enough contributions in agreement to calculate mode: most common value had 0, need 2",
		},
		{
			name: "returns error if fewer than the floor agree",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.2)),
				ToDecimal(decimal.NewFromFloat(2.2)),
				ToDecimal(decimal.NewFromFloat(3.2)),
			},
			minContributions: 2,
			errStr:           "not enough contributions in agreement to calculate mode: most common value had 1, need 2",
		},
		{
			name: "handles mixed types, tie-breaking on first type",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				ToDecimal(decimal.NewFromFloat(1.1)),
				&Quote{Benchmark: decimal.NewFromFloat(1.2)},
				&Quote{Benchmark: decimal.NewFromFloat(1.2)},
			},
			minContributions: 2,
			output:           ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name: "handles mixed types where Quote is most common",
			values: []StreamValue{
				ToDecimal(decimal.NewFromFloat(1.1)),
				&Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
				&Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
				&Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
			},
			minContributions: 2,
			output:           &Quote{Bid: decimal.NewFromFloat(1.2), Benchmark: decimal.NewFromFloat(2.2), Ask: decimal.NewFromFloat(3.2)},
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
			minContributions: 3,
			output:           ToDecimal(decimal.NewFromFloat(1.1)),
		},
		{
			name: "decimals differing only in scale agree",
			values: []StreamValue{
				ToDecimal(decimal.New(1, 0)),
				ToDecimal(decimal.New(10, -1)),
				ToDecimal(decimal.New(100, -2)),
				ToDecimal(decimal.New(2, 0)),
			},
			minContributions: 3,
			output:           ToDecimal(decimal.New(1, 0)),
		},
		{
			name: "integer decimals differing only in scale agree",
			values: []StreamValue{
				ToDecimal(decimal.New(100, 0)),
				ToDecimal(decimal.New(1, 2)),
				ToDecimal(decimal.New(10, 1)),
			},
			minContributions: 3,
			output:           ToDecimal(decimal.New(1, 2)),
		},
		{
			name: "zeros differing only in scale agree",
			values: []StreamValue{
				ToDecimal(decimal.New(0, 0)),
				ToDecimal(decimal.New(0, -5)),
				ToDecimal(decimal.New(0, 3)),
			},
			minContributions: 3,
			output:           ToDecimal(decimal.New(0, 0)),
		},
		{
			name: "quotes differing only in scale agree",
			values: []StreamValue{
				&Quote{Bid: decimal.New(9, 0), Benchmark: decimal.New(10, 0), Ask: decimal.New(11, 0)},
				&Quote{Bid: decimal.New(90, -1), Benchmark: decimal.New(1000, -2), Ask: decimal.New(110, -1)},
				&Quote{Bid: decimal.New(900, -2), Benchmark: decimal.New(1, 1), Ask: decimal.New(11000, -3)},
			},
			minContributions: 3,
			output:           &Quote{Bid: decimal.New(9, 0), Benchmark: decimal.New(1, 1), Ask: decimal.New(11, 0)},
		},
		{
			name: "timestamped values agree on inner value and take the median timestamp",
			values: []StreamValue{
				&TimestampedStreamValue{ObservedAtNanoseconds: 300, StreamValue: ToDecimal(decimal.New(15, -1))},
				&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.New(150, -2))},
				&TimestampedStreamValue{ObservedAtNanoseconds: 200, StreamValue: ToDecimal(decimal.New(15, -1))},
				&TimestampedStreamValue{ObservedAtNanoseconds: 1, StreamValue: ToDecimal(decimal.New(2, 0))},
			},
			minContributions: 3,
			output:           &TimestampedStreamValue{ObservedAtNanoseconds: 200, StreamValue: ToDecimal(decimal.New(15, -1))},
		},
		{
			name: "timestamped values that disagree on inner value fail the floor",
			values: []StreamValue{
				&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.New(1, 0))},
				&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.New(2, 0))},
				&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.New(3, 0))},
			},
			minContributions: 2,
			errStr:           "not enough contributions in agreement to calculate mode: most common value had 1, need 2",
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			sv, err := ModeAggregator(tc.values, tc.minContributions)
			if tc.errStr == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.output, sv)
			} else {
				require.EqualError(t, err, tc.errStr)
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

		sv, err := QuoteAggregator(values, 2)
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
		sv, err := QuoteAggregator(values, 2)
		require.NoError(t, err)
		assert.IsType(t, &Quote{}, sv)
		q := sv.(*Quote)
		assert.Equal(t, "4.4", q.Bid.String())
		assert.Equal(t, "5.5", q.Benchmark.String())
		assert.Equal(t, "6.6", q.Ask.String())
	})

	t.Run("fails below the contribution floor", func(t *testing.T) {
		_, err := QuoteAggregator([]StreamValue{&Quote{}, &Quote{}}, 3)
		require.EqualError(t, err, "not enough valid contributions to aggregate quote: got 2, need 3")
	})

	t.Run("ignores non-Quote type", func(t *testing.T) {
		values := []StreamValue{
			&Quote{Bid: (decimal.NewFromFloat(1.1)), Benchmark: (decimal.NewFromFloat(2.2)), Ask: (decimal.NewFromFloat(3.3))},
			&Quote{Bid: (decimal.NewFromFloat(4.4)), Benchmark: (decimal.NewFromFloat(5.5)), Ask: (decimal.NewFromFloat(6.6))},
			ToDecimal(decimal.NewFromFloat(7.7)),
			ToDecimal(decimal.NewFromFloat(8.8)),
		}
		sv, err := QuoteAggregator(values, 2)
		require.NoError(t, err)
		assert.IsType(t, &Quote{}, sv)
		q := sv.(*Quote)
		assert.Equal(t, "4.4", q.Bid.String())
		assert.Equal(t, "5.5", q.Benchmark.String())
		assert.Equal(t, "6.6", q.Ask.String())
	})
}

func Test_canonicalDecimal(t *testing.T) {
	t.Run("strips trailing zeros", func(t *testing.T) {
		d := canonicalDecimal(decimal.New(-12300, -4))
		assert.Equal(t, "-123", d.Coefficient().String())
		assert.Equal(t, int32(-2), d.Exponent())
	})
	t.Run("stops at MaxDecimalExponent", func(t *testing.T) {
		a := canonicalDecimal(decimal.New(100, MaxDecimalExponent-1))
		b := canonicalDecimal(decimal.New(1000, MaxDecimalExponent-2))
		assert.Equal(t, "10", a.Coefficient().String())
		assert.Equal(t, int32(MaxDecimalExponent), a.Exponent())
		assert.Equal(t, a, b)
		require.NoError(t, checkDecimalExponent(a))
	})
}

func Test_GetAggregatorFuncV31(t *testing.T) {
	values := []StreamValue{
		&TimestampedStreamValue{ObservedAtNanoseconds: 100, StreamValue: ToDecimal(decimal.NewFromInt(1))},
		&TimestampedStreamValue{ObservedAtNanoseconds: 999, StreamValue: &Quote{}},
		&TimestampedStreamValue{ObservedAtNanoseconds: 999, StreamValue: &Quote{}},
	}
	sv, err := GetAggregatorFuncV31(llotypes.AggregatorMedian)(values, 1)
	require.NoError(t, err)
	assert.Equal(t, uint64(100), sv.(*TimestampedStreamValue).ObservedAtNanoseconds)

	for _, a := range []llotypes.Aggregator{llotypes.AggregatorMode, llotypes.AggregatorQuote, llotypes.AggregatorCalculated} {
		assert.Equal(t, GetAggregatorFunc(a) == nil, GetAggregatorFuncV31(a) == nil, "aggregator %v", a)
	}
}
