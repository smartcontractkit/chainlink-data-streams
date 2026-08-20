package calculated

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalysisCache_HitReturnsSameResult(t *testing.T) {
	t.Parallel()

	c := newAnalysisCache(8)
	const expression = "Avg(History(s10001, 10))"

	first, err := c.analyze(expression)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, 1, c.len())

	second, err := c.analyze(expression)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, c.len(), "a repeated expression must not add an entry")
}

// TestAnalysisCache_CachesRejections keeps a bad expression from being
// re-analyzed every round for as long as it stays in the configuration.
func TestAnalysisCache_CachesRejections(t *testing.T) {
	t.Parallel()

	c := newAnalysisCache(8)
	const expression = "Add(History(s1, 10), 2)"

	refs, err := c.analyze(expression)
	require.ErrorIs(t, err, ErrHistoryExpression)
	assert.Nil(t, refs)
	assert.Equal(t, 1, c.len())

	refs, again := c.analyze(expression)
	require.ErrorIs(t, again, ErrHistoryExpression)
	assert.Nil(t, refs)
	assert.Equal(t, err.Error(), again.Error())
	assert.Equal(t, 1, c.len())
}

// TestAnalysisCache_BoundedByEviction is the property that matters
// operationally: channel churn must not grow the cache without limit.
func TestAnalysisCache_BoundedByEviction(t *testing.T) {
	t.Parallel()

	const max = 4
	c := newAnalysisCache(max)

	for i := range 100 {
		_, err := c.analyze(fmt.Sprintf("Avg(History(s%d, 10))", i+1))
		require.NoError(t, err)
		assert.LessOrEqual(t, c.len(), max)
	}
	assert.Equal(t, max, c.len())
}

func TestAnalysisCache_EvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	c := newAnalysisCache(2)
	a := "Avg(History(s1, 10))"
	b := "Avg(History(s2, 10))"
	d := "Avg(History(s3, 10))"

	_, err := c.analyze(a)
	require.NoError(t, err)
	_, err = c.analyze(b)
	require.NoError(t, err)

	// Touch a so b becomes the least recently used.
	_, err = c.analyze(a)
	require.NoError(t, err)

	_, err = c.analyze(d)
	require.NoError(t, err)

	require.Equal(t, 2, c.len())
	_, ok := c.get(a)
	assert.True(t, ok, "recently used entry must survive")
	_, ok = c.get(b)
	assert.False(t, ok, "least recently used entry must be evicted")
	_, ok = c.get(d)
	assert.True(t, ok)
}

// TestAnalysisCache_Concurrent checks the cache is safe under concurrent use;
// the plugin and configuration tooling can both reach it.
func TestAnalysisCache_Concurrent(t *testing.T) {
	t.Parallel()

	c := newAnalysisCache(16)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			expression := fmt.Sprintf("Avg(History(s%d, 10))", i%8+1)
			refs, err := c.analyze(expression)
			assert.NoError(t, err)
			assert.Len(t, refs, 1)
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, c.len(), 16)
}

func TestAnalyzeExpressionHistory(t *testing.T) {
	t.Parallel()

	refs, err := AnalyzeExpressionHistory("Avg(History(s424242, 7))")
	require.NoError(t, err)
	assert.Equal(t, []HistoryRef{{StreamID: 424242, Field: FieldValue, Count: 7}}, refs)

	// Cached results are shared, so repeated calls agree exactly.
	again, err := AnalyzeExpressionHistory("Avg(History(s424242, 7))")
	require.NoError(t, err)
	assert.Equal(t, refs, again)

	_, err = AnalyzeExpressionHistory("Add(History(s424243, 7), 1)")
	require.ErrorIs(t, err, ErrHistoryExpression)
}
