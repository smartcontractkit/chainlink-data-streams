package llo

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31/llotest"
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/commontypes"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// attachAOTelemeter wires an attributed observation telemeter to p without
// starting it, so tests read the enqueued rounds directly.
func attachAOTelemeter(t *testing.T, p *Plugin) *attributedObservationTelemeter {
	t.Helper()
	tel := newAttributedObservationTelemeter(logger.Test(t), make(chan *protocol.LLOAttributedObservationTelemetry), p.ConfigDigest, p.DonID, p.OracleID)
	p.attributedObservationTelemeter = tel
	return tel
}

// requireAORound asserts whether the last StateTransition enqueued a round.
func requireAORound(t *testing.T, tel *attributedObservationTelemeter, want bool) attributedObservationRound {
	t.Helper()
	if !want {
		require.Empty(t, tel.in, "no attributed observation telemetry expected")
		return attributedObservationRound{}
	}
	require.Len(t, tel.in, 1, "one attributed observation round expected")
	return <-tel.in
}

func Test_IsTelemetryEmitter(t *testing.T) {
	for _, tc := range []struct{ n, f int }{{4, 1}, {7, 2}, {16, 5}, {31, 10}} {
		for seqNr := uint64(0); seqNr < uint64(3*tc.n); seqNr++ {
			var emitters []int
			for id := 0; id < tc.n; id++ {
				p := &Plugin{N: tc.n, F: tc.f, OracleID: commontypes.OracleID(id)}
				if p.isTelemetryEmitter(seqNr) {
					emitters = append(emitters, id)
				}
			}
			want := make([]int, 0, tc.f+1)
			for i := 0; i <= tc.f; i++ {
				want = append(want, int((seqNr+uint64(i))%uint64(tc.n)))
			}
			sort.Ints(want)
			require.Equal(t, want, emitters, "n=%d f=%d seqNr=%d", tc.n, tc.f, seqNr)
		}
	}
}

func Test_AttributedObservationTelemetry_StateTransition(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	p.DonID = 7
	tel := attachAOTelemeter(t, p)
	kv := newMemKV()

	channelDef := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
	obsAt := func(observer int, ts uint64) Observation {
		return Observation{
			UnixTimestampNanoseconds: ts,
			RemoveChannelIDs:         map[llotypes.ChannelID]struct{}{9: {}},
			UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef},
			StreamValues: protocol.StreamValues{
				100: protocol.ToDecimal(decimal.NewFromInt(int64(40 + observer))),
				101: protocol.ToDecimal(decimal.NewFromInt(int64(50 + observer))),
			},
		}
	}
	round := func(seqNr uint64, ts uint64) precursor {
		t.Helper()
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, obsAt(i, ts+uint64(i)))))
		}
		b, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
		require.NoError(t, err)
		out, err := decodePrecursor(b)
		require.NoError(t, err)
		return out
	}

	t.Run("bootstrap round emits nothing", func(t *testing.T) {
		p.OracleID = 1
		_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
		require.NoError(t, err)
		requireAORound(t, tel, false)
	})

	t.Run("emitter round carries every observer", func(t *testing.T) {
		p.OracleID = 2
		out := round(2, 1000)
		r := requireAORound(t, tel, true)
		require.Equal(t, uint64(2), r.seqNr)
		require.Equal(t, out.ObservationTimestampNanoseconds, r.agreedObservationTimestampNanos)
		require.Len(t, r.aos, 4)

		for i, a := range r.aos {
			require.Equal(t, commontypes.OracleID(i), a.observer)
			msgs := tel.makeTelemetry(r.seqNr, r.agreedObservationTimestampNanos, a)
			require.Len(t, msgs, 1)
			m := msgs[0]
			require.Equal(t, p.ConfigDigest[:], m.ConfigDigest)
			require.Equal(t, uint64(2), m.SeqNr)
			require.Equal(t, uint32(7), m.DonId)
			require.Equal(t, uint32(i), m.Observer)
			require.Equal(t, 1000+uint64(i), m.OracleObservationTimestampNanoseconds)
			require.Equal(t, out.ObservationTimestampNanoseconds, m.AgreedObservationTimestampNanoseconds)
			require.Nil(t, m.DecodeError)
			require.Equal(t, []uint32{9}, m.RemoveChannelIds)
			require.Contains(t, m.UpdateChannelDefinitions, uint32(1))
			require.Len(t, m.StreamValues, 2)
			sv, err := protocol.UnmarshalProtoStreamValue(m.StreamValues[100])
			require.NoError(t, err)
			require.Equal(t, protocol.ToDecimal(decimal.NewFromInt(int64(40+i))), sv)
		}
	})

	t.Run("non emitter round emits nothing", func(t *testing.T) {
		// Emitters for seqNr 3 with N=4, F=1 are oracles 3 and 0.
		p.OracleID = 1
		round(3, 2000)
		requireAORound(t, tel, false)
	})

	t.Run("agreed timestamp holds when the median regresses", func(t *testing.T) {
		p.OracleID = 0 // emitter for seqNr 4
		out := round(4, 500)
		r := requireAORound(t, tel, true)
		require.Greater(t, out.ObservationTimestampNanoseconds, uint64(500+3), "median must have been held")
		require.Equal(t, out.ObservationTimestampNanoseconds, r.agreedObservationTimestampNanos)
		m := tel.makeTelemetry(r.seqNr, r.agreedObservationTimestampNanos, r.aos[0])[0]
		require.Equal(t, uint64(500), m.OracleObservationTimestampNanoseconds)
		require.Equal(t, out.ObservationTimestampNanoseconds, m.AgreedObservationTimestampNanoseconds)
	})

	t.Run("invalid observation carries only the decode error", func(t *testing.T) {
		p.OracleID = 1 // emitter for seqNr 5
		aos := []ocrtypes.AttributedObservation{
			ao(0, mustEncodeObs(t, obsAt(0, 3000))),
			ao(1, mustEncodeObs(t, obsAt(1, 3000))),
			ao(2, mustEncodeObs(t, obsAt(2, 3000))),
			ao(3, []byte{0x7f}),
		}
		_, err := p.StateTransition(ctx, 5, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
		require.NoError(t, err)
		r := requireAORound(t, tel, true)
		require.Len(t, r.aos, 4)

		bad := r.aos[3]
		require.Equal(t, commontypes.OracleID(3), bad.observer)
		msgs := tel.makeTelemetry(r.seqNr, r.agreedObservationTimestampNanos, bad)
		require.Len(t, msgs, 1)
		require.NotNil(t, msgs[0].DecodeError)
		require.Contains(t, *msgs[0].DecodeError, "unknown observation wire version")
		require.Empty(t, msgs[0].StreamValues)
		require.Empty(t, msgs[0].RemoveChannelIds)
		require.Empty(t, msgs[0].UpdateChannelDefinitions)
		require.Zero(t, msgs[0].OracleObservationTimestampNanoseconds)
	})

	t.Run("aborted round emits nothing", func(t *testing.T) {
		p.OracleID = 2 // emitter for seqNr 6
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, obsAt(i, 4000))))
		}
		// A fetcher without the blobs fails the fetch, which aborts the round.
		_, err := p.StateTransition(ctx, 6, ocrtypes.AttributedQuery{}, aos, kv, llotest.NewBlobBroadcastFetcher())
		require.Error(t, err)
		requireAORound(t, tel, false)
	})
}

func Test_PluginTelemetry_Lifecycle(t *testing.T) {
	t.Run("staging telemetry off", func(t *testing.T) { testPluginTelemetryLifecycle(t, false) })
	t.Run("staging telemetry on", func(t *testing.T) { testPluginTelemetryLifecycle(t, true) })
}

// testPluginTelemetryLifecycle drives a v31 to v31 handover and checks outcome
// and attributed observation telemetry at every stage.
func testPluginTelemetryLifecycle(t *testing.T, stagingTelemetry bool) {
	oldDigest := ocrtypes.ConfigDigest{0xa1}
	newDigest := ocrtypes.ConfigDigest{0xa2}

	// Outcome and attributed observation telemetry share the same gate: emitted
	// for production rounds, and staging ones when enabled. An empty stage
	// expects nothing.
	requireRoundTelemetry := func(t *testing.T, tel *attributedObservationTelemeter, ot chan *protocol.LLOOutcomeTelemetry, stage llotypes.LifeCycleStage) {
		t.Helper()
		requireAORound(t, tel, stage != "")
		if stage == "" {
			require.Empty(t, ot, "no outcome telemetry expected")
			return
		}
		require.Len(t, ot, 1, "one outcome telemetry expected")
		require.Equal(t, stage, llotypes.LifeCycleStage((<-ot).LifeCycleStage))
	}

	// emitterRound makes the instance an emitter for its next round, so only
	// the lifecycle gate decides whether it emits.
	emitterRound := func(v *v31Instance) {
		v.p.OracleID = commontypes.OracleID((v.seqNr + 1) % uint64(v.p.N))
	}

	old := newV31Instance(t, oldDigest, nil, nil)
	oldTel := attachAOTelemeter(t, old.p)
	oldOT := make(chan *protocol.LLOOutcomeTelemetry, 8)
	old.p.OutcomeTelemetryCh = oldOT
	ts := handoverTickNanos

	emitterRound(old)
	old.round(Observation{})
	requireRoundTelemetry(t, oldTel, oldOT, "")
	require.Equal(t, protocol.LifeCycleStageProduction, old.lifeCycleStage())

	ts += handoverTickNanos
	emitterRound(old)
	old.round(Observation{
		UnixTimestampNanoseconds: ts,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
		StreamValues:             handoverStreamValues(),
	})
	requireRoundTelemetry(t, oldTel, oldOT, protocol.LifeCycleStageProduction)

	for range 2 {
		ts += handoverTickNanos
		emitterRound(old)
		old.round(Observation{UnixTimestampNanoseconds: ts, StreamValues: handoverStreamValues()})
		requireRoundTelemetry(t, oldTel, oldOT, protocol.LifeCycleStageProduction)
	}

	// Staging instance: emits from its first round after bootstrap, only when
	// staging telemetry is enabled.
	newInst := newV31Instance(t, newDigest, &oldDigest, newPredecessorCache(t, nil, true))
	newInst.p.Config.CaptureStagingTelemetry = stagingTelemetry
	stagingStage := llotypes.LifeCycleStage("")
	if stagingTelemetry {
		stagingStage = protocol.LifeCycleStageStaging
	}
	newTel := attachAOTelemeter(t, newInst.p)
	newOT := make(chan *protocol.LLOOutcomeTelemetry, 8)
	newInst.p.OutcomeTelemetryCh = newOT
	stagingTS := ts

	emitterRound(newInst)
	newInst.round(Observation{})
	requireRoundTelemetry(t, newTel, newOT, "")
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage())

	stagingTS += handoverTickNanos
	emitterRound(newInst)
	newInst.round(Observation{
		UnixTimestampNanoseconds: stagingTS,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
		StreamValues:             handoverStreamValues(),
	})
	requireRoundTelemetry(t, newTel, newOT, stagingStage)

	stagingTS += handoverTickNanos
	emitterRound(newInst)
	newInst.round(Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})
	requireRoundTelemetry(t, newTel, newOT, stagingStage)

	// Retirement round: the outcome is retired, so nothing is emitted.
	old.p.ShouldRetireCache = &mockShouldRetireCache{retire: true}
	ts += handoverTickNanos
	emitterRound(old)
	old.round(Observation{UnixTimestampNanoseconds: ts, ShouldRetire: true, StreamValues: handoverStreamValues()})
	require.Equal(t, protocol.LifeCycleStageRetired, old.lifeCycleStage())
	requireRoundTelemetry(t, oldTel, oldOT, "")

	ts += handoverTickNanos
	emitterRound(old)
	old.round(Observation{UnixTimestampNanoseconds: ts, StreamValues: handoverStreamValues()})
	requireRoundTelemetry(t, oldTel, oldOT, "")

	// Promotion round: the outcome is production, so it emits.
	promoted := newPredecessorCache(t, attest(t, old.retirementRR), true)
	newInst.p.PredecessorRetirementReportCache = promoted
	attestedBytes, err := promoted.AttestedRetirementReport(oldDigest)
	require.NoError(t, err)

	stagingTS += handoverTickNanos
	emitterRound(newInst)
	newInst.round(Observation{
		UnixTimestampNanoseconds:      stagingTS,
		AttestedPredecessorRetirement: attestedBytes,
		PredecessorSigners:            handoverPredecessorSigners,
		PredecessorF:                  1,
		StreamValues:                  handoverStreamValues(),
	})
	require.Equal(t, protocol.LifeCycleStageProduction, newInst.lifeCycleStage())
	requireRoundTelemetry(t, newTel, newOT, protocol.LifeCycleStageProduction)

	stagingTS += handoverTickNanos
	emitterRound(newInst)
	newInst.round(Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})
	requireRoundTelemetry(t, newTel, newOT, protocol.LifeCycleStageProduction)
}

// Test_AttributedObservationTelemetry_Deterministic checks that collecting
// observations for telemetry leaves the state transition output unchanged.
func Test_AttributedObservationTelemetry_Deterministic(t *testing.T) {
	ctx := tests.Context(t)
	plain, withTel := testPlugin(t), testPlugin(t)
	tel := newAttributedObservationTelemeter(logger.Test(t), make(chan *protocol.LLOAttributedObservationTelemetry, 1024), withTel.ConfigDigest, 0, 0)
	withTel.attributedObservationTelemeter = tel
	tel.start()
	t.Cleanup(tel.Close)
	plainKV, telKV := newMemKV(), newMemKV()

	channelDef := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
	for seqNr := uint64(1); seqNr <= 6; seqNr++ {
		withTel.OracleID = commontypes.OracleID(seqNr % uint64(withTel.N))
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			var b []byte
			if seqNr > 1 {
				b = mustEncodeObs(t, Observation{
					UnixTimestampNanoseconds: seqNr * 1000,
					UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef},
					StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(int64(i)))},
				})
			}
			aos = append(aos, ao(i, b))
		}
		want, err := plain.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, plainKV, testBlobs)
		require.NoError(t, err)
		got, err := withTel.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, telKV, testBlobs)
		require.NoError(t, err)
		require.Equal(t, want, got, "seqNr %d", seqNr)
		require.Equal(t, plainKV.m, telKV.m, "seqNr %d", seqNr)
	}
}

func Test_AttributedObservationTelemetry_Split(t *testing.T) {
	tel := newAttributedObservationTelemeter(logger.Test(t), nil, ocrtypes.ConfigDigest{1}, 7, 3)

	obs := Observation{
		UnixTimestampNanoseconds: 1234,
		RemoveChannelIDs:         map[llotypes.ChannelID]struct{}{5: {}, 2: {}},
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}}},
		StreamValues:             protocol.StreamValues{},
	}
	for id := llotypes.StreamID(1); id <= 500; id++ {
		obs.StreamValues[id] = protocol.ToDecimal(decimal.NewFromInt(int64(id) * 1_000_003))
	}
	obs.StreamValues[501] = nil // skipped

	requireParts := func(t *testing.T, msgs []*protocol.LLOAttributedObservationTelemetry) {
		t.Helper()
		seen := map[uint32]bool{}
		for i, m := range msgs {
			require.Equal(t, []byte(tel.configDigest[:]), m.ConfigDigest)
			require.Equal(t, uint64(9), m.SeqNr)
			require.Equal(t, uint32(7), m.DonId)
			require.Equal(t, uint32(4), m.Observer)
			require.Equal(t, uint32(3), m.Emitter)
			require.Equal(t, uint64(1234), m.OracleObservationTimestampNanoseconds)
			require.Equal(t, uint64(5678), m.AgreedObservationTimestampNanoseconds)
			if i == 0 {
				require.Equal(t, []uint32{2, 5}, m.RemoveChannelIds)
				require.Len(t, m.UpdateChannelDefinitions, 1)
			} else {
				require.Empty(t, m.RemoveChannelIds)
				require.Empty(t, m.UpdateChannelDefinitions)
				require.NotEmpty(t, m.StreamValues, "parts after the first carry values")
			}
			for id := range m.StreamValues {
				require.False(t, seen[id], "stream %d in two parts", id)
				seen[id] = true
			}
		}
		require.Len(t, seen, 500)
	}

	t.Run("fits in one part", func(t *testing.T) {
		msgs := tel.makeTelemetry(9, 5678, attributedObservation{observer: 4, obs: obs})
		require.Len(t, msgs, 1)
		requireParts(t, msgs)
	})

	t.Run("split under the budget", func(t *testing.T) {
		tel.maxBytes = 2048
		msgs := tel.makeTelemetry(9, 5678, attributedObservation{observer: 4, obs: obs})
		require.Greater(t, len(msgs), 2)
		requireParts(t, msgs)
		for _, m := range msgs {
			require.LessOrEqual(t, proto.Size(m), tel.maxBytes)
		}
		// Ascending stream id order across parts.
		var last uint32
		for _, m := range msgs {
			ids := make([]uint32, 0, len(m.StreamValues))
			for id := range m.StreamValues {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			for _, id := range ids {
				require.Greater(t, id, last)
				last = id
			}
		}
	})

	t.Run("oversized values get a part each", func(t *testing.T) {
		tel.maxBytes = 1
		msgs := tel.makeTelemetry(9, 5678, attributedObservation{observer: 4, obs: obs})
		require.Len(t, msgs, 501, "votes part plus one part per value")
		require.Empty(t, msgs[0].StreamValues)
		requireParts(t, msgs)
	})

	t.Run("decode error", func(t *testing.T) {
		msgs := tel.makeTelemetry(9, 5678, attributedObservation{observer: 4, decodeErr: errors.New("boom")})
		require.Len(t, msgs, 1)
		require.Equal(t, "boom", msgs[0].GetDecodeError())
		require.Empty(t, msgs[0].StreamValues)
	})
}

func Test_AttributedObservationTelemetry_NonBlocking(t *testing.T) {
	// Nobody reads the output channel and the queue overflows: neither blocks.
	tel := newAttributedObservationTelemeter(logger.Test(t), make(chan *protocol.LLOAttributedObservationTelemetry), ocrtypes.ConfigDigest{1}, 0, 0)
	tel.start()

	r := attributedObservationRound{seqNr: 1, aos: []attributedObservation{{observer: 0}, {observer: 1}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 * attributedObservationTelemetryQueueSize {
			tel.enqueue(r)
		}
		tel.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("telemeter blocked")
	}
}

// Test_AttributedObservationTelemetry_Concurrent runs the telemeter alongside
// consecutive state transitions, which share the decoded observations. Run
// with -race.
func Test_AttributedObservationTelemetry_Concurrent(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	out := make(chan *protocol.LLOAttributedObservationTelemetry, 16)
	tel := newAttributedObservationTelemeter(logger.Test(t), out, p.ConfigDigest, 0, 0)
	p.attributedObservationTelemeter = tel
	tel.start()

	var wg sync.WaitGroup
	var received int
	wg.Go(func() {
		for range out {
			received++
		}
	})

	kv := newMemKV()
	channelDef := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
	for seqNr := uint64(1); seqNr <= 30; seqNr++ {
		p.OracleID = commontypes.OracleID(seqNr % uint64(p.N))
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			var b []byte
			if seqNr > 1 {
				b = mustEncodeObs(t, Observation{
					UnixTimestampNanoseconds: seqNr * 1000,
					UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef},
					StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(int64(i)))},
				})
			}
			aos = append(aos, ao(i, b))
		}
		_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool { return len(tel.in) == 0 }, 5*time.Second, 10*time.Millisecond)
	tel.Close()
	close(out)
	wg.Wait()
	assert.Positive(t, received)
}

func Test_OutcomeTelemetry_Emitters(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	otCh := make(chan *protocol.LLOOutcomeTelemetry, 8)
	p.OutcomeTelemetryCh = otCh
	kv := newMemKV()

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)

	for seqNr := uint64(2); seqNr <= 9; seqNr++ {
		// Alternate between an emitter and a non emitter for seqNr.
		emitter := seqNr%2 == 0
		p.OracleID = commontypes.OracleID(seqNr % uint64(p.N))
		if !emitter {
			p.OracleID = commontypes.OracleID((seqNr + uint64(p.F) + 1) % uint64(p.N))
		}
		require.Equal(t, emitter, p.isTelemetryEmitter(seqNr))

		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, Observation{UnixTimestampNanoseconds: seqNr * 1000})))
		}
		_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
		require.NoError(t, err)

		if !emitter {
			require.Empty(t, otCh, "seqNr %d", seqNr)
			continue
		}
		require.Len(t, otCh, 1, "seqNr %d", seqNr)
		require.Equal(t, seqNr, (<-otCh).SeqNr)
	}
}

func Test_ReportTelemetry_Lifecycle(t *testing.T) {
	run := func(t *testing.T, predecessor *ocrtypes.ConfigDigest, stagingTelemetry bool, stage llotypes.LifeCycleStage) (*v31Instance, chan *protocol.LLOReportTelemetry) {
		t.Helper()
		v := newV31Instance(t, ocrtypes.ConfigDigest{0xb1}, predecessor, newPredecessorCache(t, nil, true))
		v.p.Config.CaptureStagingTelemetry = stagingTelemetry
		rtCh := make(chan *protocol.LLOReportTelemetry, 16)
		v.p.ReportTelemetryCh = rtCh

		ts := handoverTickNanos
		v.round(Observation{})
		require.Equal(t, stage, v.lifeCycleStage())
		ts += handoverTickNanos
		v.round(Observation{
			UnixTimestampNanoseconds: ts,
			UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
			StreamValues:             handoverStreamValues(),
		})
		for range 3 {
			ts += handoverTickNanos
			v.round(Observation{UnixTimestampNanoseconds: ts, StreamValues: handoverStreamValues()})
		}
		require.NotEmpty(t, v.reports, "the channel must be reporting")
		return v, rtCh
	}

	// A staging instance emits specimen channel reports and a marker per round.
	requireSpecimensAndMarkers := func(t *testing.T, v *v31Instance) {
		t.Helper()
		require.NotEmpty(t, v.reports)
		for _, r := range v.reports {
			require.True(t, r.specimen)
		}
		require.Equal(t, int(v.seqNr)-1, v.markers, "one marker per staging round after bootstrap")
	}

	t.Run("staging emits no report telemetry by default", func(t *testing.T) {
		v, rtCh := run(t, &ocrtypes.ConfigDigest{0xb0}, false, protocol.LifeCycleStageStaging)
		requireSpecimensAndMarkers(t, v)
		require.Empty(t, rtCh)
	})

	t.Run("staging emits specimen report telemetry when enabled", func(t *testing.T) {
		v, rtCh := run(t, &ocrtypes.ConfigDigest{0xb0}, true, protocol.LifeCycleStageStaging)
		requireSpecimensAndMarkers(t, v)
		require.Len(t, rtCh, len(v.reports))
		for range len(v.reports) {
			require.True(t, (<-rtCh).Specimen)
		}
	})

	t.Run("production emits telemetry for every report", func(t *testing.T) {
		v, rtCh := run(t, nil, false, protocol.LifeCycleStageProduction)
		require.Zero(t, v.markers)
		require.Len(t, rtCh, len(v.reports))
		for range len(v.reports) {
			require.False(t, (<-rtCh).Specimen)
		}
	})
}

func Test_Reports_StagingMarker(t *testing.T) {
	ctx := tests.Context(t)

	t.Run("staging returns its specimens followed by the marker", func(t *testing.T) {
		p := testPlugin(t)
		p.ConfigDigest = ocrtypes.ConfigDigest{0xc1}
		codec := &countingReportCodec{}
		p.ReportCodecs = map[llotypes.ReportFormat]protocol.ReportCodec{llotypes.ReportFormatJSON: codec}
		b, err := encodePrecursor(precursor{
			LifeCycleStage:                  protocol.LifeCycleStageStaging,
			ObservationTimestampNanoseconds: 2000,
			ChannelDefinitions: llotypes.ChannelDefinitions{
				1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
			},
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 1000},
			StreamAggregates:      protocol.StreamAggregates{100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))}},
			SupportByFormat:       map[llotypes.ReportFormat]int{llotypes.ReportFormatJSON: 4},
		})
		require.NoError(t, err)

		rwis, err := p.Reports(ctx, 12, b)
		require.NoError(t, err)
		require.Len(t, rwis, 2)
		require.Equal(t, int32(1), codec.calls.Load(), "the channel specimen is encoded")
		require.False(t, protocol.IsStagingMarker(rwis[0].ReportWithInfo.Info, rwis[0].ReportWithInfo.Report))

		marker := rwis[1].ReportWithInfo
		require.Equal(t, llotypes.ReportInfo{LifeCycleStage: protocol.LifeCycleStageStaging, ReportFormat: llotypes.ReportFormatJSON}, marker.Info)
		require.True(t, protocol.IsStagingMarker(marker.Info, marker.Report))
		want, err := protocol.EncodeStagingMarker(p.ConfigDigest, 12)
		require.NoError(t, err)
		require.Equal(t, want, []byte(marker.Report))
	})

	t.Run("production returns no marker", func(t *testing.T) {
		p := testPlugin(t)
		b, err := encodePrecursor(precursor{LifeCycleStage: protocol.LifeCycleStageProduction})
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, 12, b)
		require.NoError(t, err)
		require.Empty(t, rwis)
	})
}

type countingReportCodec struct{ calls atomic.Int32 }

func (c *countingReportCodec) Encode(protocol.Report, llotypes.ChannelDefinition, *protocol.OptsCache) ([]byte, error) {
	c.calls.Add(1)
	return []byte(`{}`), nil
}

func (c *countingReportCodec) Verify(llotypes.ChannelDefinition) error { return nil }
