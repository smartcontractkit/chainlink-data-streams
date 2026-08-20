package calculated

import (
	"testing"

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
