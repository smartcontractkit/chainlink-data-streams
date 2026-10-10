package calculated

import (
	"fmt"
	"math/big"
	"sync"

	"github.com/shopspring/decimal"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
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
// Only Ln is called now: exponentials go through expDecimal, which shares no
// state. TWAP still calls ln once per bucket.
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

// decimalPow is decimal.PowWithPrecision with its exponential replaced by
// expDecimal.
//
// The library evaluates base^(i+f), for the integer part i and fractional part
// f of the exponent, as base^i * exp(f * ln(base)), with the logarithm and the
// exponential taken at a precision raised to cover the operands. This keeps that
// structure and those precisions, so a result changes only where ExpTaylor was
// itself inaccurate. ExpTaylor sums the series on the full argument, needing
// about e*|x| terms of growing size: Sqrt of a stored value near 1e-943 took 4.4
// seconds under the transcendental lock. See expDecimal.
//
// The exponent is bounded first. shopspring/decimal PANICS on an exponent large
// enough to overflow the result's int32 scale ("exponent ... overflows an
// int32!"), and spends a long time computing before it gets there: a fuzzed
// Pow(s1, s2) with both values around 2.6e31 burned 22 seconds of CPU and then
// panicked. Stream values come from consensus, so every node would hit that in
// the same round — a panic in StateTransition takes the node down, and 22 seconds
// blows the round budget even without one.
//
// A value beyond MaxDecimalExponent could not be stored or transmitted anyway, so
// refusing it early costs nothing real.
func decimalPow(base, exponent decimal.Decimal, prec int32) (decimal.Decimal, error) {
	if err := checkPow(base, exponent); err != nil {
		return decimal.Decimal{}, err
	}

	intPart := exponent.Truncate(0)
	fracPart := exponent.Sub(intPart)
	// A zero base never reaches the logarithm, so the library's own handling of
	// it is kept as is.
	if fracPart.IsZero() || base.IsZero() {
		return decimalIntPow(base, exponent, prec)
	}
	if base.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("cannot represent imaginary value of x ** y, where x < 0 and y is non-integer decimal")
	}

	intPow, err := decimalIntPow(base, intPart, prec)
	if err != nil {
		return decimal.Decimal{}, err
	}

	// The precision the library raises the fractional part to.
	fracPrec := prec
	if digits := int32(base.NumDigits()); digits > fracPrec {
		fracPrec = digits
	}
	if digits := int32(exponent.NumDigits()); digits > fracPrec {
		fracPrec += digits
	}
	fracPrec += 10

	lnBase, err := decimalLn(base, fracPrec)
	if err != nil {
		return decimal.Decimal{}, err
	}
	fracPow, err := expDecimal(lnBase.Mul(fracPart), fracPrec)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return intPow.Mul(fracPow), nil
}

// decimalIntPow is decimal.PowWithPrecision for an integer exponent or a zero
// base, neither of which reaches a transcendental function.
//
// Backstop: checkPow covers the case seen in the wild, but the library reserves
// the right to panic on other extremes and an expression must never be able to
// bring the node down. Converting it to an error makes the channel
// unreportable, which is the correct fail-closed outcome.
func decimalIntPow(base, exponent decimal.Decimal, prec int32) (result decimal.Decimal, err error) {
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
// This is only the cheap first gate, and it exists so that the size of the
// result can be estimated without doing arithmetic on an absurd exponent. What
// actually has to be bounded is the result: see checkPow.
const maxPowExponent = 100_000

// checkPow refuses a power whose result would be too large to compute in useful
// time, or to store afterwards.
//
// Bounding the exponent alone is not enough, because the cost is driven by the
// size of the result. A power with a non-integer exponent is evaluated as
// exp(exponent * ln(base)), and ExpTaylor's cost grows with the number of digits
// it produces: Pow(3000, 50000.5) asks for exp(4e5), a number with about 174,000
// digits, which does not complete in any useful time. An integer exponent avoids
// the logarithm but not the size — the repeated multiplication produces the same
// number of digits.
//
// So the quantity exp() guards is checked here, ahead of the work: the magnitude
// of exponent * ln(base), against maxExpArgument, which is the largest argument
// whose result still fits MaxDecimalExponent. The logarithm needed to estimate it
// is taken at low precision on an operand bounded by checkOperand, so it is cheap
// next to the power it is protecting — and exact enough that its rounding error
// cannot move a value across a bound of 2400.
//
// Magnitude does not bound digits. The integer part of the exponent is applied
// by exact repeated multiplication before anything is rounded, so a base close
// to 1 with a long coefficient has a tiny logarithm and a huge result.
// That size is checked separately, against maxPowResultBits.
//
// Both operands come from consensus, so an unbounded power is not one node's
// problem: every node computes it in the same round, inside StateTransition,
// holding the transcendental lock.
func checkPow(base, exponent decimal.Decimal) error {
	if exponent.Abs().GreaterThan(decimal.NewFromInt(maxPowExponent)) {
		return fmt.Errorf("exponent %s exceeds the maximum magnitude of %d", exponent, maxPowExponent)
	}
	// A zero base is 0 or 1 whatever the exponent, and has no logarithm.
	abs := base.Abs()
	if abs.IsZero() {
		return nil
	}
	// An upper bound on the coefficient of base^|trunc(exponent)|.
	resultBits := decimal.NewFromInt(int64(abs.Coefficient().BitLen())).Mul(exponent.Truncate(0).Abs())
	if resultBits.GreaterThan(decimal.NewFromInt(maxPowResultBits)) {
		return fmt.Errorf("power of %s by %s needs a coefficient of up to %s bits, which exceeds the maximum of %d",
			base, exponent, resultBits, maxPowResultBits)
	}
	lnBase, err := decimalLn(abs, powEstimatePrecision)
	if err != nil {
		return fmt.Errorf("power of %s by %s could not be bounded: %w", base, exponent, err)
	}
	if magnitude := exponent.Mul(lnBase).Abs(); magnitude.GreaterThan(decimal.NewFromInt(maxExpArgument)) {
		return fmt.Errorf("power of %s by %s has a natural logarithm of %s, which exceeds the maximum magnitude of %d",
			base, exponent, magnitude, maxExpArgument)
	}
	return nil
}

// maxPowResultBits bounds the coefficient of the exact integer power computed
// before a power is rounded. It admits Pow(1.0001, 100000), about 1.4 million
// bits, which takes milliseconds. A 58 digit base near 1 raised to the same
// exponent needs 19 million bits and takes most of a second.
const maxPowResultBits = 1 << 21

// powEstimatePrecision is the precision of the logarithm used to size a power's
// result. It only has to place the result on the right side of maxExpArgument, so
// it is deliberately far below the precision of the calculation itself.
const powEstimatePrecision = 8

// Operand bounds. Only the final result of an expression is held to the
// stored value bounds (MaxDecimalCoefficientBits, MaxDecimalExponent), so
// without these an intermediate grows without limit: each Mul of a value with
// itself doubles its coefficient, and 17 of them bound with let produce a
// 7.6 million digit operand whose logarithm took over four minutes. Every node
// evaluates the same expression in the same round, so that stalls the DON.
//
// Checking every operand bounds the cost of each operation by the size of its
// inputs, and each operation at most roughly doubles that size, so growth stops
// at the next operation.
//
// Both leave room for the product of four stored values at their bounds.
const (
	maxOperandCoefficientBits = 4 * protocol.MaxDecimalCoefficientBits
	// The exponent is bounded separately because adding operands rescales the
	// one with the larger exponent: a value whose exponent has grown to -1e9
	// has a one bit coefficient but turns Add(x, 1) into a billion digit one.
	maxOperandExponent = 4 * protocol.MaxDecimalExponent
)

// checkOperand refuses an operand too large to compute with. See the operand
// bounds above.
func checkOperand(d decimal.Decimal) error {
	if bits := d.Coefficient().BitLen(); bits > maxOperandCoefficientBits {
		return fmt.Errorf("operand has a coefficient of %d bits, which exceeds the maximum of %d", bits, maxOperandCoefficientBits)
	}
	if exp := d.Exponent(); exp > maxOperandExponent || exp < -maxOperandExponent {
		return fmt.Errorf("operand has an exponent of %d, which exceeds the maximum magnitude of %d", exp, maxOperandExponent)
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
func exp(x decimal.Decimal) (decimal.Decimal, error) {
	return expDecimal(x, doublePrecision)
}

// maxExpArgument bounds the argument to exp.
//
// exp(x) has roughly x/ln(10) decimal digits, so this is the largest argument
// whose result is still within MaxDecimalExponent (1000) and therefore still
// storable: 1000 * ln(10) is about 2302, rounded up for headroom.
const maxExpArgument = 2400

// expDecimal returns e^x rounded to places decimal places, the contract of
// decimal.ExpTaylor, rounding a negative argument's result twice as it does.
//
// ExpTaylor sums the series on x itself, which takes about e*|x| terms whose
// numerators are kept exact, so its cost grows steeply with |x|: exp(1085) took
// seconds. Here the argument is halved k times until it is below 1/256, where
// the series converges in a few digits per term, and the sum is squared k
// times to undo the halving.
//
// All arithmetic is on integers scaled by 10^w and truncated, so the result is
// a function of the inputs alone. Squaring doubles the relative error, so w
// covers the digits of the result, the places asked for, k*log10(2) digits lost
// to squaring, and a guard for the truncation in each term and step. That keeps
// the error several orders below the last place, so rounding only differs from
// the exact value's on a near tie.
//
// The argument is bounded because the result is not: exp(1e6) has ~434,000
// digits. Callers pass logarithms of bounded operands, which checkPow and
// MaxDecimalExponent keep within about ±2400.
func expDecimal(x decimal.Decimal, places int32) (decimal.Decimal, error) {
	if places < 0 {
		return decimal.Decimal{}, fmt.Errorf("exponential precision %d must not be negative", places)
	}
	if x.Abs().GreaterThan(decimal.NewFromInt(maxExpArgument)) {
		return decimal.Decimal{}, fmt.Errorf("exponential argument %s exceeds the maximum magnitude of %d", x, maxExpArgument)
	}
	if x.IsZero() {
		return decimal.New(1, 0).Round(places), nil
	}

	a := x.Abs()
	k := a.Ceil().BigInt().BitLen() + 8
	// 0.4343 is just above 1/ln(10).
	resultDigits := a.Mul(decimal.RequireFromString("0.4343")).Ceil().IntPart() + 1
	w := int32(places) + int32(resultDigits) + int32(k)/3 + 1 + expGuardDigits
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(w)), nil)

	// r = a / 2^k, as an integer scaled by 10^w.
	r := a.DivRound(decimal.NewFromBigInt(new(big.Int).Lsh(big.NewInt(1), uint(k)), 0), w).Shift(w).BigInt()

	sum := new(big.Int).Set(scale)
	term := new(big.Int).Set(scale)
	divisor := new(big.Int)
	for i := int64(1); ; i++ {
		term.Mul(term, r)
		term.Quo(term, divisor.Mul(scale, big.NewInt(i)))
		if term.Sign() == 0 {
			break
		}
		sum.Add(sum, term)
	}
	for range k {
		sum.Mul(sum, sum)
		sum.Quo(sum, scale)
	}

	result := decimal.NewFromBigInt(sum, -w)
	if x.IsNegative() {
		return decimal.New(1, 0).DivRound(result, places+1).Round(places), nil
	}
	return result.Round(places), nil
}

// expGuardDigits covers the truncation error of expDecimal: one unit in the last
// working place per series term and per squaring, under a thousand of each.
const expGuardDigits = 10

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
