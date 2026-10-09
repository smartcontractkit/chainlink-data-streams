package calculated

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// TWAP is ported from the original spec (ADR 0013/0014/0015), semantics are unchanged.
// It is a port rather than a reuse for two reasons: the source works from decoded
// values and a clock, while here the input is an already-agreed history window;
// and the source computes in float64, which is not guaranteed bit-identical
// across architectures and so cannot appear in a consensus path. Every logarithm,
// exponential and division below is decimal at a fixed precision.
var (
	// ErrTWAPRejected is what every rejection satisfies errors.Is against, so
	// callers can detect a rejected window without inspecting the reasons.
	ErrTWAPRejected = errors.New("TWAP window rejected by the acceptance rule")

	// ErrTWAPConfig is returned for a malformed configuration. Configuration is
	// static, so this is a deployment error rather than a data condition.
	ErrTWAPConfig = errors.New("invalid TWAP configuration")
)

// TWAPRejectionReason enumerates why a window failed the acceptance rule. A
// window can fail several checks at once.
type TWAPRejectionReason string

const (
	// ReasonInsufficientSamples: M < minSamples, the coverage floor.
	ReasonInsufficientSamples TWAPRejectionReason = "min_samples"
	// ReasonHeadGapTooLong: Ghead > maxHeadGap, backfilled prefix too long.
	ReasonHeadGapTooLong TWAPRejectionReason = "head_gap_too_long"
	// ReasonInteriorGapTooLong: Gint > maxInteriorGap, longest both-sides-anchored gap too long.
	ReasonInteriorGapTooLong TWAPRejectionReason = "interior_gap_too_long"
	// ReasonTailGapTooLong: Gtail > maxTailGap, carry-forward suffix too long.
	ReasonTailGapTooLong TWAPRejectionReason = "tail_gap_too_long"
)

// TWAPRejection carries the measured statistics alongside the thresholds they
// failed, so an operator can tell a thin window from a stalled feed without
// reproducing the calculation.
type TWAPRejection struct {
	Reasons                                            []TWAPRejectionReason
	M, Ghead, Gint, Gtail                              int
	MinSamples, MaxHeadGap, MaxInteriorGap, MaxTailGap int
	WindowStartSeconds, WindowEndSeconds               int64
	Records                                            int
}

func (e *TWAPRejection) Error() string {
	reasons := make([]string, 0, len(e.Reasons))
	for _, reason := range e.Reasons {
		reasons = append(reasons, string(reason))
	}
	return fmt.Sprintf("TWAP: window [%d, %d) rejected (%s): M=%d/%d Ghead=%d/%d Gint=%d/%d Gtail=%d/%d from %d records",
		e.WindowStartSeconds, e.WindowEndSeconds, strings.Join(reasons, ","),
		e.M, e.MinSamples, e.Ghead, e.MaxHeadGap, e.Gint, e.MaxInteriorGap, e.Gtail, e.MaxTailGap, e.Records)
}

func (e *TWAPRejection) Is(target error) bool { return target == ErrTWAPRejected }

// twapConfig is the acceptance rule for one window size. Every field is required:
// a defaulted threshold would silently accept a window an operator never approved.
type twapConfig struct {
	windowSeconds  int64
	minSamples     int
	maxHeadGap     int
	maxInteriorGap int
	maxTailGap     int
}

// twapObservation is one observed second of the window: its offset from the
// window start and the price recorded in it.
//
// The specification operates on a dense series of one-second buckets. Only the
// observed ones carry information, since every missing bucket is a function of
// the observations either side of it, so the window is held as its observations
// alone. That keeps the cost of a window proportional to its records rather than
// its length: a 24 hour window over a 1024-record history costs what its records
// and interior gaps cost, not 86400 buckets.
//
// The specification is written in log-price space throughout: build X[i] = ln(P[i]),
// fill gaps in that space, then average exp of the filled series. This stores the
// price instead, and moves into log space only where filling actually requires it.
//
// That is not a shortcut, it is the same series. For an observed bucket the
// specification computes exp(ln(P)) = P. For a head gap it backfills the first
// observed log-price, and for a tail gap it carries the last, so exponentiating
// those yields that same observed price. Only interior interpolation produces a
// value that is not already a price.
//
// The reason it matters is cost. Filling in log space needs a logarithm per
// observed bucket and an exponential per bucket in the window — about 600
// operations for a five-minute window — and they all serialize on the
// transcendental lock. Measured: 197ms per evaluation for a 300-second window,
// and 7.2s for 32 such channels in one round, against a round budget on the
// order of a second. Doing it this way, a fully covered window needs no
// transcendental operations at all, and a window with gaps needs one power per
// interior gap.
type twapObservation struct {
	offset int
	price  decimal.Decimal
}

// twapFunc returns the TWAP function bound to a round's consensus observation
// timestamp, which anchors the window.
//
// The anchor has to come from the round rather than from the data: taking it from
// the newest record would silently shorten the window whenever a feed stalled,
// which is exactly the condition the acceptance rule exists to catch.
func twapFunc(observationTimestampNanoseconds uint64) func(any, any) (decimal.Decimal, error) {
	return func(x any, rawConfig any) (decimal.Decimal, error) {
		series, err := window("TWAP", x)
		if err != nil {
			return decimal.Decimal{}, err
		}
		cfg, err := parseTWAPConfig(rawConfig)
		if err != nil {
			return decimal.Decimal{}, err
		}
		// NOTE: whether the requested history depth can ever supply minSamples
		// observations is a static property of the configuration, and is checked
		// at configuration time rather than here. Checking it here would turn a
		// specification-defined rejection (M < minSamples, a data condition with
		// diagnostics) into a configuration error, losing the measured
		// statistics an operator needs.
		return twap(series, cfg, int64(observationTimestampNanoseconds/uint64(time.Second)))
	}
}

// twapUnbound is the default TWAP binding. NewEnv replaces it with a function
// bound to the round's observation timestamp; reaching this one means TWAP was
// called against an environment that was not built by NewEnv.
func twapUnbound(any, any) (decimal.Decimal, error) {
	return decimal.Decimal{}, errors.New("TWAP has no observation timestamp bound; the environment was not created by NewEnv")
}

func twap(series Series, cfg twapConfig, anchorSeconds int64) (decimal.Decimal, error) {
	windowStart := anchorSeconds - cfg.windowSeconds
	n := int(cfg.windowSeconds)

	values, timestamps := series.Values(), series.Timestamps()
	observations := make([]twapObservation, 0, len(timestamps))
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
		// Timestamps are strictly increasing, so records sharing a second are
		// adjacent and a later one replaces an earlier one: newest wins.
		offset := int(seconds - windowStart)
		if last := len(observations) - 1; last >= 0 && observations[last].offset == offset {
			observations[last].price = values[i]
			continue
		}
		observations = append(observations, twapObservation{offset: offset, price: values[i]})
	}

	m, gHead, gInt, gTail := twapGapStats(observations, n)

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

	return twapFillThenAverage(observations, n)
}

// twapGapStats measures M, Ghead, Gint and Gtail over a window of n buckets
// from its observations, which must be in ascending offset order (spec §2,
// ADR 0015).
//
// Ghead and Gtail are kept separate from Gint deliberately: Gint is the
// both-sides-anchored statistic, and a head or tail run has only one anchor. A
// window with no observations has a single run with no anchors at all, which
// is classified as none of them; such a window is always rejected by the M
// check.
func twapGapStats(observations []twapObservation, n int) (m, gHead, gInt, gTail int) {
	m = len(observations)
	if m == 0 {
		return 0, 0, 0, 0
	}
	gHead = observations[0].offset
	gTail = n - 1 - observations[m-1].offset
	for i := 1; i < m; i++ {
		gInt = max(gInt, observations[i].offset-observations[i-1].offset-1)
	}
	return m, gHead, gInt, gTail
}

// twapFillThenAverage fills every bucket of a window of n buckets per spec §4
// and returns the mean price over the full window.
//
// Callers must only reach this once the acceptance rule has passed, which
// guarantees at least one observation.
func twapFillThenAverage(observations []twapObservation, n int) (decimal.Decimal, error) {
	first, last := observations[0], observations[len(observations)-1]

	// Head gap: backfill the first observed price (ADR 0015). Tail gap: carry
	// forward the last observed price (spec §4.3). Decimal multiplication and
	// addition are exact, so a run of k copies adds price*k, the same sum as
	// adding each copy.
	sum := first.price.Mul(decimal.NewFromInt(int64(first.offset)))
	sum = sum.Add(last.price.Mul(decimal.NewFromInt(int64(n - 1 - last.offset))))

	for i, observation := range observations {
		sum = sum.Add(observation.price) // spec §4.1: X[i] passes through
		if i == 0 || observation.offset-observations[i-1].offset == 1 {
			continue
		}
		// Interior gap: log-linear interpolation between the bracketing
		// anchors (spec §4.2). This is the only case that needs log space, so
		// it is the only one that pays for it.
		filled, err := twapInterpolate(observations[i-1], observation)
		if err != nil {
			return decimal.Decimal{}, err
		}
		sum = sum.Add(filled)
	}

	// TWAP = mean over N (spec §4-5, denominator N not M).
	return divRoundByInt(sum, n, precision)
}

// twapInterpolate fills the missing run between two adjacent observations
// (spec §4.2) and returns the sum of the filled prices.
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
// same reason. That is also why the steps cannot be summed in closed form.
func twapInterpolate(left, right twapObservation) (decimal.Decimal, error) {
	growth, err := divRound(right.price, left.price, doublePrecision)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("TWAP: bucket %d: %w", left.offset, err)
	}
	exponent, err := divRoundByInt(decimal.NewFromInt(1), right.offset-left.offset, doublePrecision)
	if err != nil {
		return decimal.Decimal{}, err
	}
	ratio, err := decimalPow(growth, exponent, doublePrecision)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("TWAP: interpolating buckets %d..%d: %w", left.offset+1, right.offset-1, err)
	}

	sum := decimal.Zero
	price := left.price
	for range right.offset - left.offset - 1 {
		price = price.Mul(ratio).Round(doublePrecision)
		sum = sum.Add(price)
	}
	return sum, nil
}

// parseTWAPConfig decodes and validates the configuration map.
//
// Every key is required and no key is optional: a defaulted threshold would mean
// accepting a window against a rule nobody wrote down. Unknown keys are rejected
// too, so a typo fails loudly instead of leaving a threshold at its intended
// value by accident.
func parseTWAPConfig(raw any) (twapConfig, error) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return twapConfig{}, fmt.Errorf("%w: expected a configuration map, got %T", ErrTWAPConfig, raw)
	}

	const (
		keyWindow         = "window"
		keyMinSamples     = "minSamples"
		keyMaxHeadGap     = "maxHeadGap"
		keyMaxInteriorGap = "maxInteriorGap"
		keyMaxTailGap     = "maxTailGap"
	)
	known := map[string]bool{
		keyWindow: true, keyMinSamples: true, keyMaxHeadGap: true,
		keyMaxInteriorGap: true, keyMaxTailGap: true,
	}
	unknown := make([]string, 0)
	for key := range fields {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return twapConfig{}, fmt.Errorf("%w: unknown keys %s", ErrTWAPConfig, strings.Join(unknown, ", "))
	}

	windowSeconds, err := twapWindowSeconds(fields[keyWindow])
	if err != nil {
		return twapConfig{}, err
	}
	minSamples, err := twapConfigInt(fields, keyMinSamples, 1)
	if err != nil {
		return twapConfig{}, err
	}
	maxHeadGap, err := twapConfigInt(fields, keyMaxHeadGap, 0)
	if err != nil {
		return twapConfig{}, err
	}
	maxInteriorGap, err := twapConfigInt(fields, keyMaxInteriorGap, 0)
	if err != nil {
		return twapConfig{}, err
	}
	maxTailGap, err := twapConfigInt(fields, keyMaxTailGap, 0)
	if err != nil {
		return twapConfig{}, err
	}

	if int64(minSamples) > windowSeconds {
		return twapConfig{}, fmt.Errorf("%w: minSamples %d exceeds the %d one-second buckets in the window",
			ErrTWAPConfig, minSamples, windowSeconds)
	}

	return twapConfig{
		windowSeconds:  windowSeconds,
		minSamples:     minSamples,
		maxHeadGap:     maxHeadGap,
		maxInteriorGap: maxInteriorGap,
		maxTailGap:     maxTailGap,
	}, nil
}

// twapWindowSeconds resolves the window length, which must be a whole number of
// seconds because the calculation is defined over one-second buckets.
func twapWindowSeconds(raw any) (int64, error) {
	if raw == nil {
		return 0, fmt.Errorf("%w: window is required", ErrTWAPConfig)
	}

	var nanoseconds decimal.Decimal
	switch v := raw.(type) {
	case time.Duration:
		nanoseconds = decimal.NewFromInt(int64(v))
	default:
		d, err := toDecimal(raw)
		if err != nil {
			return 0, fmt.Errorf("%w: window: %s", ErrTWAPConfig, err)
		}
		nanoseconds = d
	}

	perSecond := decimal.NewFromInt(int64(time.Second))
	if !nanoseconds.Mod(perSecond).IsZero() {
		return 0, fmt.Errorf("%w: window must be a whole number of seconds", ErrTWAPConfig)
	}
	// DivRound rather than Div: Div reads the mutable decimal.DivisionPrecision
	// global. The division is exact here, but the rule holds everywhere.
	// Compared and bounded as a decimal, before any narrowing; see decimalToInt.
	// The upper bound also caps the per-evaluation work: the calculation
	// steps through every missing interior second of the window.
	secondsDecimal := nanoseconds.DivRound(perSecond, 0)
	if secondsDecimal.LessThan(decimal.NewFromInt(1)) {
		return 0, fmt.Errorf("%w: window must be at least one second, got %s", ErrTWAPConfig, secondsDecimal)
	}
	if secondsDecimal.GreaterThan(decimal.NewFromInt(twapMaxWindowSeconds)) {
		return 0, fmt.Errorf("%w: window of %s seconds exceeds the maximum of %d",
			ErrTWAPConfig, secondsDecimal, twapMaxWindowSeconds)
	}
	seconds, err := decimalToInt("window", secondsDecimal, 1, twapMaxWindowSeconds)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrTWAPConfig, err)
	}
	return int64(seconds), nil
}

// twapMaxWindowSeconds bounds the number of one-second buckets a single TWAP
// evaluation may interpolate. 24 hours is far beyond any settlement window
// while keeping the per-round work bounded.
const twapMaxWindowSeconds = 24 * 60 * 60

func twapConfigInt(fields map[string]any, key string, minimum int) (int, error) {
	raw, ok := fields[key]
	if !ok || raw == nil {
		return 0, fmt.Errorf("%w: %s is required", ErrTWAPConfig, key)
	}
	d, err := toDecimal(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %s", ErrTWAPConfig, key, err)
	}
	// Bounded as a decimal before narrowing; see decimalToInt. The upper bound
	// is the window cap, since every one of these counts seconds or samples
	// inside a window that cannot itself be longer than that.
	value, err := decimalToInt(key, d, int64(minimum), twapMaxWindowSeconds)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrTWAPConfig, err)
	}
	return value, nil
}
