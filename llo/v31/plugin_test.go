package llo

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// --- test doubles ---

// memKV is an in-memory KeyValueStateReadWriter.
type memKV struct{ m map[string][]byte }

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (k *memKV) Read(key []byte) ([]byte, error) {
	v, ok := k.m[string(key)]
	if !ok {
		return nil, nil
	}
	return append([]byte{}, v...), nil
}
func (k *memKV) Write(key, value []byte) error {
	k.m[string(key)] = append([]byte{}, value...)
	return nil
}
func (k *memKV) Delete(key []byte) error {
	delete(k.m, string(key))
	return nil
}

var _ ocr3_1types.KeyValueStateReadWriter = &memKV{}

// errBroadcaster is a BlobBroadcastFetcher whose BroadcastBlob always fails.
// (A real BlobHandle cannot be constructed outside libocr, so the blob success
// path is exercised only in integration tests with libocr-provided doubles.)
type errBroadcaster struct{ broadcastCalled bool }

func (b *errBroadcaster) BroadcastBlob(context.Context, []byte, ocr3_1types.BlobExpirationHint) (ocr3_1types.BlobHandle, error) {
	b.broadcastCalled = true
	return ocr3_1types.BlobHandle{}, errors.New("broadcast unavailable")
}
func (b *errBroadcaster) FetchBlob(context.Context, ocr3_1types.BlobHandle) ([]byte, error) {
	return nil, errors.New("no blobs")
}

var _ ocr3_1types.BlobBroadcastFetcher = &errBroadcaster{}

// --- helpers ---

func testPlugin(t *testing.T) *Plugin {
	return &Plugin{
		Config:                              Config{VerboseLogging: true},
		ConfigDigest:                        ocrtypes.ConfigDigest{1, 2, 3},
		Logger:                              logger.Test(t),
		N:                                   4,
		F:                                   1,
		ReportCodecs:                        map[llotypes.ReportFormat]llocommon.ReportCodec{llotypes.ReportFormatJSON: llocommon.JSONReportCodec{}},
		OptsCache:                           llocommon.NewOptsCache(),
		ProtocolVersion:                     0,
		DefaultMinReportIntervalNanoseconds: 0,
	}
}

func ao(observer int, obsBytes []byte) ocrtypes.AttributedObservation {
	return ocrtypes.AttributedObservation{Observer: commontypes.OracleID(observer), Observation: obsBytes}
}

func mustEncodeObs(t *testing.T, obs Observation) []byte {
	t.Helper()
	b, err := encodeObservation(context.Background(), obs, 2, 0, nil)
	require.NoError(t, err)
	return b
}

// --- tests ---

func Test_Observation_InlineRoundTrip(t *testing.T) {
	ctx := tests.Context(t)
	obs := Observation{
		ShouldRetire:             true,
		UnixTimestampNanoseconds: 123456789,
		RemoveChannelIDs:         map[llotypes.ChannelID]struct{}{7: {}},
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{
			1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
		},
		StreamValues: llocommon.StreamValues{
			100: llocommon.ToDecimal(decimal.NewFromInt(42)),
		},
	}
	enc, err := encodeObservation(ctx, obs, 2, 0, nil)
	require.NoError(t, err)

	got, err := decodeObservation(ctx, enc, nil)
	require.NoError(t, err)

	assert.Equal(t, obs.ShouldRetire, got.ShouldRetire)
	assert.Equal(t, obs.UnixTimestampNanoseconds, got.UnixTimestampNanoseconds)
	assert.Equal(t, obs.RemoveChannelIDs, got.RemoveChannelIDs)
	require.Contains(t, got.UpdateChannelDefinitions, llotypes.ChannelID(1))
	require.Contains(t, got.StreamValues, llotypes.StreamID(100))
	assert.True(t, equalStreamValue(obs.StreamValues[100], got.StreamValues[100]))
}

func Test_StateTransition_Bootstrap(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	aos := []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}
	precBytes, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)

	// Lifecycle should be production (no predecessor).
	require.Equal(t, string(llocommon.LifeCycleStageProduction), string(kv.m[string(keyLifecycle)]))

	prec, err := decodePrecursor(precBytes)
	require.NoError(t, err)
	require.Equal(t, llocommon.LifeCycleStageProduction, prec.LifeCycleStage)

	// No reports on the initial round.
	reports, err := p.Reports(ctx, 1, precBytes)
	require.NoError(t, err)
	require.Empty(t, reports)
}

func Test_FullRound_AddChannelThenReport(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	// Round 1: bootstrap.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)

	channelDef := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}

	// Round 2 (seqNr=2): four oracles vote to add channel 1.
	addObs := Observation{
		UnixTimestampNanoseconds: 1_000,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef},
	}
	addAOs := []ocrtypes.AttributedObservation{}
	for i := 0; i < 4; i++ {
		addAOs = append(addAOs, ao(i, mustEncodeObs(t, addObs)))
	}
	prec2, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addAOs, kv, nil)
	require.NoError(t, err)

	// Channel is now in state; not yet reportable (validAfter == obsTs).
	require.NotEmpty(t, kv.m[string(channelKey(1))])
	reports2, err := p.Reports(ctx, 2, prec2)
	require.NoError(t, err)
	require.Empty(t, reports2)

	// Round 3 (seqNr=3): later timestamp + stream observations -> reportable.
	valObs := Observation{
		UnixTimestampNanoseconds: 2_000,
		StreamValues:             llocommon.StreamValues{100: llocommon.ToDecimal(decimal.NewFromInt(42))},
	}
	valAOs := []ocrtypes.AttributedObservation{}
	for i := 0; i < 4; i++ {
		valAOs = append(valAOs, ao(i, mustEncodeObs(t, valObs)))
	}
	prec3, err := p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, valAOs, kv, nil)
	require.NoError(t, err)

	reports3, err := p.Reports(ctx, 3, prec3)
	require.NoError(t, err)
	require.Len(t, reports3, 1)
	assert.Equal(t, llotypes.ReportFormatJSON, reports3[0].ReportWithInfo.Info.ReportFormat)
}

func Test_Precursor_RoundTrip_And_Determinism(t *testing.T) {
	p := precursor{
		LifeCycleStage:                  llocommon.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: 999,
		ChannelDefinitions: llotypes.ChannelDefinitions{
			1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
			2: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 200, Aggregator: llotypes.AggregatorMedian}}},
		},
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 100, 2: 200},
		StreamAggregates: llocommon.StreamAggregates{
			100: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(1))},
			200: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(2))},
		},
	}

	b1, err := encodePrecursor(p)
	require.NoError(t, err)
	b2, err := encodePrecursor(p)
	require.NoError(t, err)
	require.Equal(t, b1, b2, "precursor encoding must be deterministic")

	got, err := decodePrecursor(b1)
	require.NoError(t, err)
	assert.Equal(t, p.LifeCycleStage, got.LifeCycleStage)
	assert.Equal(t, p.ObservationTimestampNanoseconds, got.ObservationTimestampNanoseconds)
	assert.Equal(t, p.ValidAfterNanoseconds, got.ValidAfterNanoseconds)
	assert.Len(t, got.ChannelDefinitions, 2)
	assert.Len(t, got.StreamAggregates, 2)
}

func Test_StateTransition_Determinism_ShuffledObservations(t *testing.T) {
	ctx := tests.Context(t)

	channelDef := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	obs := func(observer int, ts uint64, val int64) ocrtypes.AttributedObservation {
		return ao(observer, mustEncodeObs(t, Observation{
			UnixTimestampNanoseconds: ts,
			UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef},
			StreamValues:             llocommon.StreamValues{100: llocommon.ToDecimal(decimal.NewFromInt(val))},
		}))
	}

	run := func(order []int) (*memKV, []byte) {
		p := testPlugin(t)
		kv := newMemKV()
		_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
		require.NoError(t, err)
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		vals := []int64{10, 20, 30, 40}
		for _, i := range order {
			aos = append(aos, obs(i, 1_000, vals[i]))
		}
		prec, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, aos, kv, nil)
		require.NoError(t, err)
		return kv, prec
	}

	kvA, precA := run([]int{0, 1, 2, 3})
	kvB, precB := run([]int{3, 2, 1, 0})

	require.Equal(t, precA, precB, "precursor must be identical regardless of observation order")
	require.Equal(t, kvA.m, kvB.m, "KV write-set must be identical regardless of observation order")
}

func Test_Observation_BlobOffloadFallback(t *testing.T) {
	ctx := tests.Context(t)

	// Build an observation whose serialized stream values exceed a tiny threshold.
	sv := llocommon.StreamValues{}
	for i := 0; i < 500; i++ {
		sv[llotypes.StreamID(i)] = llocommon.ToDecimal(decimal.NewFromInt(int64(i)))
	}
	obs := Observation{UnixTimestampNanoseconds: 1, StreamValues: sv}

	bc := &errBroadcaster{}
	enc, err := encodeObservation(ctx, obs, 2, 16, bc) // 16-byte threshold forces an offload attempt
	require.NoError(t, err)
	require.True(t, bc.broadcastCalled, "expected a blob broadcast attempt for a large observation")

	// Broadcast failed, so values must have fallen back to inline and decode without a fetcher.
	got, err := decodeObservation(ctx, enc, nil)
	require.NoError(t, err)
	require.Len(t, got.StreamValues, len(sv))
}

func Test_SecondsResolutionOverlap(t *testing.T) {
	mkPrec := func(format llotypes.ReportFormat, opts []byte, validAfterNs, obsTsNs uint64) precursor {
		return precursor{
			LifeCycleStage:                  llocommon.LifeCycleStageProduction,
			ObservationTimestampNanoseconds: obsTsNs,
			ChannelDefinitions:              llotypes.ChannelDefinitions{1: {ReportFormat: format, Opts: opts}},
			ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{1: validAfterNs},
		}
	}

	const (
		sec1a = 1_500_000_000 // second 1
		sec1b = 1_900_000_000 // second 1 (later ns, same second)
		sec2  = 2_100_000_000 // second 2
	)

	tests := []struct {
		name       string
		format     llotypes.ReportFormat
		opts       []byte
		validAfter uint64
		obsTs      uint64
		reportable bool
	}{
		{"legacy same second -> not reportable", llotypes.ReportFormatEVMPremiumLegacy, nil, sec1a, sec1b, false},
		{"legacy next second -> reportable", llotypes.ReportFormatEVMPremiumLegacy, nil, sec1a, sec2, true},
		{"json same second -> reportable (nanosecond)", llotypes.ReportFormatJSON, nil, sec1a, sec1b, true},
		{"unpacked default opts same second -> not reportable (defaults to seconds)", llotypes.ReportFormatEVMABIEncodeUnpacked, nil, sec1a, sec1b, false},
		{"unpacked explicit ns same second -> reportable", llotypes.ReportFormatEVMABIEncodeUnpacked, []byte(`{"TimeResolution":"ns"}`), sec1a, sec1b, true},
		{"unpacked explicit seconds next second -> reportable", llotypes.ReportFormatEVMABIEncodeUnpacked, []byte(`{"TimeResolution":"s"}`), sec1a, sec2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mkPrec(tc.format, tc.opts, tc.validAfter, tc.obsTs)
			got := p.reportableChannels(0)
			if tc.reportable {
				require.Equal(t, []llotypes.ChannelID{1}, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
}

func Test_DisableNilStreamValues(t *testing.T) {
	cd := llotypes.ChannelDefinition{
		ReportFormat:           llotypes.ReportFormatJSON,
		DisableNilStreamValues: true,
		Streams:                []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}, {StreamID: 200, Aggregator: llotypes.AggregatorMedian}},
	}
	base := func(aggs llocommon.StreamAggregates) precursor {
		return precursor{
			LifeCycleStage:                  llocommon.LifeCycleStageProduction,
			ObservationTimestampNanoseconds: 2000,
			ChannelDefinitions:              llotypes.ChannelDefinitions{1: cd},
			ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{1: 1000},
			StreamAggregates:                aggs,
		}
	}

	// Missing stream 200 -> not reportable.
	missing := base(llocommon.StreamAggregates{100: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(1))}})
	require.Empty(t, missing.reportableChannels(0))

	// Both streams present -> reportable.
	full := base(llocommon.StreamAggregates{
		100: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(1))},
		200: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(2))},
	})
	require.Equal(t, []llotypes.ChannelID{1}, full.reportableChannels(0))
}

func Test_TimestampedAggregate_CarryForward(t *testing.T) {
	p := testPlugin(t) // F=1
	kv := newMemKV()
	defs := llotypes.ChannelDefinitions{1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}}

	tsv := func(ts uint64, v int64) llocommon.StreamValue {
		return &llocommon.TimestampedStreamValue{ObservedAtNanoseconds: ts, StreamValue: llocommon.ToDecimal(decimal.NewFromInt(v))}
	}
	roundAgg := func(ts uint64, v int64) *llocommon.TimestampedStreamValue {
		out := llocommon.StreamAggregates{}
		obs := map[llotypes.StreamID][]llocommon.StreamValue{100: {tsv(ts, v), tsv(ts, v), tsv(ts, v)}}
		require.NoError(t, p.aggregate(kv, defs, obs, out))
		res, ok := out[100][llotypes.AggregatorMedian].(*llocommon.TimestampedStreamValue)
		require.True(t, ok, "expected a TimestampedStreamValue aggregate")
		return res
	}

	// Round 1: establish ts=100.
	require.Equal(t, uint64(100), roundAgg(100, 5).ObservedAtNanoseconds)
	// Round 2: an older aggregation must NOT overwrite the carried-forward value.
	require.Equal(t, uint64(100), roundAgg(50, 7).ObservedAtNanoseconds)
	// Round 3: a strictly newer aggregation is adopted.
	require.Equal(t, uint64(200), roundAgg(200, 9).ObservedAtNanoseconds)
	// And the newer value is persisted to the t/ carry-forward key.
	persisted, err := readTimestampedAggregate(kv, 100, llotypes.AggregatorMedian)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	require.Equal(t, uint64(200), persisted.ObservedAtNanoseconds)
}

func Test_Telemetry(t *testing.T) {
	ctx := tests.Context(t)
	otCh := make(chan *llocommon.LLOOutcomeTelemetry, 8)
	rtCh := make(chan *llocommon.LLOReportTelemetry, 8)
	p := testPlugin(t)
	p.DonID = 7
	p.OutcomeTelemetryCh = otCh
	p.ReportTelemetryCh = rtCh
	kv := newMemKV()

	channelDef := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
	obs := func(ts uint64, withVal bool) []ocrtypes.AttributedObservation {
		o := Observation{UnixTimestampNanoseconds: ts, UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef}}
		if withVal {
			o.StreamValues = llocommon.StreamValues{100: llocommon.ToDecimal(decimal.NewFromInt(42))}
		}
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, o)))
		}
		return aos
	}

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)
	require.Empty(t, otCh, "no outcome telemetry on the bootstrap round")

	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, obs(1000, false), kv, nil)
	require.NoError(t, err)
	prec3, err := p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, obs(2000, true), kv, nil)
	require.NoError(t, err)

	require.Len(t, otCh, 2, "one outcome telemetry per non-bootstrap StateTransition")
	ot := <-otCh
	require.Equal(t, uint32(7), ot.DonId)

	reports, err := p.Reports(ctx, 3, prec3)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Len(t, rtCh, 1, "one report telemetry per emitted report")
	rt := <-rtCh
	require.Equal(t, uint32(7), rt.DonId)
	require.Equal(t, uint32(1), rt.ChannelId)
}

// equalStreamValue compares two stream values by their binary encoding.
func equalStreamValue(a, b llocommon.StreamValue) bool {
	ba, err := a.MarshalBinary()
	if err != nil {
		return false
	}
	bb, err := b.MarshalBinary()
	if err != nil {
		return false
	}
	return string(ba) == string(bb)
}
