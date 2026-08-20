package calculated

import (
	"fmt"
	"sync"

	"github.com/shopspring/decimal"
)

// transcendentalMu serializes every call into shopspring/decimal's
// transcendental functions.
//
// Decimal.ExpTaylor memoizes factorials in an unsynchronized package-level slice
// (decimal.go: `var factorials = []Decimal{New(1, 0)}`) that it appends to, and
// Ln reads through the same path. Concurrent calls therefore race, and an append
// racing with a read can yield a corrupted value rather than merely a stale one —
// which in a consensus path means two nodes disagreeing, or a panic.
//
// TWAP makes it far more likely by calling ln and exp once per bucket.
//
// The lock is taken per call rather than per evaluation to keep hold times short.
// The cost is negligible against the arithmetic it guards.
var transcendentalMu sync.Mutex

// decimalLn is decimal.Ln under the transcendental lock.
func decimalLn(x decimal.Decimal, prec int32) (result decimal.Decimal, err error) {
	transcendentalMu.Lock()
	defer transcendentalMu.Unlock()
	defer recoverTranscendental("logarithm", x, &result, &err)
	return x.Ln(prec)
}

// decimalExpTaylor is decimal.ExpTaylor under the transcendental lock.
func decimalExpTaylor(x decimal.Decimal, prec int32) (result decimal.Decimal, err error) {
	transcendentalMu.Lock()
	defer transcendentalMu.Unlock()
	defer recoverTranscendental("exponential", x, &result, &err)
	return x.ExpTaylor(prec)
}

// recoverTranscendental converts a panic from shopspring/decimal into an error.
//
// The library panics on some extreme inputs rather than returning an error (see
// decimalPow for a case found by fuzzing). An expression must never be able to
// bring the node down: turning it into an error makes the channel unreportable,
// which is the correct fail-closed outcome.
func recoverTranscendental(operation string, input decimal.Decimal, result *decimal.Decimal, err *error) {
	if r := recover(); r != nil {
		*result = decimal.Decimal{}
		*err = fmt.Errorf("%s of %s could not be computed: %v", operation, input, r)
	}
}

// decimalPow is decimal.PowWithPrecision under the transcendental lock. Powers
// with a non-integer exponent are evaluated via the same logarithm and
// exponential machinery.
//
// The exponent is bounded first. shopspring/decimal PANICS on an exponent large
// enough to overflow the result's int32 scale ("exponent ... overflows an
// int32!"), and spends a long time computing before it gets there: a fuzzed
// Pow(s1, s2) with both values around 2.6e31 burned 22 seconds of CPU and then
// panicked. Stream values come from consensus, so every node would hit that in
// the same round — a panic in StateTransition takes the node down, and 22 seconds
// blows the round budget even without one. The transcendental lock makes it worse
// by serializing that work against every other plugin instance in the process.
//
// A value beyond MaxDecimalExponent could not be stored or transmitted anyway, so
// refusing it early costs nothing real.
func decimalPow(base, exponent decimal.Decimal, prec int32) (result decimal.Decimal, err error) {
	if err := checkPowExponent(exponent); err != nil {
		return decimal.Decimal{}, err
	}

	transcendentalMu.Lock()
	defer transcendentalMu.Unlock()

	// Backstop: the bound above covers the case seen in the wild, but the
	// library reserves the right to panic on other extremes and an expression
	// must never be able to bring the node down. Converting it to an error makes
	// the channel unreportable, which is the correct fail-closed outcome.
	defer func() {
		if r := recover(); r != nil {
			result = decimal.Decimal{}
			err = fmt.Errorf("power of %s by %s could not be computed: %v", base, exponent, r)
		}
	}()

	return base.PowWithPrecision(exponent, prec)
}

// maxPowExponent bounds the magnitude of an exponent passed to a power.
//
// A power whose result needs more than MaxDecimalExponent decimal places, or
// that many integer digits, is unusable downstream: protocol decoding rejects
// such a decimal outright. The bound is expressed on the exponent alone because
// it has to be cheap — the whole point is to refuse before the expensive
// computation starts, not after.
const maxPowExponent = 100_000

func checkPowExponent(exponent decimal.Decimal) error {
	if exponent.Abs().GreaterThan(decimal.NewFromInt(maxPowExponent)) {
		return fmt.Errorf("exponent %s exceeds the maximum magnitude of %d", exponent, maxPowExponent)
	}
	return nil
}

// decimalToInt converts an integral decimal to an int, refusing anything outside
// [minimum, maximum].
//
// The bounds are checked as decimals, before the narrowing. decimal.IntPart
// narrows through big.Int.Int64, which returns the low 64 bits of an oversized
// value rather than failing: 2^64+1 comes back as 1. Every caller here is
// choosing a sample count, a window length or a gap threshold, so a wrapped
// value does not error -- it silently selects a different calculation than the
// expression asked for. These arguments are not required to be literal, so the
// value can come from a stream and is bounded only by MaxDecimalExponent, which
// is far wider than an int64.
func decimalToInt(name string, d decimal.Decimal, minimum, maximum int64) (int, error) {
	if !d.IsInteger() {
		return 0, fmt.Errorf("%s must be a whole number, got %s", name, d)
	}
	if d.LessThan(decimal.NewFromInt(minimum)) {
		return 0, fmt.Errorf("%s must be at least %d, got %s", name, minimum, d)
	}
	if d.GreaterThan(decimal.NewFromInt(maximum)) {
		return 0, fmt.Errorf("%s must be at most %d, got %s", name, maximum, d)
	}
	return int(d.IntPart()), nil
}

// Determinism rules for every calculation in this package.
//
// Expression results become consensus values, so two oracles computing the same
// expression over the same inputs must produce bit-identical output. Three rules
// follow:
//
//  1. No float64. math.Log and math.Exp are not guaranteed bit-identical across
//     architectures or Go versions, so all logarithms and exponentials go through
//     decimal.Ln and decimal.ExpTaylor at a fixed precision. This is why the TWAP
//     implementation here is a port of the mercury float-based one, not a reuse.
//  2. No reliance on decimal.DivisionPrecision. That is a mutable package-level
//     global: anything in the process can change it and silently move every Div
//     result. Every division here passes an explicit precision (divRound).
//  3. Fixed rounding at every step of an iterative calculation, so the result
//     cannot depend on how much internal precision happened to survive.
const (
	// legacyDivisionPrecision is decimal.DivisionPrecision's default, and the
	// precision Div and Avg have effectively always used.
	//
	// It is pinned rather than raised to the package precision because changing
	// it would move the trailing digits of every existing calculated stream — a
	// DON-visible output change that would have to be a coordinated upgrade.
	// New functions use precision instead.
	legacyDivisionPrecision = 16
)

// divRound divides with an explicit precision, refusing division by zero.
//
// Always prefer this to Decimal.Div: Div reads decimal.DivisionPrecision, a
// mutable global, so its result is a property of process state rather than of
// the inputs.
func divRound(x, y decimal.Decimal, prec int32) (decimal.Decimal, error) {
	if y.IsZero() {
		return decimal.Decimal{}, fmt.Errorf("division by zero")
	}
	return x.DivRound(y, prec), nil
}

// divRoundByInt divides by a positive count, for the many places an aggregate is
// divided by a number of samples.
func divRoundByInt(x decimal.Decimal, n int, prec int32) (decimal.Decimal, error) {
	if n <= 0 {
		return decimal.Decimal{}, fmt.Errorf("cannot divide by %d", n)
	}
	return divRound(x, decimal.NewFromInt(int64(n)), prec)
}

// ln is a deterministic natural logarithm at double precision, for values that
// will be further combined before the result is rounded.
func ln(x decimal.Decimal) (decimal.Decimal, error) {
	if !x.IsPositive() {
		return decimal.Decimal{}, fmt.Errorf("cannot take the logarithm of %s: value must be positive", x)
	}
	return decimalLn(x, doublePrecision)
}

// exp is a deterministic exponential at double precision, the inverse of ln.
//
// The argument is bounded because ExpTaylor's cost grows with the size of the
// result, not of the input: exp(1e6) has ~434,000 digits and does not complete in
// any useful time. Callers currently only pass logarithms of stored values, which
// MaxDecimalExponent already bounds to about ±2302, so the limit is not reachable
// through TWAP today — it is here so that stays true if another caller appears.
func exp(x decimal.Decimal) (decimal.Decimal, error) {
	if x.Abs().GreaterThan(decimal.NewFromInt(maxExpArgument)) {
		return decimal.Decimal{}, fmt.Errorf("exponential argument %s exceeds the maximum magnitude of %d", x, maxExpArgument)
	}
	return decimalExpTaylor(x, doublePrecision)
}

// maxExpArgument bounds the argument to exp.
//
// exp(x) has roughly x/ln(10) decimal digits, so this is the largest argument
// whose result is still within MaxDecimalExponent (1000) and therefore still
// storable: 1000 * ln(10) is about 2302, rounded up for headroom.
const maxExpArgument = 2400

// sqrt is a deterministic square root, rounded to the package precision.
func sqrt(x decimal.Decimal) (decimal.Decimal, error) {
	if x.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("cannot take the square root of a negative number: %s", x)
	}
	res, err := decimalPow(x, decimal.NewFromFloat(0.5), doublePrecision)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return res.Round(precision), nil
}
