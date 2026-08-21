package llo

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

func exprChannel(streams []llotypes.Stream, expressions ...string) llotypes.ChannelDefinition {
	abi := ""
	for i, expression := range expressions {
		if i > 0 {
			abi += ","
		}
		abi += fmt.Sprintf(`{"type":"int256","expression":%q,"expressionStreamID":%d}`, expression, 900+i)
	}
	return llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Streams:      streams,
		Opts:         []byte(fmt.Sprintf(`{"abi":[%s]}`, abi)),
	}
}

func medianStream(id llotypes.StreamID) llotypes.Stream {
	return llotypes.Stream{StreamID: id, Aggregator: llotypes.AggregatorMedian}
}

func requirementsFor(t *testing.T, defs llotypes.ChannelDefinitions) historyRequirements {
	t.Helper()
	cache := protocol.NewOptsCache()
	cache.ResetTo(defs)
	return computeHistoryRequirements(defs, cache, logger.Test(t))
}

func TestComputeHistoryRequirements(t *testing.T) {
	t.Parallel()

	t.Run("no expressions using history", func(t *testing.T) {
		t.Parallel()
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1), medianStream(2)}, "Add(s1, s2)"),
		})
		assert.Empty(t, got.depths)
		assert.Empty(t, got.denied)
	})

	t.Run("single window", func(t *testing.T) {
		t.Parallel()
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s1, 10))"),
		})
		assert.Equal(t, map[histKey]uint32{
			{streamID: 1, aggregator: llotypes.AggregatorMedian}: 10,
		}, got.depths)
	})

	t.Run("depth is the maximum across channels", func(t *testing.T) {
		t.Parallel()
		// Two channels want different depths of the same pair; the deeper one
		// wins, and the shallower reads a prefix of the same window.
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s1, 10))"),
			2: exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s1, 300))"),
			3: exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s1, 50))"),
		})
		assert.Equal(t, map[histKey]uint32{
			{streamID: 1, aggregator: llotypes.AggregatorMedian}: 300,
		}, got.depths)
	})

	t.Run("depth is the maximum across fields of one pair", func(t *testing.T) {
		t.Parallel()
		// The field is not part of the key: one window serves all of them, so
		// the depth must cover the deepest field request.
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1)},
				"Add(Count(History(s1_bid, 10)), Count(History(s1_ask, 40)))"),
		})
		assert.Equal(t, map[histKey]uint32{
			{streamID: 1, aggregator: llotypes.AggregatorMedian}: 40,
		}, got.depths)
	})

	t.Run("depth is the maximum across expressions of one channel", func(t *testing.T) {
		t.Parallel()
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1)},
				"Count(History(s1, 5))", "Count(History(s1, 25))"),
		})
		assert.Equal(t, map[histKey]uint32{
			{streamID: 1, aggregator: llotypes.AggregatorMedian}: 25,
		}, got.depths)
	})

	t.Run("aggregator is part of the identity", func(t *testing.T) {
		t.Parallel()
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s1, 10))"),
			2: exprChannel([]llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMode}}, "Count(History(s1, 20))"),
		})
		assert.Equal(t, map[histKey]uint32{
			{streamID: 1, aggregator: llotypes.AggregatorMedian}: 10,
			{streamID: 1, aggregator: llotypes.AggregatorMode}:   20,
		}, got.depths)
	})

	t.Run("tombstoned and non-expression channels contribute nothing", func(t *testing.T) {
		t.Parallel()
		tombstoned := exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s1, 10))")
		tombstoned.Tombstone = true
		other := exprChannel([]llotypes.Stream{medianStream(2)}, "Count(History(s2, 10))")
		other.ReportFormat = llotypes.ReportFormatJSON

		got := requirementsFor(t, llotypes.ChannelDefinitions{1: tombstoned, 2: other})
		assert.Empty(t, got.depths)
	})

	t.Run("ambiguous aggregation contributes nothing", func(t *testing.T) {
		t.Parallel()
		// The channel cannot be evaluated either, so reserving history for it
		// would keep a window alive that nothing can read.
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{
				medianStream(1),
				{StreamID: 1, Aggregator: llotypes.AggregatorMode},
			}, "Count(History(s1, 10))"),
		})
		assert.Empty(t, got.depths)
	})

	t.Run("invalid expression contributes nothing", func(t *testing.T) {
		t.Parallel()
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1)}, "Add(History(s1, 10), 1)"),
		})
		assert.Empty(t, got.depths)
	})

	t.Run("undeclared stream contributes nothing", func(t *testing.T) {
		t.Parallel()
		got := requirementsFor(t, llotypes.ChannelDefinitions{
			1: exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s2, 10))"),
		})
		assert.Empty(t, got.depths)
	})

	t.Run("undecodable opts contribute nothing", func(t *testing.T) {
		t.Parallel()
		cd := exprChannel([]llotypes.Stream{medianStream(1)}, "Count(History(s1, 10))")
		cd.Opts = []byte(`{"abi":`)
		got := requirementsFor(t, llotypes.ChannelDefinitions{1: cd})
		assert.Empty(t, got.depths)
	})

	// Determinism is the property the whole design rests on: these depths become
	// persisted state, so two oracles must derive them identically.
	t.Run("deterministic across repeated computation", func(t *testing.T) {
		t.Parallel()
		defs := llotypes.ChannelDefinitions{}
		for i := range 40 {
			id := llotypes.ChannelID(i + 1)
			streamID := llotypes.StreamID(i%7 + 1)
			defs[id] = exprChannel([]llotypes.Stream{medianStream(streamID)},
				fmt.Sprintf("Count(History(s%d, %d))", streamID, i+1))
		}
		first := requirementsFor(t, defs)
		for range 20 {
			require.Equal(t, first.depths, requirementsFor(t, defs).depths)
		}
	})
}

func TestAdmitHistoryRequirements(t *testing.T) {
	t.Parallel()

	t.Run("admits within the caps", func(t *testing.T) {
		t.Parallel()
		got := admitHistoryRequirements(map[histKey]uint32{
			{streamID: 1, aggregator: llotypes.AggregatorMedian}: 10,
			{streamID: 2, aggregator: llotypes.AggregatorMedian}: 20,
		}, logger.Test(t))
		assert.Len(t, got.depths, 2)
		assert.Empty(t, got.denied)
	})

	t.Run("denies pairs beyond the pair cap, lowest keys first", func(t *testing.T) {
		t.Parallel()
		required := map[histKey]uint32{}
		for i := range protocol.MaxHistoryPairs + 5 {
			required[histKey{streamID: llotypes.StreamID(i + 1), aggregator: llotypes.AggregatorMedian}] = 1
		}
		got := admitHistoryRequirements(required, logger.Test(t))
		assert.Len(t, got.depths, protocol.MaxHistoryPairs)
		require.Len(t, got.denied, 5)
		// Admission is by (streamID, aggregator) order, so the denied set is the
		// tail. Any other rule risks differing between oracles.
		for i, key := range got.sortedDenied() {
			assert.Equal(t, llotypes.StreamID(protocol.MaxHistoryPairs+i+1), key.streamID)
		}
	})

	t.Run("depth does not affect admission", func(t *testing.T) {
		t.Parallel()
		// Under the chunked layout a pair costs the same per round however deep
		// it is, so a full-depth pair is admitted on exactly the same terms as a
		// shallow one.
		required := map[histKey]uint32{}
		for i := range protocol.MaxHistoryPairs {
			required[histKey{streamID: llotypes.StreamID(i + 1), aggregator: llotypes.AggregatorMedian}] = protocol.MaxHistoryRecordsPerPair
		}
		got := admitHistoryRequirements(required, logger.Test(t))
		assert.Len(t, got.depths, protocol.MaxHistoryPairs)
		assert.Empty(t, got.denied)
	})

	t.Run("denial is deterministic", func(t *testing.T) {
		t.Parallel()
		required := map[histKey]uint32{}
		for i := range protocol.MaxHistoryPairs + 10 {
			required[histKey{streamID: llotypes.StreamID(i + 1), aggregator: llotypes.AggregatorMedian}] = 8
		}
		first := admitHistoryRequirements(required, logger.Test(t))
		for range 20 {
			again := admitHistoryRequirements(required, logger.Test(t))
			require.Equal(t, first.depths, again.depths)
			require.Equal(t, first.sortedDenied(), again.sortedDenied())
		}
	})
}

func TestHistoryRequirements_Apply(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	key := histKey{streamID: 1, aggregator: llotypes.AggregatorMedian}

	// Round 1: the pair is required, so a window is stored.
	store := newTestHistoryStore(t, kv)
	require.NoError(t, historyRequirements{depths: map[histKey]uint32{key: 5}}.apply(store))
	_, err := store.Append(key.streamID, key.aggregator, 1_000, testDecimal(1))
	require.NoError(t, err)
	require.NoError(t, store.Flush(kv))

	stored := readHistory(t, kv, key.streamID, key.aggregator)
	require.NotNil(t, stored)

	// Round 2: the pair is no longer required. apply must clear it explicitly,
	// otherwise the window would linger forever.
	store = newTestHistoryStore(t, kv)
	require.NoError(t, historyRequirements{depths: map[histKey]uint32{}}.apply(store))
	require.NoError(t, store.Flush(kv))

	stored = readHistory(t, kv, key.streamID, key.aggregator)
	assert.Nil(t, stored, "a pair that stopped being required must be reclaimed")
}

func TestHistoryRequirements_Requires(t *testing.T) {
	t.Parallel()

	r := historyRequirements{depths: map[histKey]uint32{
		{streamID: 1, aggregator: llotypes.AggregatorMedian}: 5,
	}}
	assert.True(t, r.requires(1, llotypes.AggregatorMedian))
	assert.False(t, r.requires(1, llotypes.AggregatorMode), "the aggregator is part of the identity")
	assert.False(t, r.requires(2, llotypes.AggregatorMedian))

	// A pair denied by the caps is not required, so nothing is appended for it.
	denied := historyRequirements{depths: map[histKey]uint32{}, denied: []histKey{{streamID: 1, aggregator: llotypes.AggregatorMedian}}}
	assert.False(t, denied.requires(1, llotypes.AggregatorMedian))
}

// TestHistoryBudgetFitsPairCap is what lets admission apply the pair cap alone.
//
// A pair rewrites one chunk and one header per round whatever its depth, so if
// MaxHistoryPairs of them fit inside the per-round byte budget then the budget
// can never be the binding constraint and there is nothing for admission to
// check. Under the single-blob layout this did not hold — a pair cost
// requiredCount * MaxHistoryRecordBytes and the budget denied pairs well before
// the cap did.
func TestHistoryBudgetFitsPairCap(t *testing.T) {
	t.Parallel()

	worstCase := protocol.MaxHistoryPairs * protocol.MaxHistoryPairRoundBytes
	assert.LessOrEqual(t, worstCase, protocol.MaxHistoryTotalBytes,
		"admission relies on the pair cap alone, which requires every admitted pair to fit the byte budget")
}
