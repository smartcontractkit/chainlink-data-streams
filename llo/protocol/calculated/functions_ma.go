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
