package llo

import (
	"encoding/binary"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

func Test_ChannelCache_ReadsDefsOnlyWhenSeqNrChanges(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	// Bootstrap + add channel 1 (both write c/defs, bumping c/seqnr).
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1_000, 1, jsonChannel()), kv, nil)
	require.NoError(t, err)

	// Round 3 still re-reads once, because round 2 changed the definitions.
	noopObs := Observation{UnixTimestampNanoseconds: 2_000}
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, noopObs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)

	before := kv.readCount(keyChannelState)

	// Rounds that do not change the definitions must not re-read c/defs.
	for seqNr := uint64(4); seqNr <= 6; seqNr++ {
		_, err = p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, nil)
		require.NoError(t, err)
	}
	require.Equal(t, before, kv.readCount(keyChannelState), "c/defs must be served from the cache while c/seqnr is unchanged")
	require.Contains(t, kvChannelDefs(t, kv), llotypes.ChannelID(1))

	// Adding a channel bumps c/seqnr, so the next round re-reads.
	_, err = p.StateTransition(ctx, 7, ocrtypes.AttributedQuery{}, addChannelRound(t, 3_000, 2, jsonChannel()), kv, nil)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 8, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)
	require.Greater(t, kv.readCount(keyChannelState), before, "a c/seqnr change must force a re-read")
	require.Len(t, kvChannelDefs(t, kv), 2)
}

func Test_ChannelCache_StaleSeqNrForcesReload(t *testing.T) {
	kv := newMemKV()
	defs := llotypes.ChannelDefinitions{1: jsonChannel()}
	require.NoError(t, writeChannelState(kv, 5, defs))
	require.NoError(t, writeHotState(kv, 0, nil, nil, nil))

	cache := newChannelCache()
	s, err := loadKVState(kv, cache)
	require.NoError(t, err)
	require.Equal(t, uint64(5), s.channelStateSeqNr)
	require.Len(t, s.channelDefinitions, 1)

	// An OLDER stored seqNr (replay / snapshot restore) must not be served from
	// the cache: the comparison is equality, not "cached is newer".
	require.NoError(t, writeChannelState(kv, 4, llotypes.ChannelDefinitions{}))
	before := kv.readCount(keyChannelState)
	s, err = loadKVState(kv, cache)
	require.NoError(t, err)
	require.Greater(t, kv.readCount(keyChannelState), before)
	require.Empty(t, s.channelDefinitions)
}

func Test_HotState_DropsOrphanedCarryForward(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	tsChannel := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	tsv := &protocol.TimestampedStreamValue{ObservedAtNanoseconds: 42, StreamValue: protocol.ToDecimal(decimal.NewFromInt(7))}

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1_000, 1, tsChannel), kv, nil)
	require.NoError(t, err)

	// Round 3: observe a timestamped value so it gets carried forward.
	obs := Observation{UnixTimestampNanoseconds: 2_000, StreamValues: protocol.StreamValues{100: tsv}}
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, obs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)
	require.NotNil(t, kvHotState(t, kv).carryForward[100][llotypes.AggregatorMedian])

	// Round 4: remove the only channel referencing the pair; the carry-forward
	// value is reclaimed by not being written into the new hot record.
	removeObs := Observation{UnixTimestampNanoseconds: 3_000, RemoveChannelIDs: map[llotypes.ChannelID]struct{}{1: {}}}
	removeAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		removeAOs = append(removeAOs, ao(i, mustEncodeObs(t, removeObs)))
	}
	_, err = p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, removeAOs, kv, nil)
	require.NoError(t, err)
	require.Empty(t, kvHotState(t, kv).carryForward)
}

func Test_KVRecords_DeterministicAndRoundTrip(t *testing.T) {
	defs := llotypes.ChannelDefinitions{
		3: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 300, Aggregator: llotypes.AggregatorMedian}}},
		1: jsonChannel(),
		2: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 200, Aggregator: llotypes.AggregatorMode}}},
	}
	validAfter := map[llotypes.ChannelID]uint64{3: 30, 1: 10, 2: 20}
	reportable := map[llotypes.ChannelID]bool{3: true, 1: false, 2: true}
	carry := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{
		200: {llotypes.AggregatorMode: {ObservedAtNanoseconds: 2, StreamValue: protocol.ToDecimal(decimal.NewFromInt(2))}},
		100: {llotypes.AggregatorMedian: {ObservedAtNanoseconds: 1, StreamValue: protocol.ToDecimal(decimal.NewFromInt(1))}},
	}

	// Repeated marshals of the same logical state must be byte-identical: the
	// store is replicated and any divergence halts the protocol.
	var channelBytes, hotBytes []byte
	for i := 0; i < 8; i++ {
		kv := newMemKV()
		require.NoError(t, writeChannelState(kv, 9, defs))
		require.NoError(t, writeHotState(kv, 1_234, validAfter, reportable, carry))
		if i == 0 {
			channelBytes, hotBytes = kv.m[string(keyChannelState)], kv.m[string(keyHotState)]
			continue
		}
		require.Equal(t, channelBytes, kv.m[string(keyChannelState)])
		require.Equal(t, hotBytes, kv.m[string(keyHotState)])
	}

	kv := newMemKV()
	require.NoError(t, writeChannelState(kv, 9, defs))
	require.NoError(t, writeHotState(kv, 1_234, validAfter, reportable, carry))
	require.Equal(t, uint64(9), binary.BigEndian.Uint64(kv.m[string(keyChannelSeqNr)]))

	s, err := loadKVState(kv, nil)
	require.NoError(t, err)
	require.Equal(t, defs, s.channelDefinitions)
	require.Equal(t, uint64(1_234), s.observationTimestampNs)
	require.Equal(t, validAfter, s.validAfterNanoseconds)
	// Only true entries are persisted.
	require.Equal(t, map[llotypes.ChannelID]bool{3: true, 2: true}, s.reportedLastRound)
	require.Len(t, s.carryForward, 2)
	require.Equal(t, uint64(1), s.carryForward[100][llotypes.AggregatorMedian].ObservedAtNanoseconds)
	require.Equal(t, uint64(2), s.carryForward[200][llotypes.AggregatorMode].ObservedAtNanoseconds)
}
