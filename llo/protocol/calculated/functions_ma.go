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
