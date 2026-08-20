package calculated

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSMA(t *testing.T) {
	t.Parallel()

	window := seriesOf(t, "10", "20", "30", "40")

	// Newest 2: (30 + 40) / 2.
	got, err := SMA(window, 2)
	require.NoError(t, err)
	assertDecimal(t, "35", got)

	// The whole window matches Avg.
	got, err = SMA(window, 4)
	require.NoError(t, err)
	assertDecimal(t, "25", got)

	got, err = SMA(window, 1)
	require.NoError(t, err)
	assertDecimal(t, "40", got)
}

func TestWMA(t *testing.T) {
	t.Parallel()

	window := seriesOf(t, "10", "20", "30", "40")

	// Newest 3 with weights 1,2,3 oldest-to-newest: (1*20 + 2*30 + 3*40) / 6.
	// New functions round to precision (18), unlike Div and Avg which stay
	// pinned at 16 for backwards compatibility.
	got, err := WMA(window, 3)
	require.NoError(t, err)
	assertDecimal(t, "33.333333333333333333", got)

	// n=1 is just the newest value.
	got, err = WMA(window, 1)
	require.NoError(t, err)
	assertDecimal(t, "40", got)

	// A flat window averages to its value whatever the weights.
	got, err = WMA(seriesOf(t, "7", "7", "7"), 3)
	require.NoError(t, err)
	assertDecimal(t, "7", got)

	// Weighting is newest-heaviest, so a rising series exceeds the simple mean.
	sma, err := SMA(window, 4)
	require.NoError(t, err)
	wma, err := WMA(window, 4)
	require.NoError(t, err)
	assert.True(t, wma.GreaterThan(sma), "WMA %s should exceed SMA %s on a rising series", wma, sma)
}
