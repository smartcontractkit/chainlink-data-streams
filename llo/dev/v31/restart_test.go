package llo

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// Test_Restart_WarmKVResumes checks that a restart which keeps the KV intact is
// invisible to the protocol: a plugin instance that replaces another mid warmup
// carries on from the persisted state instead of starting over.
func Test_Restart_WarmKVResumes(t *testing.T) {
	ctx := tests.Context(t)
	const depth = 3
	expression := fmt.Sprintf("Count(History(s100, %d))", depth)

	kv := newMemKV()
	before := historyPlugin(t, expression)
	bootstrapHistoryChannel(t, before, kv, expression)

	// Warm the window to one short of the required depth.
	seqNr := uint64(3)
	for round := 1; round < depth; round++ {
		_, err := before.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, uint64(round)*10_000, int64(round)), kv, testBlobs)
		require.NoError(t, err)
		seqNr++
	}
	require.False(t, reportedFlag(t, kv, 1), "must not be reportable while warming up")
	validAfterBefore := storedValidAfter(t, kv, 1)

	// Restart: a fresh instance with empty in-memory caches over the same KV.
	after := historyPlugin(t, expression)

	// The channel definitions are read back from KV, not re-voted.
	require.Contains(t, storedChannelDefinitions(t, kv), llotypes.ChannelID(1))

	// The round that completes the window still lands on schedule, which only
	// holds if the restarted instance saw the pre-restart records.
	_, err := after.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, uint64(depth)*10_000, depth), kv, testBlobs)
	require.NoError(t, err)

	stored := readHistory(t, kv, 100, llotypes.AggregatorMedian)
	require.NotNil(t, stored)
	assert.Equal(t, depth, stored.Len(), "the restart must not drop persisted records")
	assert.True(t, reportedFlag(t, kv, 1), "the window is deep enough, so the restarted instance must report")
	// Coverage advances on the round following the one that emitted, as it does
	// on an instance that never restarted.
	_, err = after.StateTransition(ctx, seqNr+1, ocrtypes.AttributedQuery{}, valueRound(t, uint64(depth+1)*10_000, depth+1), kv, testBlobs)
	require.NoError(t, err)
	assert.Greater(t, storedValidAfter(t, kv, 1), validAfterBefore, "coverage must advance once the channel is reporting")
}

// Test_Restart_ColdKVRebuilds checks the other half: a restart that also loses
// the KV comes up clean rather than wedged. Nothing is inherited, so the
// channel has to be re-voted and the window re-warmed from scratch.
func Test_Restart_ColdKVRebuilds(t *testing.T) {
	ctx := tests.Context(t)
	const depth = 3
	expression := fmt.Sprintf("Count(History(s100, %d))", depth)

	kv := newMemKV()
	before := historyPlugin(t, expression)
	bootstrapHistoryChannel(t, before, kv, expression)
	for round := 1; round <= depth; round++ {
		_, err := before.StateTransition(ctx, uint64(2+round), ocrtypes.AttributedQuery{}, valueRound(t, uint64(round)*10_000, int64(round)), kv, testBlobs)
		require.NoError(t, err)
	}
	require.True(t, reportedFlag(t, kv, 1))

	// Restart with a wiped store: new plugin, new KV.
	cold := newMemKV()
	after := historyPlugin(t, expression)

	require.Empty(t, storedChannelDefinitions(t, cold), "a wiped KV must not carry channels")
	require.Nil(t, readHistory(t, cold, 100, llotypes.AggregatorMedian), "a wiped KV must not carry history")

	// Values arriving before the channel is re-voted are tolerated and produce
	// nothing: there is no channel requiring the stream, so nothing is stored.
	_, err := after.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, valueRound(t, 10_000, 1), cold, testBlobs)
	require.NoError(t, err)
	require.Nil(t, readHistory(t, cold, 100, llotypes.AggregatorMedian))
	require.False(t, reportedFlag(t, cold, 1))

	// Re-vote the channel, then re-warm. Reportability returns only once the
	// window is deep again, exactly as on a first start.
	_, err = after.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 20_000, 1, historyExprChannel(expression)), cold, testBlobs)
	require.NoError(t, err)

	for round := 1; round <= depth; round++ {
		_, err := after.StateTransition(ctx, uint64(2+round), ocrtypes.AttributedQuery{}, valueRound(t, uint64(round+2)*10_000, int64(round)), cold, testBlobs)
		require.NoError(t, err)

		if round < depth {
			assert.False(t, reportedFlag(t, cold, 1), "round %d: must re-warm before reporting", round)
		} else {
			assert.True(t, reportedFlag(t, cold, 1), "round %d: must report once the window is deep again", round)
		}
	}

	stored := readHistory(t, cold, 100, llotypes.AggregatorMedian)
	require.NotNil(t, stored)
	assert.Equal(t, depth, stored.Len())
}
