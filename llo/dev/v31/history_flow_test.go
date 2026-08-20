package llo

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// historyExprChannel is an expression channel reading a window of stream 100.
func historyExprChannel(expression string) llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
		Opts:         []byte(fmt.Sprintf(`{"abi":[{"type":"int256","expression":%q,"expressionStreamID":999}]}`, expression)),
	}
}

// valueRound feeds 4 identical observations carrying a value for stream 100.
func valueRound(t *testing.T, ts uint64, value int64) []ocrtypes.AttributedObservation {
	t.Helper()
	obs := Observation{
		UnixTimestampNanoseconds: ts,
		StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(value))},
	}
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := range 4 {
		aos = append(aos, ao(i, mustEncodeObs(t, obs)))
	}
	return aos
}

// historyPlugin returns a plugin whose only channel reads stream history.
func historyPlugin(t *testing.T, expression string) *Plugin {
	t.Helper()
	p := testPlugin(t)
	p.ChannelDefinitionCache = &mockChannelDefinitionCache{defs: llotypes.ChannelDefinitions{1: historyExprChannel(expression)}}
	return p
}

// bootstrapHistoryChannel brings up the plugin and installs the channel.
func bootstrapHistoryChannel(t *testing.T, p *Plugin, kv *memKV, expression string) {
	t.Helper()
	ctx := tests.Context(t)
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1_000, 1, historyExprChannel(expression)), kv, nil)
	require.NoError(t, err)
	require.Contains(t, storedChannelDefinitions(t, kv), llotypes.ChannelID(1))
}

// Test_History_Warmup is the central behaviour of the feature: a channel reading
// a window of depth N produces nothing, and does not advance its coverage
// watermark, until the window is N deep — then it reports on exactly that round.
func Test_History_Warmup(t *testing.T) {
	ctx := tests.Context(t)
	const depth = 3

	p := historyPlugin(t, fmt.Sprintf("Count(History(s100, %d))", depth))
	kv := newMemKV()
	bootstrapHistoryChannel(t, p, kv, fmt.Sprintf("Count(History(s100, %d))", depth))

	key := histKey{streamID: 100, aggregator: llotypes.AggregatorMedian}
	seqNr := uint64(3)

	var validAfterWhileWarming uint64
	for round := 1; round <= depth; round++ {
		ts := uint64(round) * 10_000
		_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, ts, int64(round)), kv, nil)
		require.NoError(t, err)
		seqNr++

		stored := readHistory(t, kv, key.streamID, key.aggregator)
		require.NotNil(t, stored, "round %d: a required pair must have a window", round)
		assert.Equal(t, round, stored.Len(), "round %d: one record per round", round)
		assert.Equal(t, uint32(depth), stored.RequiredCount())

		validAfter := storedValidAfter(t, kv, 1)

		if round < depth {
			// Not deep enough: no value, so nothing to report.
			require.False(t, reportedFlag(t, kv, 1), "round %d: must not be reportable while warming up", round)
			if round == 1 {
				validAfterWhileWarming = validAfter
			} else {
				assert.Equal(t, validAfterWhileWarming, validAfter,
					"round %d: validAfter must not advance over rounds that emitted nothing", round)
			}
		} else {
			// The window is now exactly deep enough.
			require.True(t, reportedFlag(t, kv, 1), "round %d: must become reportable once the window is deep enough", round)
		}
	}
}

// Test_History_EvictsAtDepth checks the window stays bounded across many rounds
// and keeps the newest values.
func Test_History_EvictsAtDepth(t *testing.T) {
	ctx := tests.Context(t)
	const depth = 2
	expression := fmt.Sprintf("Count(History(s100, %d))", depth)

	p := historyPlugin(t, expression)
	kv := newMemKV()
	bootstrapHistoryChannel(t, p, kv, expression)

	seqNr := uint64(3)
	for round := 1; round <= 6; round++ {
		_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, uint64(round)*10_000, int64(round)), kv, nil)
		require.NoError(t, err)
		seqNr++
	}

	stored := readHistory(t, kv, 100, llotypes.AggregatorMedian)
	requireHistoryRetention(t, stored)
	assert.Equal(t, uint64(6*10_000), stored.LastObservationTimestampNanoseconds())

	// What the expression sees is the newest `depth` records, whatever the
	// window retains around them.
	window := readHistoryNewest(t, kv, 100, llotypes.AggregatorMedian, depth)
	assert.Equal(t, []uint64{5 * 10_000, 6 * 10_000}, historyTimestamps(window))
	newest, ok := window[depth-1].Value.(*protocol.Decimal)
	require.True(t, ok)
	assert.True(t, decimal.NewFromInt(6).Equal(newest.Decimal()))
}

// Test_History_NoDuplicateOnStalledTimestamp covers the rule that keeps a window
// from double counting: if the round's observation timestamp does not advance,
// nothing is appended.
func Test_History_NoDuplicateOnStalledTimestamp(t *testing.T) {
	ctx := tests.Context(t)
	expression := "Count(History(s100, 5))"

	p := historyPlugin(t, expression)
	kv := newMemKV()
	bootstrapHistoryChannel(t, p, kv, expression)

	seqNr := uint64(3)
	_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, 10_000, 1), kv, nil)
	require.NoError(t, err)
	seqNr++

	stored := readHistory(t, kv, 100, llotypes.AggregatorMedian)
	require.Equal(t, 1, stored.Len())

	// Same observation timestamp, different value: must not be appended.
	_, err = p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, 10_000, 99), kv, nil)
	require.NoError(t, err)

	stored = readHistory(t, kv, 100, llotypes.AggregatorMedian)
	assert.Equal(t, 1, stored.Len(), "a non-advancing observation timestamp must not append")
}

// Test_History_ReclaimedOnChannelRemoval checks the window does not outlive the
// channel that needed it.
func Test_History_ReclaimedOnChannelRemoval(t *testing.T) {
	ctx := tests.Context(t)
	expression := "Count(History(s100, 3))"

	p := historyPlugin(t, expression)
	kv := newMemKV()
	bootstrapHistoryChannel(t, p, kv, expression)

	seqNr := uint64(3)
	_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, 10_000, 1), kv, nil)
	require.NoError(t, err)
	seqNr++

	require.NotEmpty(t, kv.m[string(historyHeaderKey(100, llotypes.AggregatorMedian))])
	require.NotEmpty(t, kv.m[string(keyHistoryIndex)])

	// Vote the channel away.
	removeObs := Observation{UnixTimestampNanoseconds: 20_000, RemoveChannelIDs: map[llotypes.ChannelID]struct{}{1: {}}}
	removeAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := range 4 {
		removeAOs = append(removeAOs, ao(i, mustEncodeObs(t, removeObs)))
	}
	p.ChannelDefinitionCache = &mockChannelDefinitionCache{defs: llotypes.ChannelDefinitions{}}
	_, err = p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, removeAOs, kv, nil)
	require.NoError(t, err)
	seqNr++

	// The removal round still runs against the effective definitions, which
	// include the channel being voted away, so its requirement — and therefore
	// its window — survives this round. Reclaim happens on the next one, the
	// same one-round lag the carry-forward aggregates have.
	require.NotNil(t, readHistory(t, kv, 100, llotypes.AggregatorMedian),
		"the removal round still evaluates the channel, so its window must survive it")

	_, err = p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, 30_000, 2), kv, nil)
	require.NoError(t, err)

	stored := readHistory(t, kv, 100, llotypes.AggregatorMedian)
	assert.Nil(t, stored, "history must not outlive the channel that required it")

	idx, err := readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Empty(t, idx)
}

// Test_History_DeterministicAcrossOracles is the property a divergence would
// halt the DON over: two independent oracles processing identical rounds must
// end with byte-identical state.
func Test_History_DeterministicAcrossOracles(t *testing.T) {
	ctx := tests.Context(t)
	expression := "Count(History(s100, 3))"

	run := func() map[string][]byte {
		p := historyPlugin(t, expression)
		kv := newMemKV()
		bootstrapHistoryChannel(t, p, kv, expression)
		seqNr := uint64(3)
		for round := 1; round <= 5; round++ {
			_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, valueRound(t, uint64(round)*10_000, int64(round)), kv, nil)
			require.NoError(t, err)
			seqNr++
		}
		return kv.m
	}

	a, b := run(), run()
	require.Equal(t, len(a), len(b))
	for key, want := range a {
		require.Equal(t, want, b[key], "key %q diverged", key)
	}
}

// Test_History_UnavailableOnDeniedPair checks a pair denied by the caps yields no
// value, so the channel does not report — rather than silently evaluating over a
// shallower window.
func Test_History_UnavailableOnDeniedPair(t *testing.T) {
	t.Parallel()

	// Requirements computed with the byte budget already exhausted by
	// lower-numbered pairs.
	required := map[histKey]uint32{}
	for i := range protocol.MaxHistoryPairs + 1 {
		required[histKey{streamID: llotypes.StreamID(i + 1), aggregator: llotypes.AggregatorMedian}] = 1
	}
	got := admitHistoryRequirements(required, logger.Test(t))
	require.NotEmpty(t, got.denied)

	denied := got.sortedDenied()[0]
	assert.False(t, got.requires(denied.streamID, denied.aggregator),
		"a denied pair must not be appended to")

	// And the store never gets a capacity for it, so reads report insufficient
	// history rather than an empty window.
	kv := newCountingKV()
	store := newTestHistoryStore(t, kv)
	require.NoError(t, got.apply(store))
	_, err := store.Series(denied.streamID, denied.aggregator, 1, 0)
	require.ErrorIs(t, err, protocol.ErrInsufficientStreamHistory)
}

// reportedFlag reads the persisted reportability bit. The key is written only
// when the decision changes, so an absent key means "not reportable".
// storedChannelDefinitions decodes the c/defs record.
func storedChannelDefinitions(t *testing.T, kv *memKV) llotypes.ChannelDefinitions {
	t.Helper()
	defs, err := readChannelState(kv)
	require.NoError(t, err)
	return defs
}

// storedHotState decodes the r/agg record.
func storedHotState(t *testing.T, kv *memKV) *kvState {
	t.Helper()
	s := &kvState{
		validAfterNanoseconds: map[llotypes.ChannelID]uint64{},
		reportedLastRound:     map[llotypes.ChannelID]bool{},
		carryForward:          map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{},
	}
	require.NoError(t, readHotState(kv, s))
	return s
}

// storedValidAfter returns a channel's persisted coverage watermark.
func storedValidAfter(t *testing.T, kv *memKV, cid llotypes.ChannelID) uint64 {
	t.Helper()
	return storedHotState(t, kv).validAfterNanoseconds[cid]
}

// reportedFlag returns the reportability decision the last round persisted.
func reportedFlag(t *testing.T, kv *memKV, cid llotypes.ChannelID) bool {
	t.Helper()
	return storedHotState(t, kv).reportedLastRound[cid]
}
