package calculated

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// twapWindowStartSeconds is the arbitrary window start the ported cases use.
const twapWindowStartSeconds = 100

// twapSeries builds a window from one price per bucket, 0 meaning "no observation
// in that second". Buckets are one second apart starting at twapWindowStartSeconds.
//
// This mirrors the mercury calculator tests, where a report marks exactly one
// bucket observed.
func twapSeries(t *testing.T, prices []int64) Series {
	t.Helper()
	values := make([]decimal.Decimal, 0, len(prices))
	timestamps := make([]uint64, 0, len(prices))
	for i, price := range prices {
		if price == 0 {
			continue // missing observation
		}
		values = append(values, decimal.NewFromInt(price))
		timestamps = append(timestamps, uint64(twapWindowStartSeconds+i)*uint64(time.Second))
	}
	s, err := NewSeries(values, timestamps)
	require.NoError(t, err)
	return s
}

// twapConfigMap is the configuration as an expression would supply it.
func twapConfigMap(windowSeconds, minSamples, maxHeadGap, maxInteriorGap, maxTailGap int) map[string]any {
	return map[string]any{
		"window":         time.Duration(windowSeconds) * time.Second,
		"minSamples":     minSamples,
		"maxHeadGap":     maxHeadGap,
		"maxInteriorGap": maxInteriorGap,
		"maxTailGap":     maxTailGap,
	}
}

// assertClose compares against a hand-computed value with a tolerance.
//
// Exactness is not available here and that is inherent to the algorithm, not a
// shortcut: the specification fills gaps in log-price space, so every bucket goes
// through exp(ln(price)). At any finite precision that round trip is not the
// identity, so a whole-number expectation cannot be matched bit-for-bit. The
// tolerance is many orders of magnitude tighter than any reporting precision.
func assertClose(t *testing.T, want string, got decimal.Decimal) {
	t.Helper()
	expected, err := decimal.NewFromString(want)
	require.NoError(t, err)

	const tolerance = "0.000000000001" // 1e-12
	limit, err := decimal.NewFromString(tolerance)
	require.NoError(t, err)

	diff := got.Sub(expected).Abs()
	assert.True(t, diff.LessThanOrEqual(limit),
		"expected %s (±%s), got %s (off by %s)", want, tolerance, got, diff)
}

// TestTWAP_FillThenAverage is the mercury TestCalculate_FillThenAverage suite,
// ported case for case. The expected values are the same, which is the point: the
// port changed the arithmetic from float64 to decimal, not the semantics.
func TestTWAP_FillThenAverage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		prices      []int64 // one entry per bucket; 0 = missing
		minSamples  int
		maxHead     int
		maxInterior int
		maxTail     int
		want        string
		wantReasons []TWAPRejectionReason
	}{
		{
			name:       "no gaps: plain average over the full window",
			prices:     []int64{100, 200, 300},
			minSamples: 3,
			want:       "200",
		},
		{
			name:       "interior gap at the threshold: log-linear interpolated",
			prices:     []int64{100, 0, 0, 0, 1600}, // Gint=3
			minSamples: 2, maxInterior: 3,
			// Interpolating in log space doubles each step: 100,200,400,800,1600.
			want: "620",
		},
		{
			name:       "interior gap one over the threshold: rejected",
			prices:     []int64{100, 0, 0, 0, 1600},
			minSamples: 2, maxInterior: 2,
			wantReasons: []TWAPRejectionReason{ReasonInteriorGapTooLong},
		},
		{
			name:       "tail gap at the threshold: carried forward from the last observed price",
			prices:     []int64{100, 200, 300, 0, 0}, // Gtail=2
			minSamples: 3, maxTail: 2,
			want: "240",
		},
		{
			name:       "tail gap one over the threshold: rejected",
			prices:     []int64{100, 200, 300, 0, 0},
			minSamples: 3, maxTail: 1,
			wantReasons: []TWAPRejectionReason{ReasonTailGapTooLong},
		},
		{
			name:       "insufficient samples: rejected even though every gap is within its threshold",
			prices:     []int64{100, 0, 300, 0, 500}, // M=3, Gint=1
			minSamples: 4, maxInterior: 5, maxTail: 5,
			wantReasons: []TWAPRejectionReason{ReasonInsufficientSamples},
		},
		{
			name:       "head gap at the threshold: backfilled from the first observed price",
			prices:     []int64{0, 0, 200, 300, 400}, // Ghead=2
			minSamples: 3, maxHead: 2,
			want: "260",
		},
		{
			name:       "head gap one over the threshold: rejected",
			prices:     []int64{0, 0, 200, 300, 400},
			minSamples: 3, maxHead: 1,
			wantReasons: []TWAPRejectionReason{ReasonHeadGapTooLong},
		},
		{
			// Gint is the both-sides-anchored statistic, so a head run must not
			// count toward it. Ghead=2 while maxInterior is 1: if the head run
			// leaked into Gint this would reject instead of producing a value.
			name:       "head run is not counted toward the interior-gap threshold",
			prices:     []int64{0, 0, 100, 0, 400}, // Ghead=2, Gint=1
			minSamples: 2, maxHead: 2, maxInterior: 1,
			want: "180",
		},
		{
			name:       "head and tail gap in the same window, both at their thresholds",
			prices:     []int64{0, 0, 100, 100, 0, 0}, // Ghead=2, Gtail=2
			minSamples: 2, maxHead: 2, maxTail: 2,
			want: "100",
		},
		{
			name:       "multiple applicable reasons are all returned, not just the first",
			prices:     []int64{100, 0, 0, 0, 0}, // M=1, Gtail=4
			minSamples: 3, maxInterior: 5, maxTail: 2,
			wantReasons: []TWAPRejectionReason{ReasonInsufficientSamples, ReasonTailGapTooLong},
		},
		{
			name:       "head and tail reasons are reported alongside insufficient samples",
			prices:     []int64{0, 0, 0, 100, 0}, // M=1, Ghead=3, Gtail=1
			minSamples: 3, maxHead: 2,
			wantReasons: []TWAPRejectionReason{ReasonInsufficientSamples, ReasonHeadGapTooLong, ReasonTailGapTooLong},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			windowSeconds := len(tc.prices)
			series := twapSeries(t, tc.prices)
			cfg := twapConfigMap(windowSeconds, tc.minSamples, tc.maxHead, tc.maxInterior, tc.maxTail)

			// The anchor is the exclusive end of the window.
			anchorNs := uint64(twapWindowStartSeconds+windowSeconds) * uint64(time.Second)
			got, err := twapFunc(anchorNs)(series, cfg)

			if tc.wantReasons != nil {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrTWAPRejected)
				var rejection *TWAPRejection
				require.ErrorAs(t, err, &rejection)
				assert.Equal(t, tc.wantReasons, rejection.Reasons)
				return
			}
			require.NoError(t, err)
			assertClose(t, tc.want, got)
		})
	}
}

// TestTWAP_GapStats ports the mercury gap-classification cases. These are the
// statistics the acceptance rule is written in terms of, so they are worth
// pinning independently of the averaging.
func TestTWAP_GapStats(t *testing.T) {
	t.Parallel()

	// o marks an observed bucket, x a missing one.
	const o, x = true, false

	for _, tc := range []struct {
		name                               string
		observed                           []bool
		wantM, wantHead, wantInt, wantTail int
	}{
		{"all observed", []bool{o, o, o}, 3, 0, 0, 0},
		{"head gap only", []bool{x, x, o, o, o}, 3, 2, 0, 0},
		{"tail gap only", []bool{o, o, o, x, x}, 3, 0, 0, 2},
		{"single interior gap", []bool{o, x, o}, 2, 0, 1, 0},
		{"head and tail gaps", []bool{x, x, o, x, x}, 1, 2, 0, 2},
		{"head and interior gaps", []bool{x, o, x, x, o}, 2, 1, 2, 0},
		{"interior and tail gaps", []bool{o, x, x, x, x}, 1, 0, 0, 4},
		// No anchors at all, so no run is classified; the M check rejects it.
		{"no observations", []bool{x, x, x, x, x}, 0, 0, 0, 0},
		{"single observed bucket", []bool{o}, 1, 0, 0, 0},
		{"single missing bucket", []bool{x}, 0, 0, 0, 0},
		{"alternating", []bool{o, x, o, x, o}, 3, 0, 1, 0},
		{"two interior gaps takes the longest", []bool{o, x, o, x, x, o}, 3, 0, 2, 0},
		{"three interior gaps increasing", []bool{o, x, o, x, x, o, x, x, x, o}, 4, 0, 3, 0},
		{"gaps not in order", []bool{o, x, x, x, o, x, o, x, x, o}, 4, 0, 3, 0},
		{"long head gap", []bool{x, x, x, x, x, o, o}, 2, 5, 0, 0},
		{"long tail gap", []bool{o, o, x, x, x, x, x}, 2, 0, 0, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buckets := make([]twapBucket, len(tc.observed))
			for i, observed := range tc.observed {
				buckets[i] = twapBucket{observed: observed, price: decimal.NewFromInt(1)}
			}
			m, head, interior, tail := twapGapStats(buckets)
			assert.Equal(t, tc.wantM, m, "M")
			assert.Equal(t, tc.wantHead, head, "Ghead")
			assert.Equal(t, tc.wantInt, interior, "Gint")
			assert.Equal(t, tc.wantTail, tail, "Gtail")
		})
	}
}

// TestTWAP_HalfOpenWindow covers ADR 0013: the anchor second belongs to the next
// window, and observations before the window start are dropped.
func TestTWAP_HalfOpenWindow(t *testing.T) {
	t.Parallel()

	const windowSeconds = 3
	anchorSeconds := uint64(twapWindowStartSeconds + windowSeconds)
	cfg := twapConfigMap(windowSeconds, 1, windowSeconds, windowSeconds, windowSeconds)

	// An observation exactly at the anchor is excluded, leaving the window empty.
	series, err := NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(100)},
		[]uint64{anchorSeconds * uint64(time.Second)},
	)
	require.NoError(t, err)
	_, err = twapFunc(anchorSeconds*uint64(time.Second))(series, cfg)
	require.ErrorIs(t, err, ErrTWAPRejected, "the anchor second belongs to the next window")

	// One second earlier is inside the window.
	series, err = NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(100)},
		[]uint64{(anchorSeconds - 1) * uint64(time.Second)},
	)
	require.NoError(t, err)
	got, err := twapFunc(anchorSeconds*uint64(time.Second))(series, cfg)
	require.NoError(t, err)
	assertClose(t, "100", got)

	// An observation before the window start is dropped, so only the in-window
	// one counts.
	series, err = NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(999), decimal.NewFromInt(100)},
		[]uint64{(twapWindowStartSeconds - 5) * uint64(time.Second), (anchorSeconds - 1) * uint64(time.Second)},
	)
	require.NoError(t, err)
	got, err = twapFunc(anchorSeconds*uint64(time.Second))(series, cfg)
	require.NoError(t, err)
	assertClose(t, "100", got)
}

// TestTWAP_Deterministic is the property that makes TWAP usable in a consensus
// path: identical inputs give an identical result, with no float64 anywhere.
func TestTWAP_Deterministic(t *testing.T) {
	t.Parallel()

	prices := []int64{1234, 0, 1240, 1250, 0, 0, 1300, 1310}
	series := twapSeries(t, prices)
	cfg := twapConfigMap(len(prices), 3, 2, 2, 2)
	anchorNs := uint64(twapWindowStartSeconds+len(prices)) * uint64(time.Second)

	first, err := twapFunc(anchorNs)(series, cfg)
	require.NoError(t, err)
	for range 30 {
		again, err := twapFunc(anchorNs)(series, cfg)
		require.NoError(t, err)
		require.True(t, first.Equal(again), "TWAP drifted: %s vs %s", first, again)
	}
}

func TestTWAP_ConfigValidation(t *testing.T) {
	t.Parallel()

	series := twapSeries(t, []int64{100, 200, 300})
	anchorNs := uint64(twapWindowStartSeconds+3) * uint64(time.Second)
	call := func(cfg any) error {
		_, err := twapFunc(anchorNs)(series, cfg)
		return err
	}

	t.Run("every key is required", func(t *testing.T) {
		t.Parallel()
		// A defaulted threshold would mean accepting a window against a rule
		// nobody wrote down.
		for _, missing := range []string{"window", "minSamples", "maxHeadGap", "maxInteriorGap", "maxTailGap"} {
			cfg := twapConfigMap(3, 1, 0, 0, 0)
			delete(cfg, missing)
			err := call(cfg)
			require.ErrorIs(t, err, ErrTWAPConfig, "missing %s", missing)
			require.ErrorContains(t, err, missing)
		}
	})

	t.Run("unknown keys are rejected", func(t *testing.T) {
		t.Parallel()
		cfg := twapConfigMap(3, 1, 0, 0, 0)
		cfg["maxHeadGapp"] = 1
		err := call(cfg)
		require.ErrorIs(t, err, ErrTWAPConfig)
		require.ErrorContains(t, err, "maxHeadGapp")
	})

	t.Run("window must be whole seconds and positive", func(t *testing.T) {
		t.Parallel()
		cfg := twapConfigMap(3, 1, 0, 0, 0)
		cfg["window"] = 1500 * time.Millisecond
		require.ErrorContains(t, call(cfg), "whole number of seconds")

		cfg["window"] = time.Duration(0)
		require.ErrorContains(t, call(cfg), "at least one second")

		cfg["window"] = 48 * time.Hour
		require.ErrorContains(t, call(cfg), "exceeds the maximum")
	})

	t.Run("oversized configuration values are rejected, not wrapped", func(t *testing.T) {
		t.Parallel()
		// decimal.IntPart narrows through int64 and returns the low 64 bits of
		// an oversized value, so 2^64+1 comes back as 1. Bounding after that
		// would silently accept a one-second window or a minSamples of 1. TWAP
		// configuration is not required to be literal (see checkTWAP), so these
		// values can come from stream data, bounded only by MaxDecimalExponent.
		wrapped := decimal.RequireFromString("18446744073709551617")

		cfg := twapConfigMap(3, 1, 0, 0, 0)
		// A whole number of seconds, so it clears the modulo check first.
		cfg["window"] = wrapped.Mul(decimal.NewFromInt(int64(time.Second)))
		require.ErrorContains(t, call(cfg), "exceeds the maximum")

		for _, key := range []string{"minSamples", "maxHeadGap", "maxInteriorGap", "maxTailGap"} {
			cfg := twapConfigMap(3, 1, 0, 0, 0)
			cfg[key] = wrapped
			err := call(cfg)
			require.ErrorIs(t, err, ErrTWAPConfig, key)
			require.ErrorContains(t, err, "at most", key)
		}
	})

	t.Run("thresholds must be whole and non-negative", func(t *testing.T) {
		t.Parallel()
		cfg := twapConfigMap(3, 1, 0, 0, 0)
		cfg["maxHeadGap"] = -1
		require.ErrorContains(t, call(cfg), "at least 0")

		cfg = twapConfigMap(3, 1, 0, 0, 0)
		cfg["minSamples"] = 0
		require.ErrorContains(t, call(cfg), "at least 1")

		cfg = twapConfigMap(3, 1, 0, 0, 0)
		cfg["maxTailGap"] = 1.5
		require.ErrorContains(t, call(cfg), "whole number")
	})

	t.Run("minSamples cannot exceed the window", func(t *testing.T) {
		t.Parallel()
		require.ErrorContains(t, call(twapConfigMap(3, 4, 0, 0, 0)), "exceeds the")
	})

	t.Run("a window too thin to satisfy minSamples is a rejection, not a config error", func(t *testing.T) {
		t.Parallel()
		// Whether the requested depth can ever supply minSamples is a static
		// property checked at configuration time. At runtime a thin window is a
		// data condition, and must come back with the measured statistics.
		shallow := twapSeries(t, []int64{100})
		_, err := twapFunc(anchorNs)(shallow, twapConfigMap(3, 3, 0, 2, 2))
		require.ErrorIs(t, err, ErrTWAPRejected)
		var rejection *TWAPRejection
		require.ErrorAs(t, err, &rejection)
		assert.Equal(t, []TWAPRejectionReason{ReasonInsufficientSamples}, rejection.Reasons)
	})

	t.Run("not a configuration map", func(t *testing.T) {
		t.Parallel()
		require.ErrorIs(t, call(42), ErrTWAPConfig)
	})

	t.Run("not a window", func(t *testing.T) {
		t.Parallel()
		_, err := twapFunc(anchorNs)(decimal.NewFromInt(1), twapConfigMap(3, 1, 0, 0, 0))
		require.ErrorContains(t, err, "expects a history window")
	})
}

// TestTWAP_NonPositivePriceRejected covers the log-space requirement: a
// non-positive price has no logarithm, so the window cannot be filled.
func TestTWAP_NonPositivePriceRejected(t *testing.T) {
	t.Parallel()

	series, err := NewSeries(
		[]decimal.Decimal{decimal.NewFromInt(100), decimal.Zero},
		[]uint64{twapWindowStartSeconds * uint64(time.Second), (twapWindowStartSeconds + 1) * uint64(time.Second)},
	)
	require.NoError(t, err)

	anchorNs := uint64(twapWindowStartSeconds+3) * uint64(time.Second)
	_, err = twapFunc(anchorNs)(series, twapConfigMap(3, 1, 2, 2, 2))
	require.ErrorContains(t, err, "must be positive")
}

// TestTWAP_Unbound covers the patch-bypass equivalent for TWAP: without a round
// to anchor the window, it must refuse rather than invent one.
func TestTWAP_Unbound(t *testing.T) {
	t.Parallel()

	_, err := twapUnbound(nil, nil)
	require.ErrorContains(t, err, "no observation timestamp bound")

	// A pooled environment always carries a bound TWAP; release restores the
	// unbound default.
	env := NewEnv(uint64(5 * time.Second))
	bound, ok := env["TWAP"].(func(any, any) (decimal.Decimal, error))
	require.True(t, ok, "NewEnv must bind TWAP to the round")
	env.release()

	series := twapSeries(t, []int64{100, 200, 300})
	_, err = bound(series, twapConfigMap(3, 1, 0, 0, 0))
	require.Error(t, err, "the round anchor is 5s, so the window is far from these observations")
}

// TestTWAP_RejectionMessage checks an operator can tell what failed without
// reproducing the calculation.
func TestTWAP_RejectionMessage(t *testing.T) {
	t.Parallel()

	prices := []int64{100, 0, 0, 0, 0}
	series := twapSeries(t, prices)
	anchorNs := uint64(twapWindowStartSeconds+len(prices)) * uint64(time.Second)

	_, err := twapFunc(anchorNs)(series, twapConfigMap(len(prices), 3, 0, 5, 2))
	require.Error(t, err)

	var rejection *TWAPRejection
	require.ErrorAs(t, err, &rejection)
	assert.Equal(t, 1, rejection.M)
	assert.Equal(t, 4, rejection.Gtail)
	assert.Equal(t, 3, rejection.MinSamples)
	assert.Equal(t, 2, rejection.MaxTailGap)

	message := err.Error()
	for _, want := range []string{"min_samples", "tail_gap_too_long", "M=1/3", "Gtail=4/2"} {
		assert.Contains(t, message, want)
	}
	assert.True(t, errors.Is(err, ErrTWAPRejected))
	assert.Contains(t, fmt.Sprint(err), "rejected")
}
