package calculated

import (
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDivisionPrecisionIsPinned is the guard on the determinism rule that matters
// most in practice: decimal.DivisionPrecision is a mutable package global, so any
// function relying on it would silently produce different consensus values
// depending on process state.
func TestDivisionPrecisionIsPinned(t *testing.T) {
	// Must NOT call t.Parallel(): it mutates decimal.DivisionPrecision, a
	// package-level global. Go runs non-parallel tests to completion before
	// resuming parallel ones, so as written it cannot overlap with them; adding
	// t.Parallel() here would make every other test in the package read a
	// precision this one set.
	original := decimal.DivisionPrecision
	t.Cleanup(func() { decimal.DivisionPrecision = original })

	type result struct {
		name  string
		value decimal.Decimal
	}
	compute := func(t *testing.T) []result {
		t.Helper()
		// Values chosen so the division does not terminate.
		window := seriesOf(t, "1", "2", "4")

		div, err := Div(1, 3)
		require.NoError(t, err)
		avg, err := Avg(window)
		require.NoError(t, err)
		avgScalars, err := Avg(1, 3)
		require.NoError(t, err)
		median, err := Median(seriesOf(t, "1", "2"))
		require.NoError(t, err)
		variance, err := Variance(window)
		require.NoError(t, err)
		stddev, err := Stddev(window)
		require.NoError(t, err)
		pct, err := PctChange(window)
		require.NoError(t, err)
		sma, err := SMA(window, 3)
		require.NoError(t, err)
		wma, err := WMA(window, 3)
		require.NoError(t, err)
		ema, err := EMA(window, 2)
		require.NoError(t, err)

		return []result{
			{"Div", div}, {"Avg", avg}, {"AvgScalars", avgScalars}, {"Median", median},
			{"Variance", variance}, {"Stddev", stddev}, {"PctChange", pct},
			{"SMA", sma}, {"WMA", wma}, {"EMA", ema},
		}
	}

	decimal.DivisionPrecision = 16
	baseline := compute(t)

	for _, precision := range []int{2, 8, 30, 64} {
		decimal.DivisionPrecision = precision
		got := compute(t)
		require.Len(t, got, len(baseline))
		for i, want := range baseline {
			assert.True(t, want.value.Equal(got[i].value),
				"%s moved when DivisionPrecision=%d: %s -> %s", want.name, precision, want.value, got[i].value)
		}
	}
}

// TestDivAndAvgPinnedToLegacyPrecision documents the pinned value: raising it
// would change the trailing digits of every existing calculated stream.
func TestDivAndAvgPinnedToLegacyPrecision(t *testing.T) {
	t.Parallel()

	got, err := Div(1, 3)
	require.NoError(t, err)
	assertDecimal(t, "0.3333333333333333", got) // 16 decimal places

	got, err = Avg(1, 2)
	require.NoError(t, err)
	assertDecimal(t, "1.5", got)

	// A non-terminating average also truncates at 16.
	got, err = Avg(1, 1, 1, 2)
	require.NoError(t, err)
	assertDecimal(t, "1.25", got)

	got, err = Avg(2, 3, 2)
	require.NoError(t, err)
	assertDecimal(t, "2.3333333333333333", got)
}

// TestTranscendentalsAreConcurrencySafe guards the lock around
// shopspring/decimal's transcendental functions.
//
// ExpTaylor memoizes factorials in an unsynchronized package-level slice it
// appends to, and Ln reads through the same path, so concurrent calls race. An
// append racing with a read can produce a corrupted value rather than a merely
// stale one, which in a consensus path means nodes disagreeing. A node runs
// several plugin instances in one process, each evaluating on its own goroutine,
// so this is reachable in production.
//
// Run under -race, this fails if the guard is removed.
func TestTranscendentalsAreConcurrencySafe(t *testing.T) {
	t.Parallel()

	const goroutines = 24
	window := seriesOf(t, "1.5", "2.5", "3.5", "4.5", "5.5")

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value := decimal.NewFromInt(int64(i + 2))

			got, err := ln(value)
			assert.NoError(t, err)
			_, err = exp(got)
			assert.NoError(t, err)
			_, err = sqrt(value)
			assert.NoError(t, err)

			// The exported functions share the same machinery.
			_, err = Ln(value)
			assert.NoError(t, err)
			_, err = Log(2, value)
			assert.NoError(t, err)
			_, err = Pow(value, 3)
			assert.NoError(t, err)
			_, err = Sqrt(value)
			assert.NoError(t, err)
			_, err = Stddev(window)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
}

func Test_checkPow(t *testing.T) {
	// The bound is on the size of the result, not of the exponent: an exponent
	// well inside maxPowExponent still asks for a result nothing can compute or
	// store.
	for _, tc := range []struct {
		name      string
		base, exp string
		wantErr   string
	}{
		{"large result from a modest exponent", "3000", "50000.5", "exceeds the maximum magnitude of 2400"},
		{"large result from an integer exponent", "2", "5000", "exceeds the maximum magnitude of 2400"},
		{"tiny result", "0.0001", "-40000", "exceeds the maximum magnitude of 2400"},
		{"absurd exponent is refused by the cheap gate", "2", "100001", "exceeds the maximum magnitude of 100000"},
		{"largest storable result", "10", "1000", ""},
		{"base near one with a large exponent", "1.0001", "100000", ""},
		{"square root", "4", "0.5", ""},
		{"zero base has no logarithm", "0", "3", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := decimal.NewFromString(tc.base)
			require.NoError(t, err)
			exponent, err := decimal.NewFromString(tc.exp)
			require.NoError(t, err)

			// checkPow has to be cheap, and so has to reject before the work it
			// is protecting against would have started.
			start := time.Now()
			err = checkPow(base, exponent)
			require.Less(t, time.Since(start), time.Second)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
			// decimalPow reports the same refusal rather than computing.
			_, err = decimalPow(base, exponent, doublePrecision)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
