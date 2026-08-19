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

	cache := protocol.NewChannelCache()
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

	// Round 3: the channel is now in effect; observe a timestamped value so it
	// gets carried forward.
	obs := Observation{UnixTimestampNanoseconds: 2_000, StreamValues: protocol.StreamValues{100: tsv}}
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, obs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)
	require.NotNil(t, kvHotState(t, kv).carryForward[100][llotypes.AggregatorMedian])

	// Round 4: vote to remove the only channel referencing the pair. The removal
	// is deferred, so the channel is still in effect and the value is retained.
	removeObs := Observation{UnixTimestampNanoseconds: 3_000, RemoveChannelIDs: map[llotypes.ChannelID]struct{}{1: {}}}
	removeAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		removeAOs = append(removeAOs, ao(i, mustEncodeObs(t, removeObs)))
	}
	_, err = p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, removeAOs, kv, nil)
	require.NoError(t, err)
	require.NotNil(t, kvHotState(t, kv).carryForward[100][llotypes.AggregatorMedian])

	// Round 5: the removal takes effect and the carry-forward value is reclaimed
	// by not being written into the new hot record.
	nextObs := Observation{UnixTimestampNanoseconds: 4_000}
	nextAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		nextAOs = append(nextAOs, ao(i, mustEncodeObs(t, nextObs)))
	}
	_, err = p.StateTransition(ctx, 5, ocrtypes.AttributedQuery{}, nextAOs, kv, nil)
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

// Test_DeferredDefinitions_TakeEffectNextRound covers the core rule: an agreed
// definition change is persisted immediately but is not in effect until the
// following round, so the round that agreed it still aggregates, reports and
// caches opts against the definitions Observation actually read.
func Test_DeferredDefinitions_TakeEffectNextRound(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	v1 := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
		Opts:         []byte(`{"v":1}`),
	}
	v2 := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}, {StreamID: 200, Aggregator: llotypes.AggregatorMedian}},
		Opts:         []byte(`{"v":2}`),
	}

	round := func(seqNr, ts uint64, defs llotypes.ChannelDefinitions) precursor {
		o := Observation{UnixTimestampNanoseconds: ts, StreamValues: protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(1))}}
		if defs != nil {
			o.UpdateChannelDefinitions = defs
		}
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, o)))
		}
		raw, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, nil)
		require.NoError(t, err)
		prec, err := decodePrecursor(raw)
		require.NoError(t, err)
		return prec
	}

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)

	// Round 2 agrees the addition. Persisted, but not in effect: the precursor
	// (and therefore Reports) must not see it.
	prec2 := round(2, 1_000, llotypes.ChannelDefinitions{1: v1})
	require.Contains(t, kvChannelDefs(t, kv), llotypes.ChannelID(1))
	require.NotContains(t, prec2.ChannelDefinitions, llotypes.ChannelID(1))

	// Round 3: in effect, with v1.
	prec3 := round(3, 2_000, nil)
	require.Equal(t, v1, prec3.ChannelDefinitions[1])

	// Round 4 agrees the update to v2. The precursor must still carry v1,
	// because the round-4 observations were gathered against v1.
	prec4 := round(4, 3_000, llotypes.ChannelDefinitions{1: v2})
	require.Equal(t, v2, kvChannelDefs(t, kv)[1], "the update is persisted immediately")
	require.Equal(t, v1, prec4.ChannelDefinitions[1], "but is not in effect until the next round")

	// Round 5: v2 is in effect.
	prec5 := round(5, 4_000, nil)
	require.Equal(t, v2, prec5.ChannelDefinitions[1])
}

// Test_ChannelGeneration_BindsOptsToDefinitions guards the invariant that
// decoded opts always belong to the very definitions record they were loaded
// with, and that a generation already handed out is never repointed when a later
// round writes a new record. Rounds overlap (Reports for seqNr N can run while
// StateTransition for N+1 does), so this is what stops a report from being
// encoded with another round's opts.
func Test_ChannelGeneration_BindsOptsToDefinitions(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	withOpts := func(raw string) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
			Opts:         []byte(raw),
		}
	}
	type vOpts struct {
		V int `json:"v"`
	}
	// optsV reads channel 1's opts exactly the way a round does: through the
	// generation its own state load resolved.
	optsV := func(s *kvState) int {
		o, err := protocol.GetOpts[vOpts](s.opts, 1)
		require.NoError(t, err)
		return o.V
	}
	round := func(seqNr, ts uint64, defs llotypes.ChannelDefinitions) {
		o := Observation{UnixTimestampNanoseconds: ts}
		if defs != nil {
			o.UpdateChannelDefinitions = defs
		}
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, o)))
		}
		_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, nil)
		require.NoError(t, err)
	}

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)

	// No channels yet: nothing to decode.
	s1, err := loadColdKVState(kv, p.ChannelCache)
	require.NoError(t, err)
	_, err = protocol.GetOpts[vOpts](s1.opts, 1)
	require.Error(t, err, "opts for a channel that is not in the record must not be cached")

	round(2, 1_000, llotypes.ChannelDefinitions{1: withOpts(`{"v":1}`)})

	// The record written by round 2 is what round 3 reads and reports on.
	s2, err := loadColdKVState(kv, p.ChannelCache)
	require.NoError(t, err)
	require.Equal(t, uint64(2), s2.channelStateSeqNr)
	require.Equal(t, 1, optsV(s2))
	require.JSONEq(t, `{"v":1}`, string(s2.channelDefinitions[1].Opts), "opts must decode from this generation's own definitions")

	round(3, 2_000, nil)
	round(4, 3_000, llotypes.ChannelDefinitions{1: withOpts(`{"v":2}`)})

	s4, err := loadColdKVState(kv, p.ChannelCache)
	require.NoError(t, err)
	require.Equal(t, uint64(4), s4.channelStateSeqNr)
	require.Equal(t, 2, optsV(s4))

	// The older generation is untouched by the newer record: a round still
	// holding it keeps the definitions AND the opts it started with.
	require.Equal(t, 1, optsV(s2), "a handed-out generation must never be repointed")
	require.JSONEq(t, `{"v":1}`, string(s2.channelDefinitions[1].Opts))

	// Reports resolves the generation of the record the precursor was built from,
	// so a cache wiped by a restart is repopulated rather than failing.
	p.ChannelCache = protocol.NewChannelCache()
	prec, err := encodePrecursor(precursor{
		LifeCycleStage:     protocol.LifeCycleStageProduction,
		ChannelStateSeqNr:  4,
		ChannelDefinitions: llotypes.ChannelDefinitions{1: withOpts(`{"v":2}`)},
	})
	require.NoError(t, err)
	_, err = p.Reports(ctx, 6, prec)
	require.NoError(t, err)
	s6, err := loadColdKVState(kv, p.ChannelCache)
	require.NoError(t, err)
	require.Equal(t, 2, optsV(s6))
}

// Test_ChannelCache_GenesisThenChangeIsVisible covers the bootstrap path, which
// writes the channel record at seqNr 1 without going through the normal load.
// Generations are keyed by c/seqnr, so the empty genesis record cannot mask a
// later change: no explicit cache invalidation is needed.
func Test_ChannelCache_GenesisThenChangeIsVisible(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	// A reader that runs before any round sees the empty pre-genesis record.
	pre, err := loadColdKVState(kv, p.ChannelCache)
	require.NoError(t, err)
	require.Zero(t, pre.channelStateSeqNr)
	require.Empty(t, pre.channelDefinitions)

	_, err = p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)

	genesis, err := loadColdKVState(kv, p.ChannelCache)
	require.NoError(t, err)
	require.Equal(t, uint64(1), genesis.channelStateSeqNr)
	require.Empty(t, genesis.channelDefinitions)

	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1_000, 1, jsonChannel()), kv, nil)
	require.NoError(t, err)

	after, err := loadColdKVState(kv, p.ChannelCache)
	require.NoError(t, err)
	require.Equal(t, uint64(2), after.channelStateSeqNr)
	require.Contains(t, after.channelDefinitions, llotypes.ChannelID(1))
}
