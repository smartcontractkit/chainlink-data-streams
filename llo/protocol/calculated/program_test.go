package calculated

import (
	"fmt"
	"sync"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
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

// TestAnalysisCache_PutKeepsCompiledProgram replays a cache-miss race: a late
// put for an already cached expression must not drop its compiled program.
func TestAnalysisCache_PutKeepsCompiledProgram(t *testing.T) {
	t.Parallel()

	c := newAnalysisCache(8)
	const expression = "Avg(History(s10001, 10))"

	first := c.entry(expression)
	program, err := expr.Compile("1")
	require.NoError(t, err)
	c.storeProgram(expression, program)

	late := &analysisEntry{expression: expression, refs: first.refs, twapCalls: first.twapCalls}
	assert.Same(t, first, c.put(late), "put must return the existing entry")
	assert.Same(t, program, c.program(expression))
	assert.Equal(t, 1, c.len())
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

// TestAnalysisCache_PinnedSurvivesChurn replays a peer flooding the cache with
// expressions it proposes: a pinned expression keeps its compiled program, and
// the unpinned entries stay bounded.
func TestAnalysisCache_PinnedSurvivesChurn(t *testing.T) {
	t.Parallel()

	const max = 4
	c := newAnalysisCache(max)
	const live = "Avg(History(s10001, 10))"

	c.pin([]string{live})
	program, err := expr.Compile("1")
	require.NoError(t, err)
	c.storeProgram(live, program)

	for i := range 100 {
		c.entry(fmt.Sprintf("Avg(History(s%d, 10))", i+1))
		assert.LessOrEqual(t, c.len(), max)
	}

	assert.Same(t, program, c.program(live), "pinned program must survive eviction")
	assert.Equal(t, 1, c.pinnedLen())
	assert.Equal(t, max, c.len())
}

// TestAnalysisCache_PinnedExceedsMax checks that a live expression set larger
// than the LRU bound keeps every compiled program through churn, since
// admission permits far more calculated streams than the bound.
func TestAnalysisCache_PinnedExceedsMax(t *testing.T) {
	t.Parallel()

	const max = 4
	c := newAnalysisCache(max)

	live := make([]string, 4*max)
	for i := range live {
		live[i] = fmt.Sprintf("Avg(History(s%d, 10))", 10001+i)
	}
	c.pin(live)

	programs := make([]*vm.Program, len(live))
	for i, expression := range live {
		program, err := expr.Compile("1")
		require.NoError(t, err)
		c.storeProgram(expression, program)
		programs[i] = program
	}

	for i := range 100 {
		c.entry(fmt.Sprintf("Avg(History(s%d, 10))", i+1))
	}

	for i, expression := range live {
		assert.Same(t, programs[i], c.program(expression), "pinned program must survive eviction: %s", expression)
	}
	assert.Equal(t, len(live), c.pinnedLen())
	assert.Equal(t, max, c.len())
}

// TestAnalysisCache_PinKeepsCachedProgram checks that pinning an expression
// already in the LRU moves its entry rather than analyzing it again.
func TestAnalysisCache_PinKeepsCachedProgram(t *testing.T) {
	t.Parallel()

	c := newAnalysisCache(8)
	const expression = "Avg(History(s10001, 10))"

	first := c.entry(expression)
	program, err := expr.Compile("1")
	require.NoError(t, err)
	c.storeProgram(expression, program)

	c.pin([]string{expression})
	assert.Same(t, first, c.entry(expression))
	assert.Same(t, program, c.program(expression))
	assert.Equal(t, 0, c.len(), "a pinned entry must leave the LRU")
	assert.Equal(t, 1, c.pinnedLen())
}

// TestAnalysisCache_UnpinCountsHolders checks that an expression stays pinned
// while any holder remains, and returns to the LRU once none does.
func TestAnalysisCache_UnpinCountsHolders(t *testing.T) {
	t.Parallel()

	c := newAnalysisCache(8)
	const expression = "Avg(History(s10001, 10))"

	c.pin([]string{expression})
	c.pin([]string{expression})
	pinned := c.entry(expression)

	c.unpin([]string{expression})
	assert.Equal(t, 1, c.pinnedLen())
	assert.Equal(t, 0, c.len())

	c.unpin([]string{expression})
	assert.Equal(t, 0, c.pinnedLen())
	assert.Equal(t, 1, c.len())
	assert.Same(t, pinned, c.entry(expression), "an unpinned entry must return to the LRU")

	// Unpinning what is not pinned is a no-op.
	c.unpin([]string{expression})
	assert.Equal(t, 1, c.len())
}

func TestExpressionPins(t *testing.T) {
	t.Parallel()

	channel := func(format llotypes.ReportFormat, expressions ...string) llotypes.ChannelDefinition {
		abi := ""
		for i, expression := range expressions {
			if i > 0 {
				abi += ","
			}
			abi += fmt.Sprintf(`{"type":"int256","expression":%q,"expressionStreamID":%d}`, expression, 900+i)
		}
		return llotypes.ChannelDefinition{
			ReportFormat: format,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			Opts:         []byte(fmt.Sprintf(`{"abi":[%s]}`, abi)),
		}
	}
	const (
		a = "Avg(History(s1, 10))"
		b = "Count(History(s1, 10))"
		d = "Sum(History(s1, 10))"
	)

	c := newAnalysisCache(2)
	first := &ExpressionPins{cache: c}
	second := &ExpressionPins{cache: c}

	tombstoned := channel(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, d)
	tombstoned.Tombstone = true
	first.Set(nil, llotypes.ChannelDefinitions{
		1: channel(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, a, b),
		2: channel(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, a),
		3: tombstoned,
		4: channel(llotypes.ReportFormatJSON, d),
	})
	assert.Equal(t, 2, c.pinnedLen(), "only distinct expressions of evaluated channels are pinned")

	second.Set(nil, llotypes.ChannelDefinitions{1: channel(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, a)})
	assert.Equal(t, 2, c.pinnedLen())

	// Dropping b from the first set unpins it, and a stays held by both.
	first.Set(nil, llotypes.ChannelDefinitions{1: channel(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, a)})
	assert.Equal(t, 1, c.pinnedLen())
	assert.Equal(t, 1, c.len())

	first.Release()
	assert.Equal(t, 1, c.pinnedLen(), "a must stay pinned while second holds it")
	second.Release()
	assert.Equal(t, 0, c.pinnedLen())

	var nilPins *ExpressionPins
	nilPins.Set(nil, llotypes.ChannelDefinitions{1: channel(llotypes.ReportFormatEVMABIEncodeUnpackedExpr, a)})
	nilPins.Release()
}
