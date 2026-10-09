package calculated

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// The dense implementation TWAP shipped with: one bucket per second of the
// window. Kept as the reference the sparse implementation is checked against,
// bit for bit, by TestTWAP_MatchesDenseReference.

type twapBucket struct {
	observed bool
	price    decimal.Decimal
}

func twapDenseReference(series Series, cfg twapConfig, anchorSeconds int64) (decimal.Decimal, error) {
	windowStart := anchorSeconds - cfg.windowSeconds
	buckets := make([]twapBucket, cfg.windowSeconds)

	values, timestamps := series.Values(), series.Timestamps()
	for i, ts := range timestamps {
		seconds := int64(ts / uint64(time.Second))
		if seconds < windowStart || seconds >= anchorSeconds {
			continue // outside the half-open window (ADR 0013)
		}
		// The price must be positive: the filling rules are defined in log space,
		// so a non-positive price has no representation there. Checked here for
		// every observed bucket rather than only where a logarithm is taken, so
		// acceptance does not depend on where the gaps happen to fall.
		if !values[i].IsPositive() {
			return decimal.Decimal{}, fmt.Errorf("TWAP: record %d: price %s must be positive", i, values[i])
		}
		// Timestamps are strictly increasing, so a later record legitimately
		// overwrites an earlier one in the same bucket: newest wins.
		buckets[seconds-windowStart] = twapBucket{observed: true, price: values[i]}
	}

	m, gHead, gInt, gTail := twapDenseGapStats(buckets)

	var reasons []TWAPRejectionReason
	// A floor of 1 observation is required for head backfill to have an anchor.
	// With a validated minSamples >= 1 this is redundant, but it keeps a
	// misconfiguration from reaching an out-of-range index below.
	minSamples := max(cfg.minSamples, 1)
	if m < minSamples {
		reasons = append(reasons, ReasonInsufficientSamples)
	}
	if gHead > cfg.maxHeadGap {
		reasons = append(reasons, ReasonHeadGapTooLong)
	}
	if gInt > cfg.maxInteriorGap {
		reasons = append(reasons, ReasonInteriorGapTooLong)
	}
	if gTail > cfg.maxTailGap {
		reasons = append(reasons, ReasonTailGapTooLong)
	}
	if len(reasons) > 0 {
		return decimal.Decimal{}, &TWAPRejection{
			Reasons: reasons,
			M:       m, Ghead: gHead, Gint: gInt, Gtail: gTail,
			MinSamples: cfg.minSamples, MaxHeadGap: cfg.maxHeadGap,
			MaxInteriorGap: cfg.maxInteriorGap, MaxTailGap: cfg.maxTailGap,
			WindowStartSeconds: windowStart, WindowEndSeconds: anchorSeconds,
			Records: series.Len(),
		}
	}

	return twapDenseFillThenAverage(buckets)
}

// twapDenseGapStats measures M, Ghead, Gint and Gtail by classifying each missing run
// by its position (spec §2, ADR 0015).
//
// Ghead and Gtail are kept separate from Gint deliberately: Gint is the
// both-sides-anchored statistic, and a head or tail run has only one anchor. A
// run spanning the whole window is classified as none of them because it has no
// anchors at all; such a window is always rejected by the M check.
func twapDenseGapStats(buckets []twapBucket) (m, gHead, gInt, gTail int) {
	n := len(buckets)
	for i := 0; i < n; {
		runStart := i
		observed := buckets[i].observed
		for i < n && buckets[i].observed == observed {
			i++
		}
		runLen := i - runStart

		if observed {
			m += runLen
			continue
		}
		switch {
		case runStart == 0 && i == n:
			// Entire window missing: no anchors, so not head, tail or interior.
		case runStart == 0:
			gHead = runLen
		case i == n:
			gTail = runLen
		default:
			gInt = max(gInt, runLen)
		}
	}
	return m, gHead, gInt, gTail
}

// twapDenseFillThenAverage fills every bucket per spec §4 and returns the mean price
// over the full window.
//
// Callers must only reach this once the acceptance rule has passed, which
// guarantees at least one observation.
func twapDenseFillThenAverage(buckets []twapBucket) (decimal.Decimal, error) {
	n := len(buckets)
	filled := make([]decimal.Decimal, n)

	for i := 0; i < n; {
		if buckets[i].observed {
			filled[i] = buckets[i].price // spec §4.1: X[i] passes through
			i++
			continue
		}
		runStart := i
		for i < n && !buckets[i].observed {
			i++
		}
		switch {
		case runStart == 0:
			// Head gap: backfill the first observed price (ADR 0015).
			// buckets[i] is observed, because a window with no observation at
			// all was rejected above.
			for k := 0; k < i; k++ {
				filled[k] = buckets[i].price
			}
		case i == n:
			// Tail gap: carry forward the last observed price (spec §4.3).
			for k := runStart; k < n; k++ {
				filled[k] = buckets[runStart-1].price
			}
		default:
			// Interior gap: log-linear interpolation between the bracketing
			// anchors at runStart-1 and i (spec §4.2). This is the only case
			// that needs log space, so it is the only one that pays for it.
			if err := twapDenseInterpolate(buckets, filled, runStart, i); err != nil {
				return decimal.Decimal{}, err
			}
		}
	}

	// TWAP = mean over N (spec §4-5, denominator N not M).
	sum := decimal.Zero
	for _, price := range filled {
		sum = sum.Add(price)
	}
	return divRoundByInt(sum, n, precision)
}

// twapDenseInterpolate fills the missing run [runStart, rightIdx) between its
// bracketing anchors (spec §4.2).
//
// Linear interpolation in log space is geometric interpolation in price space: a
// gap between 100 and 1600 fills as 200, 400, 800, not as evenly spaced prices.
// So rather than exponentiating each interpolated log-price, this takes the
// constant per-second ratio once and steps through the gap by multiplication:
//
//	ratio    = (right / left) ^ (1 / span)
//	filled[k] = filled[k-1] * ratio
//
// One power per gap instead of two logarithms plus one exponential per missing
// bucket. With the spec's example thresholds a window can be missing 60 buckets,
// which cost ~73ms the other way and a fraction of that here. Exponentials are the
// expensive operation (~0.5ms each) and reducing their precision only helps by
// about a factor of two, so cutting their number is the only lever that matters.
//
// Determinism: the ratio is computed at a fixed precision and every step is
// rounded, so the sequence is reproducible — the same requirement EMA has, for the
// same reason.
func twapDenseInterpolate(buckets []twapBucket, filled []decimal.Decimal, runStart, rightIdx int) error {
	leftIdx := runStart - 1
	left, right := buckets[leftIdx].price, buckets[rightIdx].price

	growth, err := divRound(right, left, doublePrecision)
	if err != nil {
		return fmt.Errorf("TWAP: bucket %d: %w", leftIdx, err)
	}
	exponent, err := divRoundByInt(decimal.NewFromInt(1), rightIdx-leftIdx, doublePrecision)
	if err != nil {
		return err
	}
	ratio, err := decimalPow(growth, exponent, doublePrecision)
	if err != nil {
		return fmt.Errorf("TWAP: interpolating buckets %d..%d: %w", runStart, rightIdx-1, err)
	}

	price := left
	for k := runStart; k < rightIdx; k++ {
		price = price.Mul(ratio).Round(doublePrecision)
		filled[k] = price
	}
	return nil
}

// TestTWAP_MatchesDenseReference checks the sparse implementation against the
// dense one on random windows: the same value, digit for digit, or the same
// error. Records land several to a second, outside the window and across gaps
// of every kind, and thresholds vary so both acceptance and rejection are hit.
func TestTWAP_MatchesDenseReference(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))
	var accepted, rejected int
	for i := range 2000 {
		windowSeconds := 1 + rng.IntN(120)
		anchorSeconds := int64(1_000 + windowSeconds)

		var values []decimal.Decimal
		var timestamps []uint64
		ts := uint64(anchorSeconds-int64(windowSeconds)-2) * uint64(time.Second)
		for ts < uint64(anchorSeconds+2)*uint64(time.Second) {
			// Mostly sub-second steps within a second, sometimes a gap of
			// several seconds.
			if rng.IntN(4) == 0 {
				ts += uint64(1+rng.IntN(15)) * uint64(time.Second)
			} else {
				ts += uint64(1 + rng.IntN(int(time.Second)))
			}
			values = append(values, decimal.New(1+rng.Int64N(1_000_000), -int32(rng.IntN(9))))
			timestamps = append(timestamps, ts)
		}
		series, err := NewSeries(values, timestamps)
		require.NoError(t, err)

		cfg := twapConfig{
			windowSeconds:  int64(windowSeconds),
			minSamples:     1 + rng.IntN(windowSeconds),
			maxHeadGap:     rng.IntN(windowSeconds + 1),
			maxInteriorGap: rng.IntN(windowSeconds + 1),
			maxTailGap:     rng.IntN(windowSeconds + 1),
		}

		want, wantErr := twapDenseReference(series, cfg, anchorSeconds)
		got, gotErr := twap(series, cfg, anchorSeconds)
		if wantErr != nil {
			require.Error(t, gotErr, "case %d", i)
			require.Equal(t, wantErr.Error(), gotErr.Error(), "case %d", i)
			rejected++
			continue
		}
		require.NoError(t, gotErr, "case %d", i)
		require.Equal(t, want.String(), got.String(), "case %d", i)
		accepted++
	}
	// Both paths must actually be exercised for the comparison to mean anything.
	require.Greater(t, accepted, 100)
	require.Greater(t, rejected, 100)
}
