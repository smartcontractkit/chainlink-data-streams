package llo

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	"google.golang.org/protobuf/proto"
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
		ReportCodecs:                        map[llotypes.ReportFormat]protocol.ReportCodec{llotypes.ReportFormatJSON: reportcodec.JSONReportCodec{}},
		OptsCache:                           protocol.NewOptsCache(),
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
		StreamValues: protocol.StreamValues{
			100: protocol.ToDecimal(decimal.NewFromInt(42)),
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
	require.Equal(t, string(protocol.LifeCycleStageProduction), string(kv.m[string(keyLifecycle)]))

	prec, err := decodePrecursor(precBytes)
	require.NoError(t, err)
	require.Equal(t, protocol.LifeCycleStageProduction, prec.LifeCycleStage)

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
		StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(42))},
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
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: 999,
		ChannelDefinitions: llotypes.ChannelDefinitions{
			1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
			2: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 200, Aggregator: llotypes.AggregatorMedian}}},
		},
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 100, 2: 200},
		StreamAggregates: protocol.StreamAggregates{
			100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
			200: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(2))},
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
			StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(val))},
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
	sv := protocol.StreamValues{}
	for i := 0; i < 500; i++ {
		sv[llotypes.StreamID(i)] = protocol.ToDecimal(decimal.NewFromInt(int64(i)))
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

func Test_decodeObservation_RejectsHugeHandleCount(t *testing.T) {
	ctx := tests.Context(t)
	// version byte + a uvarint encoding a huge handle count.
	buf := []byte{observationWireVersion}
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], ^uint64(0)) // max uint64
	buf = append(buf, tmp[:n]...)
	_, err := decodeObservation(ctx, buf, nil)
	require.Error(t, err, "must reject an oversized handle count instead of allocating")
	require.Contains(t, err.Error(), "too many blobs")
}

func Test_SecondsResolutionOverlap(t *testing.T) {
	mkPrec := func(format llotypes.ReportFormat, opts []byte, validAfterNs, obsTsNs uint64) precursor {
		return precursor{
			LifeCycleStage:                  protocol.LifeCycleStageProduction,
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
			got := p.reportableChannels(0, protocol.NewOptsCache(), logger.Test(t))
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
	base := func(aggs protocol.StreamAggregates) precursor {
		return precursor{
			LifeCycleStage:                  protocol.LifeCycleStageProduction,
			ObservationTimestampNanoseconds: 2000,
			ChannelDefinitions:              llotypes.ChannelDefinitions{1: cd},
			ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{1: 1000},
			StreamAggregates:                aggs,
		}
	}

	// Missing stream 200 -> not reportable.
	missing := base(protocol.StreamAggregates{100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))}})
	require.Empty(t, missing.reportableChannels(0, protocol.NewOptsCache(), logger.Test(t)))

	// Both streams present -> reportable.
	full := base(protocol.StreamAggregates{
		100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
		200: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(2))},
	})
	require.Equal(t, []llotypes.ChannelID{1}, full.reportableChannels(0, protocol.NewOptsCache(), logger.Test(t)))
}

func Test_DisableNilStreamValues_CalculatedStreams(t *testing.T) {
	const (
		base1 = llotypes.StreamID(100)
		base2 = llotypes.StreamID(200)
		expr1 = llotypes.StreamID(900)
	)
	validOpts := []byte(`{"abi":[{"type":"int256","expression":"Add(s100, s200)","expressionStreamID":900}]}`)

	baseStreams := []llotypes.Stream{
		{StreamID: base1, Aggregator: llotypes.AggregatorMedian},
		{StreamID: base2, Aggregator: llotypes.AggregatorMedian},
	}
	withCalculated := append(append([]llotypes.Stream{}, baseStreams...),
		llotypes.Stream{StreamID: expr1, Aggregator: llotypes.AggregatorCalculated})

	baseAggregates := func() protocol.StreamAggregates {
		return protocol.StreamAggregates{
			base1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
			base2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(2))},
		}
	}
	evaluatedAggregates := func() protocol.StreamAggregates {
		aggs := baseAggregates()
		aggs[expr1] = map[llotypes.Aggregator]protocol.StreamValue{
			llotypes.AggregatorCalculated: protocol.ToDecimal(decimal.NewFromInt(3)),
		}
		return aggs
	}

	mkPrec := func(disableNil bool, opts []byte, streams []llotypes.Stream, aggs protocol.StreamAggregates) precursor {
		return precursor{
			LifeCycleStage:                  protocol.LifeCycleStageProduction,
			ObservationTimestampNanoseconds: 2000,
			ChannelDefinitions: llotypes.ChannelDefinitions{1: {
				ReportFormat:           llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
				DisableNilStreamValues: disableNil,
				Opts:                   opts,
				Streams:                streams,
			}},
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 1000},
			StreamAggregates:      aggs,
		}
	}
	// A cache populated with the channel's opts, as StateTransition would leave it.
	populatedCache := func(o precursor) *protocol.OptsCache {
		c := protocol.NewOptsCache()
		c.ResetTo(o.ChannelDefinitions)
		return c
	}

	t.Run("evaluation failed -> not reportable", func(t *testing.T) {
		// ProcessCalculatedStreams bailed before appending the calculated stream
		// and before writing its aggregate; the definition alone looks complete.
		o := mkPrec(true, validOpts, baseStreams, baseAggregates())
		require.Empty(t, o.reportableChannels(0, populatedCache(o), logger.Test(t)))
	})

	t.Run("calculated stream declared but nil aggregate -> not reportable", func(t *testing.T) {
		o := mkPrec(true, validOpts, withCalculated, baseAggregates())
		require.Empty(t, o.reportableChannels(0, populatedCache(o), logger.Test(t)))
	})

	t.Run("fully evaluated -> reportable", func(t *testing.T) {
		o := mkPrec(true, validOpts, withCalculated, evaluatedAggregates())
		require.Equal(t, []llotypes.ChannelID{1}, o.reportableChannels(0, populatedCache(o), logger.Test(t)))
	})

	t.Run("DisableNilStreamValues=false, evaluation failed -> still reportable", func(t *testing.T) {
		o := mkPrec(false, validOpts, baseStreams, baseAggregates())
		require.Equal(t, []llotypes.ChannelID{1}, o.reportableChannels(0, populatedCache(o), logger.Test(t)))
	})

	t.Run("malformed opts -> not reportable", func(t *testing.T) {
		o := mkPrec(true, []byte(`{"abi":`), withCalculated, evaluatedAggregates())
		require.Empty(t, o.reportableChannels(0, populatedCache(o), logger.Test(t)))
	})

	t.Run("opts declare no expressions -> not reportable", func(t *testing.T) {
		o := mkPrec(true, []byte(`{"abi":[]}`), withCalculated, evaluatedAggregates())
		require.Empty(t, o.reportableChannels(0, populatedCache(o), logger.Test(t)))
	})

	t.Run("cache miss falls back to channel opts -> reportable", func(t *testing.T) {
		o := mkPrec(true, validOpts, withCalculated, evaluatedAggregates())
		require.Equal(t, []llotypes.ChannelID{1}, o.reportableChannels(0, protocol.NewOptsCache(), logger.Test(t)))
	})

	t.Run("cache miss falls back to channel opts -> not reportable when unevaluated", func(t *testing.T) {
		o := mkPrec(true, validOpts, baseStreams, baseAggregates())
		require.Empty(t, o.reportableChannels(0, protocol.NewOptsCache(), logger.Test(t)))
	})
}

func Test_TimestampedAggregate_CarryForward(t *testing.T) {
	p := testPlugin(t) // F=1
	kv := newMemKV()
	defs := llotypes.ChannelDefinitions{1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}}

	tsv := func(ts uint64, v int64) protocol.StreamValue {
		return &protocol.TimestampedStreamValue{ObservedAtNanoseconds: ts, StreamValue: protocol.ToDecimal(decimal.NewFromInt(v))}
	}
	roundAgg := func(ts uint64, v int64) *protocol.TimestampedStreamValue {
		out := protocol.StreamAggregates{}
		obs := map[llotypes.StreamID][]protocol.StreamValue{100: {tsv(ts, v), tsv(ts, v), tsv(ts, v)}}
		require.NoError(t, p.aggregate(kv, defs, obs, out))
		res, ok := out[100][llotypes.AggregatorMedian].(*protocol.TimestampedStreamValue)
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
	otCh := make(chan *protocol.LLOOutcomeTelemetry, 8)
	rtCh := make(chan *protocol.LLOReportTelemetry, 8)
	p := testPlugin(t)
	p.DonID = 7
	p.OutcomeTelemetryCh = otCh
	p.ReportTelemetryCh = rtCh
	kv := newMemKV()

	channelDef := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
	obs := func(ts uint64, withVal bool) []ocrtypes.AttributedObservation {
		o := Observation{UnixTimestampNanoseconds: ts, UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef}}
		if withVal {
			o.StreamValues = protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(42))}
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

func Test_CalculatedStreams(t *testing.T) {
	p := testPlugin(t)
	cid := llotypes.ChannelID(5)
	opts := []byte(`{"abi":[{"type":"int256","expression":"Add(s1, s2)","expressionStreamID":999}]}`)
	p.OptsCache.Set(cid, opts)

	prec := precursor{
		ObservationTimestampNanoseconds: 1000,
		ChannelDefinitions: llotypes.ChannelDefinitions{cid: {
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Opts:         opts,
			Streams: []llotypes.Stream{
				{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
				{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
			},
		}},
		StreamAggregates: protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
			2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(4))},
		},
	}

	calculated.ProcessCalculatedStreams(p.Logger, prec.ChannelDefinitions, prec.StreamAggregates, prec.ObservationTimestampNanoseconds, p.OptsCache)

	// The calculated stream (999) should hold Add(s1, s2) = 7.
	got := prec.StreamAggregates[999][llotypes.AggregatorCalculated]
	require.NotNil(t, got)
	d, ok := got.(*protocol.Decimal)
	require.True(t, ok)
	require.True(t, d.Decimal().Equal(decimal.NewFromInt(7)), "expected 7, got %s", d.Decimal())

	// The calculated stream should have been appended to the channel definition.
	require.Len(t, prec.ChannelDefinitions[cid].Streams, 3)
	require.Equal(t, llotypes.StreamID(999), prec.ChannelDefinitions[cid].Streams[2].StreamID)
	require.EqualValues(t, llotypes.AggregatorCalculated, prec.ChannelDefinitions[cid].Streams[2].Aggregator)

	// Dry-run helper should accept a valid expression.
	require.NoError(t, calculated.ProcessCalculatedStreamsDryRun("Add(s1, s2)"))
}

func Test_HistoryBackfill(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)

	const (
		targetCID   = llotypes.ChannelID(10)
		backfillCID = llotypes.ChannelID(20)
		fiveSec     = uint64(5_000_000_000)
		eightSec    = uint64(8_000_000_000)
		tenSec      = uint64(10_000_000_000)
	)
	targetCD := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
	backfillCD := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatHistoryBackfill,
		Opts:         []byte(`{"targetChannelId":10,"observations":{"5":{"100":"1.5"},"8":{"100":"2.5"}}}`),
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	defs := llotypes.ChannelDefinitions{targetCID: targetCD, backfillCID: backfillCD}

	// Candidate selection advances with the watermark, then completes.
	ts, raw, opts, ok := selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: 0}, tenSec, backfillCID)
	require.True(t, ok)
	require.Equal(t, fiveSec, ts)
	require.Equal(t, uint64(5), raw)
	require.Equal(t, targetCID, opts.TargetChannelID)

	ts2, _, _, ok2 := selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: fiveSec}, tenSec, backfillCID)
	require.True(t, ok2)
	require.Equal(t, eightSec, ts2)

	_, _, _, ok3 := selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: eightSec}, tenSec, backfillCID)
	require.False(t, ok3, "backfill should be complete once watermark passes the last observation")

	// Reports emits the backfill report, encoded with the target channel's format.
	prec := precursor{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: tenSec,
		ChannelDefinitions:              defs,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{targetCID: tenSec /* target not reportable */, backfillCID: 0},
		StreamAggregates:                protocol.StreamAggregates{},
	}
	b, err := encodePrecursor(prec)
	require.NoError(t, err)
	reports, err := p.Reports(ctx, 2, b)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, llotypes.ReportFormatJSON, reports[0].ReportWithInfo.Info.ReportFormat)
}

// validBlobHandleBytes returns the wire encoding of a syntactically-valid (but
// meaningless) BlobHandle: the sum-type variant byte 0x01 (LightCertifiedBlob)
// followed by a protobuf carrying only chunk_digests_root (field 1) = 32 bytes,
// which is the minimum LightCertifiedBlob.UnmarshalBinary accepts. It lets a
// test build an observation that *references* a blob (so decodeObservation
// reaches the fetch path); the handle can never be fetched because there is no
// real blob behind it. A real handle cannot be constructed outside libocr.
func validBlobHandleBytes() []byte {
	b := []byte{0x01}         // BlobHandle sum-type variant: LightCertifiedBlob
	b = append(b, 0x0A, 0x20) // proto field 1 (bytes), length 32 (== sha256.Size)
	b = append(b, make([]byte, 32)...)
	return b
}

// Test_decodeObservation_BlobFetchErrorIsClassified verifies that a failure to
// fetch a referenced blob surfaces as a *blobFetchError, so StateTransition can
// propagate it (uniform retry) instead of silently dropping the observation on
// only some oracles (Finding 2).
func Test_decodeObservation_BlobFetchErrorIsClassified(t *testing.T) {
	ctx := tests.Context(t)
	mainBytes, err := proto.Marshal(&protocol.LLOObservationProto{UnixTimestampNanoseconds: 42})
	require.NoError(t, err)
	frame := frameObservation([][]byte{validBlobHandleBytes()}, mainBytes)

	var bfErr *blobFetchError

	// A failing fetcher -> node-local error, must be a *blobFetchError.
	_, err = decodeObservation(ctx, frame, &errBroadcaster{})
	require.Error(t, err)
	require.True(t, errors.As(err, &bfErr), "fetch failure must be a blobFetchError so StateTransition propagates it")

	// A nil fetcher (blob referenced but unfetchable) -> also a *blobFetchError.
	_, err = decodeObservation(ctx, frame, nil)
	require.Error(t, err)
	require.True(t, errors.As(err, &bfErr))
}

// Test_decodeObservation_MalformedIsNotBlobFetchError verifies that
// deterministic decode failures (same bytes on every oracle) are NOT classified
// as blobFetchError, so StateTransition still drops just that observation rather
// than aborting the whole round (Finding 2, the other side of the boundary).
func Test_decodeObservation_MalformedIsNotBlobFetchError(t *testing.T) {
	ctx := tests.Context(t)
	var bfErr *blobFetchError

	// Unknown wire version.
	_, err := decodeObservation(ctx, []byte{0x02, 0x00}, &errBroadcaster{})
	require.Error(t, err)
	require.False(t, errors.As(err, &bfErr), "malformed framing must stay droppable, not a blobFetchError")

	// Well-framed but garbage handle bytes: UnmarshalBinary fails deterministically.
	frame := frameObservation([][]byte{{0xFF}}, nil)
	_, err = decodeObservation(ctx, frame, &errBroadcaster{})
	require.Error(t, err)
	require.False(t, errors.As(err, &bfErr))
}

// Test_StateTransition_PropagatesBlobFetchFailure is the end-to-end guard for
// Finding 2: when observations reference an unfetchable blob, StateTransition
// must return an error rather than silently dropping them.
func Test_StateTransition_PropagatesBlobFetchFailure(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	// Bootstrap.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)

	// All four observations reference a blob that cannot be fetched.
	mainBytes, err := proto.Marshal(&protocol.LLOObservationProto{UnixTimestampNanoseconds: 1000})
	require.NoError(t, err)
	frame := frameObservation([][]byte{validBlobHandleBytes()}, mainBytes)
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, frame))
	}

	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, aos, kv, &errBroadcaster{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "fetch blob")
}

// equalStreamValue compares two stream values by their binary encoding.
func equalStreamValue(a, b protocol.StreamValue) bool {
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
