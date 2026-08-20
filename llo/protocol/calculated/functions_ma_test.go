package calculated

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSMA(t *testing.T) {
	t.Parallel()

	window := seriesOf(t, "10", "20", "30", "40")

	// Newest 2: (30 + 40) / 2.
	got, err := SMA(window, 2)
	require.NoError(t, err)
	assertDecimal(t, "35", got)

	// The whole window matches Avg.
	got, err = SMA(window, 4)
	require.NoError(t, err)
	assertDecimal(t, "25", got)

	got, err = SMA(window, 1)
	require.NoError(t, err)
	assertDecimal(t, "40", got)
}

func TestWMA(t *testing.T) {
	t.Parallel()

	window := seriesOf(t, "10", "20", "30", "40")

	// Newest 3 with weights 1,2,3 oldest-to-newest: (1*20 + 2*30 + 3*40) / 6.
	// New functions round to precision (18), unlike Div and Avg which stay
	// pinned at 16 for backwards compatibility.
	got, err := WMA(window, 3)
	require.NoError(t, err)
	assertDecimal(t, "33.333333333333333333", got)

	// n=1 is just the newest value.
	got, err = WMA(window, 1)
	require.NoError(t, err)
	assertDecimal(t, "40", got)

	// A flat window averages to its value whatever the weights.
	got, err = WMA(seriesOf(t, "7", "7", "7"), 3)
	require.NoError(t, err)
	assertDecimal(t, "7", got)

	// Weighting is newest-heaviest, so a rising series exceeds the simple mean.
	sma, err := SMA(window, 4)
	require.NoError(t, err)
	wma, err := WMA(window, 4)
	require.NoError(t, err)
	assert.True(t, wma.GreaterThan(sma), "WMA %s should exceed SMA %s on a rising series", wma, sma)
}

func TestEMA(t *testing.T) {
	t.Parallel()

	// Seed is the mean of the oldest n; with n == len there is nothing left to
	// iterate, so EMA equals that mean.
	got, err := EMA(seriesOf(t, "10", "20", "30", "40"), 4)
	require.NoError(t, err)
	assertDecimal(t, "25", got)

	// A flat series is unchanged by any amount of smoothing.
	got, err = EMA(seriesOf(t, "5", "5", "5", "5", "5"), 2)
	require.NoError(t, err)
	assertDecimal(t, "5", got)

	// Hand-computed: n=2 over [10,20,30].
	//   seed  = (10+20)/2 = 15
	//   alpha = 2/3
	//   ema   = 30*(2/3) + 15*(1/3) = 20 + 5 = 25
	got, err = EMA(seriesOf(t, "10", "20", "30"), 2)
	require.NoError(t, err)
	assertDecimal(t, "25", got)

	// Weighting the newest values more heavily puts EMA above the simple mean on
	// a rising series.
	window := seriesOf(t, "10", "20", "30", "40", "50")
	ema, err := EMA(window, 2)
	require.NoError(t, err)
	sma, err := SMA(window, 5)
	require.NoError(t, err)
	assert.True(t, ema.GreaterThan(sma), "EMA %s should exceed SMA %s on a rising series", ema, sma)
}

// TestEMA_Deterministic covers the property that makes a path-dependent
// recurrence safe in a consensus path: identical inputs must give an identical
// result every time, and the rounding must not drift with repetition.
func TestEMA_Deterministic(t *testing.T) {
	t.Parallel()

	window := seriesOf(t, "1.1", "2.7", "3.14159", "4.000001", "5.5", "6.25", "7.77")
	first, err := EMA(window, 3)
	require.NoError(t, err)
	for range 50 {
		again, err := EMA(window, 3)
		require.NoError(t, err)
		require.True(t, first.Equal(again), "EMA drifted: %s vs %s", first, again)
	}
	// Every step rounds to the package precision, so the result cannot carry
	// more than that.
	assert.LessOrEqual(t, -first.Exponent(), int32(precision))
}

func TestMovingAverages_Errors(t *testing.T) {
	t.Parallel()

	for name, fn := range map[string]func(any, any) (decimal.Decimal, error){
		"SMA": SMA, "WMA": WMA, "EMA": EMA,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			window := seriesOf(t, "1", "2", "3")

			_, err := fn(decimal.NewFromInt(1), 2)
			require.ErrorContains(t, err, "expects a history window")

			_, err = fn(Series{}, 2)
			require.ErrorContains(t, err, "empty")

			// Silently using fewer samples than asked for would change the
			// meaning of the result.
			_, err = fn(window, 4)
			require.ErrorContains(t, err, "exceeds the window length")

			_, err = fn(window, 0)
			require.ErrorContains(t, err, "at least 1")

			_, err = fn(window, -1)
			require.ErrorContains(t, err, "at least 1")

			_, err = fn(window, 1.5)
			require.ErrorContains(t, err, "whole number")
		})
	}
}
