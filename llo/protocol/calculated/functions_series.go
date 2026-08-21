package calculated

import (
	"fmt"
	"sort"

	"github.com/shopspring/decimal"
)

// Sum returns the total of a history window or a list of scalars.
//
// Add remains the binary form; Sum is the aggregate one.
func Sum(x ...any) (decimal.Decimal, error) {
	values, err := scalarsOrWindow("Sum", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	total := decimal.Zero
	for _, v := range values {
		total = total.Add(v)
	}
	return total, nil
}

// First returns the oldest value in a history window.
func First(x any) (decimal.Decimal, error) {
	series, err := window("First", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return series.Oldest()
}

// Last returns the newest value in a history window, which is the value the
// current round agreed on.
func Last(x any) (decimal.Decimal, error) {
	series, err := window("Last", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return series.Newest()
}

// Median returns the middle value of a history window: the exact middle for an
// odd length, the mean of the two middle values for an even one.
func Median(x any) (decimal.Decimal, error) {
	series, err := window("Median", x)
	if err != nil {
		return decimal.Decimal{}, err
	}

	// Sorted on a copy: the window is shared with the environment and with any
	// other function reading it this round.
	sorted := make([]decimal.Decimal, len(series.Values()))
	copy(sorted, series.Values())
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].LessThan(sorted[j]) })

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid], nil
	}
	return divRoundByInt(sorted[mid-1].Add(sorted[mid]), 2, precision)
}

// Variance returns the population variance of a history window.
//
// Population rather than sample variance: the window is the whole series being
// described, not a sample drawn from a larger one.
func Variance(x any) (decimal.Decimal, error) {
	series, err := window("Variance", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	values := series.Values()

	// The mean is taken at double precision so that squaring it does not
	// amplify the rounding of an intermediate value.
	sum := decimal.Zero
	for _, v := range values {
		sum = sum.Add(v)
	}
	mean, err := divRoundByInt(sum, len(values), doublePrecision)
	if err != nil {
		return decimal.Decimal{}, err
	}

	squares := decimal.Zero
	for _, v := range values {
		diff := v.Sub(mean)
		squares = squares.Add(diff.Mul(diff))
	}
	return divRoundByInt(squares, len(values), precision)
}

// Stddev returns the population standard deviation of a history window.
func Stddev(x any) (decimal.Decimal, error) {
	variance, err := Variance(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return sqrt(variance)
}

// Delta returns the change across a history window: newest minus oldest.
func Delta(x any) (decimal.Decimal, error) {
	series, err := window("Delta", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	newest, err := series.Newest()
	if err != nil {
		return decimal.Decimal{}, err
	}
	oldest, err := series.Oldest()
	if err != nil {
		return decimal.Decimal{}, err
	}
	return newest.Sub(oldest), nil
}

// PctChange returns the fractional change across a history window,
// (newest - oldest) / oldest. It is a fraction, not a percentage: 0.05 is a 5%
// rise.
func PctChange(x any) (decimal.Decimal, error) {
	series, err := window("PctChange", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	newest, err := series.Newest()
	if err != nil {
		return decimal.Decimal{}, err
	}
	oldest, err := series.Oldest()
	if err != nil {
		return decimal.Decimal{}, err
	}
	if oldest.IsZero() {
		return decimal.Decimal{}, fmt.Errorf("PctChange: oldest value is zero, relative change is undefined")
	}
	return divRound(newest.Sub(oldest), oldest, precision)
}

// Spread returns the range of a history window: maximum minus minimum.
func Spread(x any) (decimal.Decimal, error) {
	series, err := window("Spread", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	values := series.Values()
	return decimal.Max(values[0], values[1:]...).Sub(decimal.Min(values[0], values[1:]...)), nil
}
