package calculated

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seriesOf builds a window from integer-valued strings, one second apart.
func seriesOf(t *testing.T, values ...string) Series {
	t.Helper()
	decimals := make([]decimal.Decimal, 0, len(values))
	timestamps := make([]uint64, 0, len(values))
	for i, v := range values {
		d, err := decimal.NewFromString(v)
		require.NoError(t, err)
		decimals = append(decimals, d)
		timestamps = append(timestamps, uint64(i+1)*1_000_000_000)
	}
	s, err := NewSeries(decimals, timestamps)
	require.NoError(t, err)
	return s
}

func assertDecimal(t *testing.T, want string, got decimal.Decimal) {
	t.Helper()
	expected, err := decimal.NewFromString(want)
	require.NoError(t, err)
	assert.True(t, expected.Equal(got), "expected %s, got %s", want, got.String())
}

// TestSeriesFunctions_Golden pins each function against hand-computed values.
func TestSeriesFunctions_Golden(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		fn     func(any) (decimal.Decimal, error)
		values []string
		want   string
	}{
		"Count":                {Count, []string{"1", "2", "3", "4"}, "4"},
		"First":                {First, []string{"10", "20", "30"}, "10"},
		"Last":                 {Last, []string{"10", "20", "30"}, "30"},
		"Median odd":           {Median, []string{"30", "10", "20"}, "20"},
		"Median even":          {Median, []string{"40", "10", "30", "20"}, "25"},
		"Median single":        {Median, []string{"7"}, "7"},
		"Median even fraction": {Median, []string{"1", "2"}, "1.5"},
		"Delta":                {Delta, []string{"10", "50", "30"}, "20"},
		"Delta negative":       {Delta, []string{"30", "10"}, "-20"},
		"Spread":               {Spread, []string{"10", "50", "30"}, "40"},
		"Spread flat":          {Spread, []string{"5", "5", "5"}, "0"},
		"PctChange up":         {PctChange, []string{"100", "125"}, "0.25"},
		"PctChange down":       {PctChange, []string{"100", "75"}, "-0.25"},
		"Variance":             {Variance, []string{"2", "4", "4", "4", "5", "5", "7", "9"}, "4"},
		"Variance flat":        {Variance, []string{"3", "3", "3"}, "0"},
		"Stddev":               {Stddev, []string{"2", "4", "4", "4", "5", "5", "7", "9"}, "2"},
		"Stddev single":        {Stddev, []string{"9"}, "0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.fn(seriesOf(t, tc.values...))
			require.NoError(t, err)
			assertDecimal(t, tc.want, got)
		})
	}
}

func TestSeriesFunctions_Errors(t *testing.T) {
	t.Parallel()

	windowOnly := map[string]func(any) (decimal.Decimal, error){
		"Count": Count, "First": First, "Last": Last, "Median": Median,
		"Variance": Variance, "Stddev": Stddev, "Delta": Delta,
		"PctChange": PctChange, "Spread": Spread,
	}
	for name, fn := range windowOnly {
		t.Run(name+" rejects a scalar", func(t *testing.T) {
			t.Parallel()
			_, err := fn(decimal.NewFromInt(1))
			require.Error(t, err)
		})
		t.Run(name+" rejects an empty window", func(t *testing.T) {
			t.Parallel()
			_, err := fn(Series{})
			require.Error(t, err)
		})
	}

	t.Run("PctChange from zero is undefined", func(t *testing.T) {
		t.Parallel()
		_, err := PctChange(seriesOf(t, "0", "10"))
		require.ErrorContains(t, err, "oldest value is zero")
	})
}

// TestAggregateFunctions_AcceptWindowOrScalars covers the functions that take
// either form, including that the two cannot be mixed.
func TestAggregateFunctions_AcceptWindowOrScalars(t *testing.T) {
	t.Parallel()

	for name, fn := range map[string]func(...any) (decimal.Decimal, error){
		"Avg": Avg, "Sum": Sum, "Min": Min, "Max": Max,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fromWindow, err := fn(seriesOf(t, "10", "20", "30"))
			require.NoError(t, err)
			fromScalars, err := fn(10, 20, 30)
			require.NoError(t, err)
			assert.True(t, fromWindow.Equal(fromScalars),
				"window and scalar forms must agree: %s vs %s", fromWindow, fromScalars)

			// Mixing the forms is ambiguous: is the scalar another sample or a
			// weight? Rejected rather than given a meaning.
			_, err = fn(seriesOf(t, "10"), 20)
			require.ErrorContains(t, err, "not both")

			_, err = fn()
			require.Error(t, err)

			_, err = fn(Series{})
			require.ErrorContains(t, err, "empty")
		})
	}

	avg, err := Avg(seriesOf(t, "10", "20", "30"))
	require.NoError(t, err)
	assertDecimal(t, "20", avg)

	sum, err := Sum(seriesOf(t, "10", "20", "30"))
	require.NoError(t, err)
	assertDecimal(t, "60", sum)

	min, err := Min(seriesOf(t, "30", "10", "20"))
	require.NoError(t, err)
	assertDecimal(t, "10", min)

	max, err := Max(seriesOf(t, "30", "10", "20"))
	require.NoError(t, err)
	assertDecimal(t, "30", max)
}
