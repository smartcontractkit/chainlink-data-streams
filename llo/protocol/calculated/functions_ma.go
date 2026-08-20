package calculated

import (
	"github.com/shopspring/decimal"
)

// SMA returns the simple moving average of the newest n values of a history
// window.
func SMA(x any, n any) (decimal.Decimal, error) {
	series, err := window("SMA", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	count, err := windowSize("SMA", series, n)
	if err != nil {
		return decimal.Decimal{}, err
	}

	values := series.Values()
	sum := decimal.Zero
	for _, v := range values[len(values)-count:] {
		sum = sum.Add(v)
	}
	return divRoundByInt(sum, count, precision)
}

// WMA returns the linearly weighted moving average of the newest n values of a
// history window, weighting the newest value n and the oldest of the n values 1.
//
//	WMA = (n*x[newest] + (n-1)*x[newest-1] + ... + 1*x[oldest]) / (n + (n-1) + ... + 1)
func WMA(x any, n any) (decimal.Decimal, error) {
	series, err := window("WMA", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	count, err := windowSize("WMA", series, n)
	if err != nil {
		return decimal.Decimal{}, err
	}

	values := series.Values()
	considered := values[len(values)-count:]

	weighted := decimal.Zero
	for i, v := range considered {
		// i counts from the oldest of the considered values, so its weight is
		// i+1 and the newest gets count.
		weighted = weighted.Add(v.Mul(decimal.NewFromInt(int64(i + 1))))
	}
	// Sum of 1..count, computed exactly rather than accumulated.
	totalWeight := count * (count + 1) / 2
	return divRoundByInt(weighted, totalWeight, precision)
}

// EMA returns the exponential moving average of a history window with smoothing
// period n.
//
// The series is seeded with the simple mean of its oldest n values, then
// iterated newest-ward with the conventional smoothing factor:
//
//	alpha = 2 / (n + 1)
//	ema   = value*alpha + ema*(1 - alpha)
//
// Determinism note: the recursive form is path-dependent, so the seed rule, the
// iteration count and the rounding must all be fixed. They are: the window
// length is exactly the depth the expression requested, the channel does not
// evaluate until the window is that deep, alpha is an exact decimal fraction
// rather than a float, and every step is rounded to the package precision. That
// is what makes the result reproducible across oracles and across restarts.
func EMA(x any, n any) (decimal.Decimal, error) {
	series, err := window("EMA", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	count, err := windowSize("EMA", series, n)
	if err != nil {
		return decimal.Decimal{}, err
	}

	values := series.Values()

	// Seed: mean of the oldest count values.
	seedSum := decimal.Zero
	for _, v := range values[:count] {
		seedSum = seedSum.Add(v)
	}
	ema, err := divRoundByInt(seedSum, count, precision)
	if err != nil {
		return decimal.Decimal{}, err
	}

	alpha, err := divRoundByInt(decimal.NewFromInt(2), count+1, doublePrecision)
	if err != nil {
		return decimal.Decimal{}, err
	}
	oneMinusAlpha := decimal.NewFromInt(1).Sub(alpha)

	for _, v := range values[count:] {
		ema = v.Mul(alpha).Add(ema.Mul(oneMinusAlpha)).Round(precision)
	}
	return ema, nil
}
