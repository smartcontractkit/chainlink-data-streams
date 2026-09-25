package llo

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31/llotest"
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
type memKV struct {
	m     map[string][]byte
	reads map[string]int
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}, reads: map[string]int{}} }

func (k *memKV) Read(key []byte) ([]byte, error) {
	k.reads[string(key)]++
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

// readCount counts Read calls per key, for cache assertions.
func (k *memKV) readCount(key []byte) int { return k.reads[string(key)] }

// --- KV record accessors for assertions ---

// kvChannelDefs decodes the c/defs record.
func kvChannelDefs(t *testing.T, kv *memKV) llotypes.ChannelDefinitions {
	t.Helper()
	defs, err := readChannelState(kv)
	require.NoError(t, err)
	return defs
}

// kvHotState decodes the r/agg record into a kvState projection.
func kvHotState(t *testing.T, kv *memKV) *kvState {
	t.Helper()
	s := &kvState{
		validAfterNanoseconds: map[llotypes.ChannelID]uint64{},
		reportedLastRound:     map[llotypes.ChannelID]bool{},
		carryForward:          map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{},
	}
	require.NoError(t, readHotState(kv, s))
	return s
}

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

// The blob-carrying paths use the exported in-memory double from llotest, which
// is also what non-libocr hosts (benchmarks, simulation harnesses) should pass
// to NewReportingPlugin in place of a nil BlobBroadcastFetcher.
func newFakeBroadcaster() *llotest.BlobBroadcastFetcher { return llotest.NewBlobBroadcastFetcher() }

// --- helpers ---

func testPlugin(t *testing.T) *Plugin {
	return &Plugin{
		Config:                              Config{VerboseLogging: true},
		ConfigDigest:                        ocrtypes.ConfigDigest{1, 2, 3},
		Logger:                              logger.Test(t),
		N:                                   4,
		F:                                   1,
		ReportCodecs:                        map[llotypes.ReportFormat]protocol.ReportCodec{llotypes.ReportFormatJSON: reportcodec.JSONReportCodec{}},
		ChannelCache:                        protocol.NewChannelCache(),
		ProtocolVersion:                     0,
		DefaultMinReportIntervalNanoseconds: 0,
		// Floor of 3 contributions, which is what N=4, F=1 can sustain.
		AggregationFaultTolerance: 1,
	}
}

// attachPump wires a plugin to a data source and broadcaster through a blob pump
// with test-friendly timings, and stops it at the end of the test.
func attachPump(t *testing.T, p *Plugin, ds DataSource, bbf ocr3_1types.BlobBroadcastFetcher) *blobPump {
	t.Helper()
	p.DataSource = ds
	p.pump = newBlobPump(logger.Test(t), blobPumpParams{
		bbf:                bbf,
		ds:                 ds,
		configDigest:       p.ConfigDigest,
		verboseLogging:     true,
		observationTimeout: tests.WaitTimeout(t),
		maxSnapshotAge:     time.Minute,
		maxSnapshotRounds:  DefaultMaxSnapshotRounds,
		blobLifetimeRounds: DefaultBlobLifetimeRounds,
	})
	p.pump.Start()
	t.Cleanup(func() { require.NoError(t, p.Close()) })
	return p.pump
}

func ao(observer int, obsBytes []byte) ocrtypes.AttributedObservation {
	return ocrtypes.AttributedObservation{Observer: commontypes.OracleID(observer), Observation: obsBytes}
}

// testSupportedReportFormats is the codec coverage a fixture oracle advertises
// by default: every format the tests in this package build channels for.
var testSupportedReportFormats = formatSet(
	llotypes.ReportFormatJSON,
	llotypes.ReportFormatEVMPremiumLegacy,
	llotypes.ReportFormatEVMABIEncodeUnpacked,
	llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
	llotypes.ReportFormatHistoryBackfill,
)

// formatSet builds the advertised-format set a fixture oracle carries.
func formatSet(formats ...llotypes.ReportFormat) map[llotypes.ReportFormat]struct{} {
	out := make(map[llotypes.ReportFormat]struct{}, len(formats))
	for _, f := range formats {
		out[f] = struct{}{}
	}
	return out
}

// testBlobs is the shared in-memory blob store used by StateTransition
// fixtures. It is content-addressed and mutex-guarded, so tests can share it.
var testBlobs = llotest.NewBlobBroadcastFetcher()

// mustEncodeObs encodes an observation for use as a StateTransition fixture.
// Stream values travel the production path: broadcast into testBlobs and
// referenced by handle. Pass testBlobs as the fetcher to StateTransition.
func mustEncodeObs(t *testing.T, obs Observation) []byte {
	t.Helper()
	// A real oracle always advertises the formats it can encode, and channel
	// reportability requires 2f+1 of them to do so. Fixtures that do not care
	// about the support gate get the full set, so they behave like a healthy
	// DON; tests that exercise the gate set the field explicitly.
	if obs.SupportedReportFormats == nil {
		obs.SupportedReportFormats = testSupportedReportFormats
	}
	if len(obs.StreamValues) == 0 {
		b, err := encodeObservation(obs, nil)
		require.NoError(t, err)
		return b
	}
	payload, err := marshalStreamValues(obs.StreamValues)
	require.NoError(t, err)
	handle, err := testBlobs.BroadcastBlob(tests.Context(t), payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 0})
	require.NoError(t, err)
	handleBytes, err := handle.MarshalBinary()
	require.NoError(t, err)
	b, err := encodeObservation(obs, [][]byte{handleBytes})
	require.NoError(t, err)
	return b
}

// --- tests ---

func Test_Observation_WireRoundTrip(t *testing.T) {
	ctx := tests.Context(t)
	obs := Observation{
		ShouldRetire:             true,
		UnixTimestampNanoseconds: 123456789,
		RemoveChannelIDs:         map[llotypes.ChannelID]struct{}{7: {}},
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{
			1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
		},
	}
	enc, err := encodeObservation(obs, nil)
	require.NoError(t, err)

	got, err := decodeObservation(ctx, enc, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, obs.ShouldRetire, got.ShouldRetire)
	assert.Equal(t, obs.UnixTimestampNanoseconds, got.UnixTimestampNanoseconds)
	assert.Equal(t, obs.RemoveChannelIDs, got.RemoveChannelIDs)
	require.Contains(t, got.UpdateChannelDefinitions, llotypes.ChannelID(1))
	require.Empty(t, got.StreamValues)
}

// Test_Observation_StreamValuesNeverInline pins the invariant that stream values
// are only ever disseminated by blob: an encoded observation carries no inline
// stream values even when the Observation struct holds some.
func Test_Observation_StreamValuesNeverInline(t *testing.T) {
	ctx := tests.Context(t)
	obs := Observation{
		UnixTimestampNanoseconds: 1,
		StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(42))},
	}
	enc, err := encodeObservation(obs, nil)
	require.NoError(t, err)

	got, err := decodeObservation(ctx, enc, nil, nil)
	require.NoError(t, err)
	require.Empty(t, got.StreamValues, "stream values must travel in a blob, never inline")
}

// Test_Observation_BlobRoundTrip covers the blob path end to end: the pump's
// serialized payload is broadcast, referenced by handle, and recovered by the
// decoder through the fetcher.
func Test_Observation_BlobRoundTrip(t *testing.T) {
	ctx := tests.Context(t)
	sv := protocol.StreamValues{}
	for i := 0; i < 500; i++ {
		sv[llotypes.StreamID(i)] = protocol.ToDecimal(decimal.NewFromInt(int64(i)))
	}
	payload, err := marshalStreamValues(sv)
	require.NoError(t, err)

	bc := newFakeBroadcaster()
	handle, err := bc.BroadcastBlob(ctx, payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 5})
	require.NoError(t, err)
	handleBytes, err := handle.MarshalBinary()
	require.NoError(t, err)

	enc, err := encodeObservation(Observation{UnixTimestampNanoseconds: 1}, [][]byte{handleBytes})
	require.NoError(t, err)

	got, err := decodeObservation(ctx, enc, bc, nil)
	require.NoError(t, err)
	require.Len(t, got.StreamValues, len(sv))
	require.True(t, equalStreamValue(sv[100], got.StreamValues[100]))

	// Without a fetcher the reference is unusable, and that must be classified
	// as a node-local blob-fetch failure rather than a malformed observation.
	_, err = decodeObservation(ctx, enc, nil, nil)
	var bfErr *blobFetchError
	require.ErrorAs(t, err, &bfErr)
}

func Test_StateTransition_Bootstrap(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	aos := []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}
	precBytes, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
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
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
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
	prec2, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addAOs, kv, testBlobs)
	require.NoError(t, err)

	// The definition is persisted, but the addition is deferred: it is not in
	// effect for round 2, so nothing is reportable.
	require.Contains(t, kvChannelDefs(t, kv), llotypes.ChannelID(1))
	reports2, err := p.Reports(ctx, 2, prec2)
	require.NoError(t, err)
	require.Empty(t, reports2)

	valObs := func(ts uint64) []ocrtypes.AttributedObservation {
		obs := Observation{
			UnixTimestampNanoseconds: ts,
			StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(42))},
		}
		aos := []ocrtypes.AttributedObservation{}
		for i := 0; i < 4; i++ {
			aos = append(aos, ao(i, mustEncodeObs(t, obs)))
		}
		return aos
	}

	// Round 3: the channel is now in effect and gets its first watermark
	// (validAfter == obsTs), so it is still not reportable.
	prec3, err := p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, valObs(2_000), kv, testBlobs)
	require.NoError(t, err)
	reports3, err := p.Reports(ctx, 3, prec3)
	require.NoError(t, err)
	require.Empty(t, reports3)

	// Round 4: later timestamp + stream observations -> reportable.
	prec4, err := p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, valObs(3_000), kv, testBlobs)
	require.NoError(t, err)

	reports4, err := p.Reports(ctx, 4, prec4)
	require.NoError(t, err)
	require.Len(t, reports4, 1)
	assert.Equal(t, llotypes.ReportFormatJSON, reports4[0].ReportWithInfo.Info.ReportFormat)
}

func Test_StateTransition_ContributionFloor(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t) // F=1, AggregationFaultTolerance=1, floor 3
	require.Equal(t, 3, p.minContributions())
	kv := newMemKV()

	channelDef := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	addAOs := []ocrtypes.AttributedObservation{}
	for i := 0; i < 4; i++ {
		addAOs = append(addAOs, ao(i, mustEncodeObs(t, Observation{
			UnixTimestampNanoseconds: 1_000,
			UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: channelDef},
		})))
	}
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addAOs, kv, testBlobs)
	require.NoError(t, err)

	// contributors oracles carry stream 100; the rest take part in the round
	// without contributing a value for it, which is exactly the population
	// collapse the floor guards against.
	valObs := func(ts uint64, contributors int) []ocrtypes.AttributedObservation {
		aos := []ocrtypes.AttributedObservation{}
		for i := 0; i < 4; i++ {
			obs := Observation{UnixTimestampNanoseconds: ts}
			if i < contributors {
				obs.StreamValues = protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(42))}
			}
			aos = append(aos, ao(i, mustEncodeObs(t, obs)))
		}
		return aos
	}

	// Three contributions meet the floor, so the stream aggregates.
	prec, err := p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, valObs(2_000, 3), kv, testBlobs)
	require.NoError(t, err)
	out, err := decodePrecursor(prec)
	require.NoError(t, err)
	require.Contains(t, out.StreamAggregates, llotypes.StreamID(100))

	// Two do not: no aggregate, so no report. The channel itself stays
	// reportable, since nil observed stream values are permitted unless the
	// definition sets DisableNilStreamValues; the report is then dropped at
	// encode time, exactly as for any other missing observed value.
	prec, err = p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, valObs(3_000, 2), kv, testBlobs)
	require.NoError(t, err)
	out, err = decodePrecursor(prec)
	require.NoError(t, err)
	require.NotContains(t, out.StreamAggregates, llotypes.StreamID(100))
	reports, err := p.Reports(ctx, 4, prec)
	require.NoError(t, err)
	require.Empty(t, reports)
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
		_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
		require.NoError(t, err)
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		vals := []int64{10, 20, 30, 40}
		for _, i := range order {
			aos = append(aos, obs(i, 1_000, vals[i]))
		}
		prec, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
		require.NoError(t, err)
		return kv, prec
	}

	kvA, precA := run([]int{0, 1, 2, 3})
	kvB, precB := run([]int{3, 2, 1, 0})

	require.Equal(t, precA, precB, "precursor must be identical regardless of observation order")
	require.Equal(t, kvA.m, kvB.m, "KV write-set must be identical regardless of observation order")
}

func Test_decodeObservation_RejectsHugeHandleCount(t *testing.T) {
	ctx := tests.Context(t)
	// version byte + a uvarint encoding a huge handle count.
	buf := []byte{observationWireVersion}
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], ^uint64(0)) // max uint64
	buf = append(buf, tmp[:n]...)
	_, err := decodeObservation(ctx, buf, nil, nil)
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
			got := p.withSupport(1).reportableChannels(0, 0, protocol.NewOptsCache(), nil)
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
	require.Empty(t, missing.withSupport(1).reportableChannels(0, 0, protocol.NewOptsCache(), nil))

	// Both streams present -> reportable.
	full := base(protocol.StreamAggregates{
		100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
		200: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(2))},
	})
	require.Equal(t, []llotypes.ChannelID{1}, full.withSupport(1).reportableChannels(0, 0, protocol.NewOptsCache(), nil))
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
		// ProcessCalculatedStreams bailed before writing the calculated
		// aggregate; the definition alone looks complete.
		o := mkPrec(true, validOpts, baseStreams, baseAggregates())
		require.Empty(t, o.withSupport(1).reportableChannels(0, 0, populatedCache(o), nil))
	})

	t.Run("inline calculated stream but nil aggregate -> not reportable", func(t *testing.T) {
		o := mkPrec(true, validOpts, withCalculated, baseAggregates())
		require.Empty(t, o.withSupport(1).reportableChannels(0, 0, populatedCache(o), nil))
	})

	t.Run("fully evaluated -> reportable", func(t *testing.T) {
		o := mkPrec(true, validOpts, withCalculated, evaluatedAggregates())
		require.Equal(t, []llotypes.ChannelID{1}, o.withSupport(1).reportableChannels(0, 0, populatedCache(o), nil))
	})

	t.Run("DisableNilStreamValues=false, evaluation failed -> not reportable", func(t *testing.T) {
		// The calculated-stream gate is independent of DisableNilStreamValues,
		// which is about observed values. A missing calculated stream cannot be
		// reported around: the codec has nothing to encode, so Reports skips the
		// report. Treating the channel as reportable would advance validAfter
		// over a round that emitted nothing.
		o := mkPrec(false, validOpts, baseStreams, baseAggregates())
		require.Empty(t, o.withSupport(1).reportableChannels(0, 0, populatedCache(o), nil))
	})

	t.Run("DisableNilStreamValues=false, fully evaluated -> reportable", func(t *testing.T) {
		o := mkPrec(false, validOpts, withCalculated, evaluatedAggregates())
		require.Equal(t, []llotypes.ChannelID{1}, o.withSupport(1).reportableChannels(0, 0, populatedCache(o), nil))
	})

	t.Run("malformed opts -> not reportable", func(t *testing.T) {
		o := mkPrec(true, []byte(`{"abi":`), withCalculated, evaluatedAggregates())
		require.Empty(t, o.withSupport(1).reportableChannels(0, 0, populatedCache(o), nil))
	})

	t.Run("opts declare no expressions -> not reportable", func(t *testing.T) {
		o := mkPrec(true, []byte(`{"abi":[]}`), withCalculated, evaluatedAggregates())
		require.Empty(t, o.withSupport(1).reportableChannels(0, 0, populatedCache(o), nil))
	})

	t.Run("cache miss falls back to channel opts -> reportable", func(t *testing.T) {
		o := mkPrec(true, validOpts, withCalculated, evaluatedAggregates())
		require.Equal(t, []llotypes.ChannelID{1}, o.withSupport(1).reportableChannels(0, 0, protocol.NewOptsCache(), nil))
	})

	t.Run("cache miss falls back to channel opts -> not reportable when unevaluated", func(t *testing.T) {
		o := mkPrec(true, validOpts, baseStreams, baseAggregates())
		require.Empty(t, o.withSupport(1).reportableChannels(0, 0, protocol.NewOptsCache(), nil))
	})
}

func Test_TimestampedAggregate_CarryForward(t *testing.T) {
	p := testPlugin(t) // F=1
	defs := llotypes.ChannelDefinitions{1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}}

	tsv := func(ts uint64, v int64) protocol.StreamValue {
		return &protocol.TimestampedStreamValue{ObservedAtNanoseconds: ts, StreamValue: protocol.ToDecimal(decimal.NewFromInt(v))}
	}
	carry := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
	roundAgg := func(ts uint64, v int64) *protocol.TimestampedStreamValue {
		out := protocol.StreamAggregates{}
		obs := map[llotypes.StreamID][]protocol.StreamValue{100: {tsv(ts, v), tsv(ts, v), tsv(ts, v)}}
		next := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		// No history requirements: this test is about carry-forward aggregation.
		require.NoError(t, p.aggregate(carry, next, defs, obs, out, nil, historyRequirements{}, ts))
		carry = next
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
	// And the newer value is what carries into the next round.
	persisted := carry[100][llotypes.AggregatorMedian]
	require.NotNil(t, persisted)
	require.Equal(t, uint64(200), persisted.ObservedAtNanoseconds)
}

func Test_TimestampedAggregate_CarryForwardOnAggregationFailure(t *testing.T) {
	p := testPlugin(t) // F=1, contribution floor of 3
	defs := llotypes.ChannelDefinitions{1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}}

	tsv := func(ts uint64, v int64) protocol.StreamValue {
		return &protocol.TimestampedStreamValue{ObservedAtNanoseconds: ts, StreamValue: protocol.ToDecimal(decimal.NewFromInt(v))}
	}

	// A single contribution is below the floor, so the median aggregator fails.
	starved := map[llotypes.StreamID][]protocol.StreamValue{100: {tsv(200, 9)}}

	t.Run("carried value is republished into the precursor", func(t *testing.T) {
		carry := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		next := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		out := protocol.StreamAggregates{}

		// Round 1: enough contributions, establishes ts=100.
		healthy := map[llotypes.StreamID][]protocol.StreamValue{100: {tsv(100, 5), tsv(100, 5), tsv(100, 5)}}
		require.NoError(t, p.aggregate(carry, next, defs, healthy, out, nil, historyRequirements{}, 100))
		// Round 2: aggregation fails, the carried value stands in for it.
		carry = next
		next = map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		out = protocol.StreamAggregates{}
		require.NoError(t, p.aggregate(carry, next, defs, starved, out, nil, historyRequirements{}, 200))

		got, ok := out[100][llotypes.AggregatorMedian].(*protocol.TimestampedStreamValue)
		require.True(t, ok, "failed aggregation must still publish the carried value")
		require.Equal(t, uint64(100), got.ObservedAtNanoseconds)

		persisted := next[100][llotypes.AggregatorMedian]
		require.NotNil(t, persisted, "the carry must survive into the next round")
		require.Equal(t, uint64(100), persisted.ObservedAtNanoseconds)
	})

	t.Run("without a carry the pair is absent", func(t *testing.T) {
		carry := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		next := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		out := protocol.StreamAggregates{}

		require.NoError(t, p.aggregate(carry, next, defs, starved, out, nil, historyRequirements{}, 200))
		require.NotContains(t, out[100], llotypes.AggregatorMedian)
		require.Empty(t, next)
	})
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

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	require.Empty(t, otCh, "no outcome telemetry on the bootstrap round")

	// The channel is added at round 2, takes effect at round 3 (where it gets
	// its first watermark) and first reports at round 4.
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, obs(1000, false), kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, obs(2000, true), kv, testBlobs)
	require.NoError(t, err)
	prec4, err := p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, obs(3000, true), kv, testBlobs)
	require.NoError(t, err)

	require.Len(t, otCh, 3, "one outcome telemetry per non-bootstrap StateTransition")
	ot := <-otCh
	require.Equal(t, uint32(7), ot.DonId)

	reports, err := p.Reports(ctx, 4, prec4)
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

	definitions := llotypes.ChannelDefinitions{cid: {
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Opts:         []byte(`{"abi":[{"type":"int256","expression":"Add(s1, s2)","expressionStreamID":999}]}`),
		Streams: []llotypes.Stream{
			{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
		},
	}}

	channelCache := protocol.NewChannelCache()
	generation, err := channelCache.Load(1, func() (llotypes.ChannelDefinitions, error) {
		return definitions, nil
	})
	require.NoError(t, err)

	prec := precursor{
		ObservationTimestampNanoseconds: 1000,
		ChannelDefinitions:              definitions,
		StreamAggregates: protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
			2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(4))},
		},
	}

	calculated.ProcessCalculatedStreams(p.Logger, prec.ChannelDefinitions, prec.StreamAggregates, prec.ObservationTimestampNanoseconds, generation.Opts(), nil)

	// The calculated stream (999) should hold Add(s1, s2) = 7.
	got := prec.StreamAggregates[999][llotypes.AggregatorCalculated]
	require.NotNil(t, got)
	d, ok := got.(*protocol.Decimal)
	require.True(t, ok)
	require.True(t, d.Decimal().Equal(decimal.NewFromInt(7)), "expected 7, got %s", d.Decimal())

	// The channel definition is left untouched: the calculated stream is derived
	// from the opts, not stored.
	require.Len(t, prec.ChannelDefinitions[cid].Streams, 2)

	// It shows up in the derived stream list instead, as the trailing entry.
	streams, err := protocol.EffectiveStreams(generation.Opts(), prec.ChannelDefinitions[cid], cid)
	require.NoError(t, err)
	require.Len(t, streams, 3)
	require.Equal(t, llotypes.StreamID(999), streams[2].StreamID)
	require.EqualValues(t, llotypes.AggregatorCalculated, streams[2].Aggregator)

	// Deriving twice yields the same list: EffectiveStreams drops any inline
	// calculated entries before appending the declared ones, so a definition
	// written by older code derives identically to one that was never mutated.
	mutated := prec.ChannelDefinitions[cid]
	mutated.Streams = streams
	again, err := protocol.EffectiveStreams(generation.Opts(), mutated, cid)
	require.NoError(t, err)
	require.Equal(t, streams, again)

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
	ts, raw, opts, ok := selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: 0}, tenSec, backfillCID, nil)
	require.True(t, ok)
	require.Equal(t, fiveSec, ts)
	require.Equal(t, uint64(5), raw)
	require.Equal(t, targetCID, opts.TargetChannelID)

	ts2, _, _, ok2 := selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: fiveSec}, tenSec, backfillCID, nil)
	require.True(t, ok2)
	require.Equal(t, eightSec, ts2)

	_, _, _, ok3 := selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: eightSec}, tenSec, backfillCID, nil)
	require.False(t, ok3, "backfill should be complete once watermark passes the last observation")

	// Reports emits the backfill report, encoded with the target channel's format.
	prec := precursor{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: tenSec,
		ChannelDefinitions:              defs,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{targetCID: tenSec /* target not reportable */, backfillCID: 0},
		StreamAggregates:                protocol.StreamAggregates{},
	}
	b, err := encodePrecursor(prec.withSupport(3))
	require.NoError(t, err)
	reports, err := p.Reports(ctx, 2, b)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, llotypes.ReportFormatJSON, reports[0].ReportWithInfo.Info.ReportFormat)
}

// validBlobHandleBytes returns the wire encoding of a syntactically valid handle
// for a blob nobody broadcast. It lets a test build an observation that
// *references* a blob (so decodeObservation reaches the fetch path) while the
// fetch is guaranteed to fail.
func validBlobHandleBytes() []byte {
	handle, err := llotest.NewBlobHandle([]byte("never broadcast"))
	if err != nil {
		panic(err)
	}
	b, err := handle.MarshalBinary()
	if err != nil {
		panic(err)
	}
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
	_, err = decodeObservation(ctx, frame, &errBroadcaster{}, nil)
	require.Error(t, err)
	require.True(t, errors.As(err, &bfErr), "fetch failure must be a blobFetchError so StateTransition propagates it")

	// A nil fetcher (blob referenced but unfetchable) -> also a *blobFetchError.
	_, err = decodeObservation(ctx, frame, nil, nil)
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
	_, err := decodeObservation(ctx, []byte{0x02, 0x00}, &errBroadcaster{}, nil)
	require.Error(t, err)
	require.False(t, errors.As(err, &bfErr), "malformed framing must stay droppable, not a blobFetchError")

	// Well-framed but garbage handle bytes: UnmarshalBinary fails deterministically.
	frame := frameObservation([][]byte{{0xFF}}, nil)
	_, err = decodeObservation(ctx, frame, &errBroadcaster{}, nil)
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
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
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

// captureCodec records the report it was asked to encode, so a test can assert
// on the values report assembly produced.
type captureCodec struct{ got *protocol.Report }

func (c captureCodec) Encode(r protocol.Report, _ llotypes.ChannelDefinition, _ *protocol.OptsCache) ([]byte, error) {
	*c.got = r
	return []byte("ok"), nil
}

func (captureCodec) Verify(llotypes.ChannelDefinition) error { return nil }

var _ protocol.ReportCodec = captureCodec{}

// Test_CalculatedStreams_ReportValues pins the contract report assembly and
// ReportCodecEVMABIEncodeUnpackedExpr share: a report carries the channel's
// observed values followed by one calculated value per declared expression, in
// declaration order, even though the channel definition lists only the observed
// streams.
func Test_CalculatedStreams_ReportValues(t *testing.T) {
	ctx := tests.Context(t)
	cid := llotypes.ChannelID(5)

	var got protocol.Report
	p := testPlugin(t)
	p.ReportCodecs = map[llotypes.ReportFormat]protocol.ReportCodec{
		llotypes.ReportFormatEVMABIEncodeUnpackedExpr: captureCodec{got: &got},
	}

	definitions := llotypes.ChannelDefinitions{cid: {
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Opts: []byte(`{"abi":[
			{"type":"int256","expression":"Add(s1, s2)","expressionStreamID":998},
			{"type":"int256","expression":"Sub(s1, s2)","expressionStreamID":999}
		]}`),
		Streams: []llotypes.Stream{
			{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
		},
	}}

	prec := precursor{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: 3 * uint64(time.Second),
		ChannelDefinitions:              definitions,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{cid: uint64(time.Second)},
		StreamAggregates: protocol.StreamAggregates{
			1: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(3))},
			2: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(4))},
		},
	}

	cache := protocol.NewOptsCache()
	cache.ResetTo(definitions)
	calculated.ProcessCalculatedStreams(p.Logger, prec.ChannelDefinitions, prec.StreamAggregates, prec.ObservationTimestampNanoseconds, cache, nil)

	precBytes, err := encodePrecursor(prec.withSupport(3))
	require.NoError(t, err)
	reports, err := p.Reports(ctx, 2, precBytes)
	require.NoError(t, err)
	require.Len(t, reports, 1)

	require.Len(t, got.Values, 4)
	want := []int64{3, 4, 7, -1}
	for i, w := range want {
		d, ok := got.Values[i].(*protocol.Decimal)
		require.True(t, ok, "value %d has type %T", i, got.Values[i])
		require.True(t, d.Decimal().Equal(decimal.NewFromInt(w)), "value %d: expected %d, got %s", i, w, d.Decimal())
	}
}

// Test_Observation_RejectsInlineStreamValues pins the wire contract: a peer that
// inlines stream values is not speaking v31 framing and its observation is
// dropped rather than silently accepted.
func Test_Observation_RejectsInlineStreamValues(t *testing.T) {
	ctx := tests.Context(t)
	sv, err := streamValuesToProto(protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(42))})
	require.NoError(t, err)
	mainBytes, err := proto.Marshal(&protocol.LLOObservationProto{
		UnixTimestampNanoseconds: 1,
		StreamValues:             sv,
	})
	require.NoError(t, err)

	_, err = decodeObservation(ctx, frameObservation(nil, mainBytes), nil, nil)
	require.ErrorContains(t, err, "inline stream values")

	// Deterministic across oracles, so it must not be a blob-fetch failure.
	var bfErr *blobFetchError
	require.NotErrorAs(t, err, &bfErr)
}

func Test_IsReportable_EffectiveStreamsFailure(t *testing.T) {
	// Reportability and emission share one derivation: isReportable and Reports
	// both go through protocol.EffectiveStreams. A channel whose opts cannot be
	// decoded has no derivable stream list, so Reports could not assemble
	// values for it and reportability must agree, otherwise validAfter advances
	// over a round that emitted nothing.
	cd := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		Opts:         []byte(`{"not":`),
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	prec := precursor{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: 2_000_000_000,
		ChannelDefinitions:              llotypes.ChannelDefinitions{1: cd},
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{1: 1_000_000_000},
		StreamAggregates: protocol.StreamAggregates{
			100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
		},
	}
	require.Empty(t, prec.withSupport(1).reportableChannels(0, 0, protocol.NewOptsCache(), nil))
}

func Test_SelectBackfillCandidate_UnemittableRow(t *testing.T) {
	const (
		targetCID   = llotypes.ChannelID(10)
		backfillCID = llotypes.ChannelID(20)
		tenSec      = uint64(10_000_000_000)
	)
	targetCD := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}

	// Row carries stream 999, not the target's stream 100, so
	// BuildBackfillStreamValues would fail in Reports. The candidate must not
	// be selectable: the watermark would otherwise advance past a row that
	// never emitted, losing it permanently.
	missingStream := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatHistoryBackfill,
		Opts:         []byte(`{"targetChannelId":10,"observations":{"5":{"999":"1.5"}}}`),
	}
	defs := llotypes.ChannelDefinitions{targetCID: targetCD, backfillCID: missingStream}
	_, _, _, ok := selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: 0}, tenSec, backfillCID, nil)
	require.False(t, ok, "row missing a target stream must not be selectable")

	// Same row shape, but the value is not parseable as a stream value.
	badValue := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatHistoryBackfill,
		Opts:         []byte(`{"targetChannelId":10,"observations":{"5":{"100":"not-a-number"}}}`),
	}
	defs[backfillCID] = badValue
	_, _, _, ok = selectBackfillCandidate(defs, map[llotypes.ChannelID]uint64{backfillCID: 0}, tenSec, backfillCID, nil)
	require.False(t, ok, "row with an unparseable value must not be selectable")
}

// withSupport returns a copy of the precursor with n oracles advertising a
// report codec for every format its channels need, so tests that are not about
// the support gate are unaffected by it. Backfill channels are resolved to
// their target's format, which is the one their report is encoded with.
func (o precursor) withSupport(n int) precursor {
	o.SupportByFormat = make(map[llotypes.ReportFormat]int, len(o.ChannelDefinitions))
	for _, cd := range o.ChannelDefinitions {
		o.SupportByFormat[cd.ReportFormat] = n
	}
	return o
}

func Test_ReportFormatSupportGate(t *testing.T) {
	const cid = llotypes.ChannelID(1)
	cd := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	base := func(support int) precursor {
		return precursor{
			LifeCycleStage:                  protocol.LifeCycleStageProduction,
			ObservationTimestampNanoseconds: 2_000_000_000,
			ChannelDefinitions:              llotypes.ChannelDefinitions{cid: cd},
			ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{cid: 1_000_000_000},
			StreamAggregates: protocol.StreamAggregates{
				100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
			},
			SupportByFormat: map[llotypes.ReportFormat]int{llotypes.ReportFormatJSON: support},
		}
	}

	// f=1 requires 2f+1 = 3 advertised supporters: 2f would only guarantee f+1
	// real encoders if none of the advertisements were lies.
	require.Equal(t, []llotypes.ChannelID{cid}, base(3).reportableChannels(0, 1, protocol.NewOptsCache(), nil))
	require.Empty(t, base(2).reportableChannels(0, 1, protocol.NewOptsCache(), nil))
	require.Empty(t, base(0).reportableChannels(0, 1, protocol.NewOptsCache(), nil))

	// A format no oracle advertises is never reportable, however healthy the
	// channel otherwise is.
	noEntry := base(3)
	noEntry.SupportByFormat = map[llotypes.ReportFormat]int{}
	require.Empty(t, noEntry.reportableChannels(0, 1, protocol.NewOptsCache(), nil))

	// Support is keyed by format, not channel: an unrelated format's coverage
	// does not carry the channel.
	wrongFormat := base(0)
	wrongFormat.SupportByFormat = map[llotypes.ReportFormat]int{llotypes.ReportFormatEVMPremiumLegacy: 4}
	require.Empty(t, wrongFormat.reportableChannels(0, 1, protocol.NewOptsCache(), nil))
}

func Test_ReportFormatSupportGate_Backfill(t *testing.T) {
	const (
		targetCID   = llotypes.ChannelID(10)
		backfillCID = llotypes.ChannelID(20)
		tenSec      = uint64(10_000_000_000)
	)
	defs := llotypes.ChannelDefinitions{
		targetCID: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
		backfillCID: {
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Opts:         []byte(`{"targetChannelId":10,"observations":{"5":{"100":"1.5"}}}`),
		},
	}
	base := func(support map[llotypes.ReportFormat]int) precursor {
		return precursor{
			LifeCycleStage:                  protocol.LifeCycleStageProduction,
			ObservationTimestampNanoseconds: tenSec,
			ChannelDefinitions:              defs,
			ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{targetCID: tenSec /* target not reportable */, backfillCID: 0},
			StreamAggregates:                protocol.StreamAggregates{},
			SupportByFormat:                 support,
		}
	}

	// The backfill report is encoded with the TARGET's codec, so the target's
	// format is what must be covered. Coverage of history_backfill itself is
	// irrelevant: no codec encodes it.
	targetCovered := base(map[llotypes.ReportFormat]int{llotypes.ReportFormatJSON: 3})
	require.Equal(t, []llotypes.ChannelID{backfillCID}, targetCovered.reportableChannels(0, 1, protocol.NewOptsCache(), nil))

	backfillCoveredOnly := base(map[llotypes.ReportFormat]int{llotypes.ReportFormatHistoryBackfill: 4})
	require.Empty(t, backfillCoveredOnly.reportableChannels(0, 1, protocol.NewOptsCache(), nil))
}

func Test_ReportFormatSupportGate_StopsValidAfterAdvance(t *testing.T) {
	// The gate's point: an uncovered channel must not have validAfter advance
	// over a round that emitted nothing. reportedLastRound is what the next
	// round reads to decide the advance, so assert on that.
	const cid = llotypes.ChannelID(1)
	cd := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	prec := precursor{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: 2_000_000_000,
		ChannelDefinitions:              llotypes.ChannelDefinitions{cid: cd},
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{cid: 1_000_000_000},
		StreamAggregates: protocol.StreamAggregates{
			100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
		},
	}

	covered := prec
	covered.SupportByFormat = map[llotypes.ReportFormat]int{llotypes.ReportFormatJSON: 3}
	require.True(t, covered.isReportable(cid, 0, 1, protocol.NewOptsCache(), nil))

	uncovered := prec
	uncovered.SupportByFormat = map[llotypes.ReportFormat]int{llotypes.ReportFormatJSON: 2}
	require.False(t, uncovered.isReportable(cid, 0, 1, protocol.NewOptsCache(), nil))
}

func Test_Observation_SupportedReportFormats_RoundTrip(t *testing.T) {
	ctx := tests.Context(t)

	// The set is sorted on the wire and collapses back to a set on decode, so
	// one oracle can only ever contribute one vote per format to the tally.
	obs := Observation{
		UnixTimestampNanoseconds: 1,
		SupportedReportFormats: formatSet(
			llotypes.ReportFormatJSON,
			llotypes.ReportFormatEVMPremiumLegacy,
		),
	}
	b, err := encodeObservation(obs, nil)
	require.NoError(t, err)
	got, err := decodeObservation(ctx, b, nil, nil)
	require.NoError(t, err)
	require.Equal(t, formatSet(llotypes.ReportFormatEVMPremiumLegacy, llotypes.ReportFormatJSON), got.SupportedReportFormats)
	require.Equal(t, []uint32{uint32(llotypes.ReportFormatEVMPremiumLegacy), uint32(llotypes.ReportFormatJSON)}, sortedFormatsToWire(got.SupportedReportFormats))

	// Duplicate wire entries collapse rather than double counting.
	dup, err := proto.Marshal(&protocol.LLOObservationProto{
		UnixTimestampNanoseconds: 1,
		SupportedReportFormats:   []uint32{uint32(llotypes.ReportFormatJSON), uint32(llotypes.ReportFormatJSON)},
	})
	require.NoError(t, err)
	got, err = decodeObservation(ctx, frameObservation(nil, dup), nil, nil)
	require.NoError(t, err)
	require.Equal(t, formatSet(llotypes.ReportFormatJSON), got.SupportedReportFormats)

	// An oracle advertising nothing decodes as advertising nothing, rather than
	// as an empty-but-present list.
	b, err = encodeObservation(Observation{UnixTimestampNanoseconds: 1}, nil)
	require.NoError(t, err)
	got, err = decodeObservation(ctx, b, nil, nil)
	require.NoError(t, err)
	require.Nil(t, got.SupportedReportFormats)

	// Over-length lists are rejected at decode rather than reaching the tally.
	tooMany := make([]uint32, protocol.MaxObservationSupportedReportFormatsLength+1)
	for i := range tooMany {
		tooMany[i] = uint32(i)
	}
	raw, err := proto.Marshal(&protocol.LLOObservationProto{UnixTimestampNanoseconds: 1, SupportedReportFormats: tooMany})
	require.NoError(t, err)
	_, err = decodeObservation(ctx, frameObservation(nil, raw), nil, nil)
	require.ErrorContains(t, err, "advertises too many report formats")
}

func Test_Precursor_SupportByFormat_RoundTrip(t *testing.T) {
	prec := precursor{
		LifeCycleStage: protocol.LifeCycleStageProduction,
		SupportByFormat: map[llotypes.ReportFormat]int{
			llotypes.ReportFormatJSON:             4,
			llotypes.ReportFormatEVMPremiumLegacy: 3,
		},
	}
	// Encoding must be deterministic despite map iteration order: libocr
	// digests the precursor bytes, so two oracles disagreeing on byte order
	// would fail attestation.
	b1, err := encodePrecursor(prec)
	require.NoError(t, err)
	b2, err := encodePrecursor(prec)
	require.NoError(t, err)
	require.Equal(t, b1, b2)

	got, err := decodePrecursor(b1)
	require.NoError(t, err)
	require.Equal(t, prec.SupportByFormat, got.SupportByFormat)
}

func Test_StateTransition_TalliesReportFormatSupport(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	require.NoError(t, writeChannelState(kv, 1, llotypes.ChannelDefinitions{
		1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}},
	}))

	// Two oracles advertise JSON, one advertises nothing: the tally is a count
	// of advertisements, and an oracle that advertises nothing is not counted.
	obsJSON := mustEncodeObs(t, Observation{UnixTimestampNanoseconds: 1, SupportedReportFormats: formatSet(llotypes.ReportFormatJSON)})
	obsNone := mustEncodeObs(t, Observation{UnixTimestampNanoseconds: 1, SupportedReportFormats: formatSet()})
	precBytes, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, obsJSON), ao(1, obsJSON), ao(2, obsNone)}, kv, testBlobs)
	require.NoError(t, err)

	prec, err := decodePrecursor(precBytes)
	require.NoError(t, err)
	require.Equal(t, 2, prec.SupportByFormat[llotypes.ReportFormatJSON])
}

func Test_StateTransition_ObservationTimestampDoesNotRegress(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)

	obsAt := func(ts uint64) []byte {
		return mustEncodeObs(t, Observation{UnixTimestampNanoseconds: ts})
	}
	agreedAt := func(seqNr uint64, aos ...ocrtypes.AttributedObservation) uint64 {
		precBytes, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
		require.NoError(t, err)
		prec, err := decodePrecursor(precBytes)
		require.NoError(t, err)
		return prec.ObservationTimestampNanoseconds
	}

	require.Equal(t, uint64(1_000), agreedAt(2, ao(0, obsAt(1_000)), ao(1, obsAt(1_000)), ao(2, obsAt(1_000))))

	// Node clocks disagree and the quorum that carried the higher stamp is gone:
	// hold the previous timestamp rather than dating a report before a watermark
	// already in use.
	require.Equal(t, uint64(1_000), agreedAt(3, ao(0, obsAt(500)), ao(1, obsAt(500)), ao(2, obsAt(500))))

	// Forward again once the clock passes the floor.
	require.Equal(t, uint64(2_000), agreedAt(4, ao(0, obsAt(2_000)), ao(1, obsAt(2_000)), ao(2, obsAt(2_000))))
}

func Test_Observation_StampsTheRoundNotTheSnapshot(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()
	require.NoError(t, writeChannelState(kv, 1, llotypes.ChannelDefinitions{
		1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}},
	}))
	p.ChannelDefinitionCache = &mockChannelDefinitionCache{}
	p.ShouldRetireCache = &mockShouldRetireCache{}
	bc := newFakeBroadcaster()
	attachPump(t, p, &recordingDataSource{}, bc)

	// Whether or not the pump has a snapshot ready, the stamp is the round time.
	for _, round := range []uint64{2, 3} {
		before := uint64(time.Now().UnixNano()) //nolint:gosec // G115 test clock is positive
		obsBytes, err := p.Observation(ctx, round, ocrtypes.AttributedQuery{}, kv, nil)
		require.NoError(t, err)
		obs, err := decodeObservation(ctx, obsBytes, bc, nil)
		require.NoError(t, err)
		require.GreaterOrEqual(t, obs.UnixTimestampNanoseconds, before)
		require.Eventually(t, func() bool { return p.pump.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	}
}

// Codec support accumulates across rounds, so f oracles omitting their
// advertisement cannot drop a format below the 2f+1 threshold.
//
// Counted per round, support could never exceed the 2f+1 observation quorum, so
// the threshold would demand that every observation in a minimal quorum
// advertise the format and any single omission would make every channel of that
// format unreportable.
func Test_StateTransition_CodecSupportAccumulatesAcrossRounds(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	require.NoError(t, writeChannelState(kv, 1, llotypes.ChannelDefinitions{
		1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}},
	}))

	obsJSON := func(ts uint64) []byte {
		return mustEncodeObs(t, Observation{UnixTimestampNanoseconds: ts, SupportedReportFormats: formatSet(llotypes.ReportFormatJSON)})
	}
	obsNone := func(ts uint64) []byte {
		return mustEncodeObs(t, Observation{UnixTimestampNanoseconds: ts, SupportedReportFormats: formatSet()})
	}

	// All N oracles advertise JSON.
	precBytes, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, obsJSON(10)), ao(1, obsJSON(10)), ao(2, obsJSON(10)), ao(3, obsJSON(10))}, kv, testBlobs)
	require.NoError(t, err)
	prec, err := decodePrecursor(precBytes)
	require.NoError(t, err)
	require.Equal(t, p.N, prec.SupportByFormat[llotypes.ReportFormatJSON])

	// A minimal quorum in which one oracle now omits its advertisement. Oracle
	// 3 contributes nothing this round, but its last advertisement still counts,
	// so the format keeps 2f+1 supporters and the channel stays reportable.
	precBytes, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, obsJSON(20)), ao(1, obsJSON(20)), ao(2, obsNone(20))}, kv, testBlobs)
	require.NoError(t, err)
	prec, err = decodePrecursor(precBytes)
	require.NoError(t, err)
	require.Equal(t, 2*p.F+1, prec.SupportByFormat[llotypes.ReportFormatJSON])
	require.Equal(t, []llotypes.ChannelID{1}, prec.reportableChannels(0, p.F, nil, nil))
}

// One oracle padding its observation with unused report formats must not be
// able to grow the precursor tally: the encoded map is keyed by oracle-chosen
// values, and an oversized one fails to decode on every oracle, which would
// stop reporting DON-wide while StateTransition keeps succeeding.
func Test_StateTransition_PrunesUnusedReportFormatSupport(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	require.NoError(t, writeChannelState(kv, 1, llotypes.ChannelDefinitions{
		1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}},
	}))

	// Each oracle advertises JSON plus a disjoint block of junk formats, so an
	// unpruned tally would hold 3*MaxObservationSupportedReportFormatsLength-2
	// entries.
	padded := func(oracle int) []byte {
		formats := formatSet(llotypes.ReportFormatJSON)
		for i := 1; i < protocol.MaxObservationSupportedReportFormatsLength; i++ {
			formats[llotypes.ReportFormat(1_000_000+oracle*1_000+i)] = struct{}{}
		}
		return mustEncodeObs(t, Observation{UnixTimestampNanoseconds: 1, SupportedReportFormats: formats})
	}
	precBytes, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, padded(0)), ao(1, padded(1)), ao(2, padded(2))}, kv, testBlobs)
	require.NoError(t, err)

	prec, err := decodePrecursor(precBytes)
	require.NoError(t, err)
	require.Equal(t, map[llotypes.ReportFormat]int{llotypes.ReportFormatJSON: 3}, prec.SupportByFormat)
}

// strictJSONCodec is reportcodec.JSONReportCodec with an extra Verify rule this
// build has and the build that admitted the definition did not: the version
// skew that makes a baseline failure on committed state reachable.
type strictJSONCodec struct {
	reportcodec.JSONReportCodec
	rejectStream llotypes.StreamID
}

func (c strictJSONCodec) Verify(cd llotypes.ChannelDefinition) error {
	for _, strm := range cd.Streams {
		if strm.StreamID == c.rejectStream {
			return errors.New("this build rejects stream " + strconv.Itoa(int(strm.StreamID)))
		}
	}
	return c.JSONReportCodec.Verify(cd)
}

// recordingDataSource records the stream IDs it was asked to observe.
type recordingDataSource struct {
	mu   sync.Mutex
	seen map[llotypes.StreamID]struct{}
}

func (d *recordingDataSource) Observe(_ context.Context, sv protocol.StreamValues, _ DSOpts) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[llotypes.StreamID]struct{}{}
	}
	for streamID := range sv {
		d.seen[streamID] = struct{}{}
		sv[streamID] = protocol.ToDecimal(decimal.NewFromInt(1))
	}
	return nil
}

func (d *recordingDataSource) streams() []llotypes.StreamID {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]llotypes.StreamID, 0, len(d.seen))
	for streamID := range d.seen {
		out = append(out, streamID)
	}
	return out
}

// Test_Observation_UnverifiableCommittedChannelIsNotFatal covers the
// version-skew case: a channel committed under an older build fails this
// build's codec.Verify. The node must not halt. It keeps observing and,
// crucially, still votes the offending channel out, which is the only way the
// DON recovers.
func Test_Observation_UnverifiableCommittedChannelIsNotFatal(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	healthy := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	rejected := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 999, Aggregator: llotypes.AggregatorMedian}},
	}

	// Rounds 1-2: both channels are admitted by a build that accepts them.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	addObs := Observation{
		UnixTimestampNanoseconds: 1_000,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: healthy, 2: rejected},
	}
	addAOs := []ocrtypes.AttributedObservation{}
	for i := 0; i < 4; i++ {
		addAOs = append(addAOs, ao(i, mustEncodeObs(t, addObs)))
	}
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addAOs, kv, testBlobs)
	require.NoError(t, err)
	require.Contains(t, kvChannelDefs(t, kv), llotypes.ChannelID(2))

	// Now this node upgrades to a build whose codec rejects channel 2, and the
	// definitions file drops it so the node has something to vote for.
	p.ReportCodecs = map[llotypes.ReportFormat]protocol.ReportCodec{
		llotypes.ReportFormatJSON: strictJSONCodec{rejectStream: 999},
	}
	p.ChannelCache = protocol.NewChannelCache()
	p.ChannelDefinitionCache = &mockChannelDefinitionCache{defs: llotypes.ChannelDefinitions{1: healthy}}
	p.ShouldRetireCache = &mockShouldRetireCache{}
	ds := &recordingDataSource{}
	attachPump(t, p, ds, newFakeBroadcaster())

	obsBytes, err := p.Observation(ctx, 3, ocrtypes.AttributedQuery{}, kv, nil)
	require.NoError(t, err, "a committed definition this build rejects must not halt the node")
	obs, err := decodeObservation(ctx, obsBytes, testBlobs, nil)
	require.NoError(t, err)

	// The removal vote is the recovery path, and it is only cast because the
	// verification failure above was not fatal.
	require.Contains(t, obs.RemoveChannelIDs, llotypes.ChannelID(2))
	require.NotContains(t, obs.UpdateChannelDefinitions, llotypes.ChannelID(2))

	// The rejected channel's streams are still observed: withholding them would
	// only starve the nodes still on the old build, which considers the channel
	// valid and reportable.
	require.Eventually(t, func() bool { return p.pump.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.ElementsMatch(t, []llotypes.StreamID{100, 999}, ds.streams())
}

// Test_FullRound_RecoverFromUnverifiableChannel is the end-to-end recovery:
// every node upgrades to a build that rejects a committed channel, and the DON
// keeps making rounds and votes the channel out.
func Test_FullRound_RecoverFromUnverifiableChannel(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	healthy := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	rejected := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 999, Aggregator: llotypes.AggregatorMedian}},
	}

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	addObs := Observation{
		UnixTimestampNanoseconds: 1_000,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{1: healthy, 2: rejected},
	}
	addAOs := []ocrtypes.AttributedObservation{}
	for i := 0; i < 4; i++ {
		addAOs = append(addAOs, ao(i, mustEncodeObs(t, addObs)))
	}
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addAOs, kv, testBlobs)
	require.NoError(t, err)

	p.ReportCodecs = map[llotypes.ReportFormat]protocol.ReportCodec{
		llotypes.ReportFormatJSON: strictJSONCodec{rejectStream: 999},
	}
	p.ChannelCache = protocol.NewChannelCache()

	// Every oracle votes the rejected channel out, and the round is accepted:
	// the same observation must also pass ValidateObservation on the new build.
	removeObs := Observation{
		UnixTimestampNanoseconds: 2_000,
		RemoveChannelIDs:         map[llotypes.ChannelID]struct{}{2: {}},
		StreamValues:             protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(42))},
	}
	removeAOs := []ocrtypes.AttributedObservation{}
	for i := 0; i < 4; i++ {
		removeAOs = append(removeAOs, ao(i, mustEncodeObs(t, removeObs)))
	}
	for _, aObs := range removeAOs {
		require.NoError(t, p.ValidateObservation(ctx, 3, ocrtypes.AttributedQuery{}, aObs, kv, testBlobs))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, removeAOs, kv, testBlobs)
	require.NoError(t, err)
	require.NotContains(t, kvChannelDefs(t, kv), llotypes.ChannelID(2))
	require.Contains(t, kvChannelDefs(t, kv), llotypes.ChannelID(1))
}

// Test_Observation_RetirementCacheErrorsAreNotFatal guards the liveness fix:
// both retirement caches read from node-local, asynchronously populated state,
// so a transient failure must not fail the round. The vote is best-effort — a
// single node supplying a valid retirement report is enough, and retirement
// needs a quorum of votes — so the node abstains and carries on.
func Test_Observation_RetirementCacheErrorsAreNotFatal(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	predecessor := ocrtypes.ConfigDigest{0xAB}
	p.PredecessorConfigDigest = &predecessor
	p.PredecessorRetirementReportCache = &mockPredecessorRetirementReportCache{err: errors.New("rpc failure")}
	p.ShouldRetireCache = &mockShouldRetireCache{retire: true, err: errors.New("rpc failure")}
	p.ChannelDefinitionCache = &mockChannelDefinitionCache{defs: llotypes.ChannelDefinitions{}}
	kv := newMemKV()

	// Bootstrap -> staging, which is the only stage that reads the predecessor
	// retirement report cache.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)

	obsBytes, err := p.Observation(ctx, 2, ocrtypes.AttributedQuery{}, kv, nil)
	require.NoError(t, err, "a failing retirement cache must not halt the node")
	obs, err := decodeObservation(ctx, obsBytes, testBlobs, nil)
	require.NoError(t, err)

	require.Empty(t, obs.AttestedPredecessorRetirement)
	require.False(t, obs.ShouldRetire)
}

func Test_UnreportableTally_CollapsesPerChannelWarnings(t *testing.T) {
	const channels = 50
	out := precursor{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: 1_000,
		ChannelDefinitions:              llotypes.ChannelDefinitions{},
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{},
		StreamAggregates:                protocol.StreamAggregates{},
	}
	for i := 1; i <= channels; i++ {
		cid := llotypes.ChannelID(i) //nolint:gosec // G115 bounded by the loop
		out.ChannelDefinitions[cid] = llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
		}
		out.ValidAfterNanoseconds[cid] = 1
	}

	// No oracle advertises the format, so every channel fails the same check.
	tally := &unreportableTally{}
	require.Empty(t, out.withSupport(0).reportableChannels(0, 1, protocol.NewOptsCache(), tally))

	require.Len(t, tally.reasons, 1, "one reason, not one entry per channel")
	reason := tally.reasons["too few oracles advertise a report codec for this format"]
	require.NotNil(t, reason)
	require.Equal(t, channels, reason.count)
	require.Len(t, reason.channels, maxUnreportableSamples, "the sample is bounded")
	require.Equal(t, []any{"reportFormat", llotypes.ReportFormatJSON, "supporters", 0, "required", 3}, reason.detail)

	// A nil tally is the no-op the predicate-only callers pass.
	require.Empty(t, out.withSupport(0).reportableChannels(0, 1, protocol.NewOptsCache(), nil))
}

// Test_StateTransition_BackfillValidAfterRequiresPreviousReport covers the
// backfill watermark advancing only over a row the previous round actually
// reported.
func Test_StateTransition_BackfillValidAfterRequiresPreviousReport(t *testing.T) {
	const (
		targetCID   = llotypes.ChannelID(10)
		backfillCID = llotypes.ChannelID(20)
		fiveSec     = uint64(5_000_000_000)
		tenSec      = uint64(10_000_000_000)
	)
	targetCD := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
	backfillCD := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatHistoryBackfill,
		Opts:         []byte(`{"targetChannelId":10,"observations":{"5":{"100":"1.5"},"8":{"100":"2.5"}}}`),
	}

	// round builds four identical observations at ts.
	round := func(t *testing.T, ts uint64, shape func(*Observation)) []ocrtypes.AttributedObservation {
		t.Helper()
		aos := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			obs := Observation{UnixTimestampNanoseconds: ts}
			if shape != nil {
				shape(&obs)
			}
			aos = append(aos, ao(i, mustEncodeObs(t, obs)))
		}
		return aos
	}

	for _, tc := range []struct {
		name string
		// defs is what the verdict round starts from; shape is how its
		// observations differ from a healthy round.
		defs  llotypes.ChannelDefinitions
		shape func(*Observation)
		want  uint64
	}{
		{
			// Guard 1: a retired instance emits nothing, and retirement is
			// terminal, so an unguarded advance drains the whole backfill.
			name:  "retired instance",
			defs:  llotypes.ChannelDefinitions{targetCID: targetCD, backfillCID: backfillCD},
			shape: func(obs *Observation) { obs.ShouldRetire = true },
			want:  0,
		},
		{
			// Guard 2: selection checks that the channel exists but not that
			// it is live, so a tombstone is invisible to it.
			name: "tombstoned backfill channel",
			defs: func() llotypes.ChannelDefinitions {
				tombstoned := backfillCD
				tombstoned.Tombstone = true
				return llotypes.ChannelDefinitions{targetCID: targetCD, backfillCID: tombstoned}
			}(),
			want: 0,
		},
		{
			// The support gate: no oracle advertises a codec for the target's
			// format, so the verdict round could not certify a report. Codec
			// coverage is not an input to selection. Coverage returns in the
			// next round, which must still not skip the row.
			name: "target format not encodable",
			defs: llotypes.ChannelDefinitions{targetCID: targetCD, backfillCID: backfillCD},
			shape: func(obs *Observation) {
				obs.SupportedReportFormats = formatSet(llotypes.ReportFormatHistoryBackfill)
			},
			want: 0,
		},
		{
			// Selection reads this round's definitions against the previous
			// round's watermark and timestamp. The verdict round has no target
			// channel, so nothing was selectable and nothing was reported; the
			// vote it agrees adds one, which makes the same row selectable in
			// the round that decides the advance.
			name: "target channel added since the verdict",
			defs: llotypes.ChannelDefinitions{backfillCID: backfillCD},
			shape: func(obs *Observation) {
				obs.UpdateChannelDefinitions = llotypes.ChannelDefinitions{targetCID: targetCD}
			},
			want: 0,
		},
		{
			// Control: the verdict round did report, so the watermark moves to
			// the row it emitted.
			name: "previous round reported",
			defs: llotypes.ChannelDefinitions{targetCID: targetCD, backfillCID: backfillCD},
			want: fiveSec,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tests.Context(t)
			p := testPlugin(t)
			kv := newMemKV()

			// State the verdict round starts from. A candidate is selectable
			// throughout (watermark 0, observations at 5s and 8s), so the
			// advance turns entirely on the verdict.
			require.NoError(t, writeLifecycle(kv, protocol.LifeCycleStageProduction))
			require.NoError(t, writeChannelState(kv, 1, tc.defs))
			require.NoError(t, writeHotState(kv, tenSec,
				map[llotypes.ChannelID]uint64{targetCID: tenSec, backfillCID: 0},
				map[llotypes.ChannelID]bool{targetCID: false, backfillCID: false},
				nil, logger.Test(t)))

			// The verdict round.
			_, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, round(t, tenSec+1, tc.shape), kv, testBlobs)
			require.NoError(t, err)
			require.Equal(t, uint64(0), readHotStateForTest(t, kv).validAfterNanoseconds[backfillCID],
				"the verdict round must not have advanced the watermark itself")

			// The round that decides the advance.
			precursorBytes, err := p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, round(t, tenSec+2, nil), kv, testBlobs)
			require.NoError(t, err)

			out, err := decodePrecursor(precursorBytes)
			require.NoError(t, err)
			require.Equal(t, tc.want, out.ValidAfterNanoseconds[backfillCID])
		})
	}
}

// readHotStateForTest decodes the r/agg record written by the last round.
func readHotStateForTest(t *testing.T, kv *memKV) *kvState {
	t.Helper()
	s := &kvState{
		validAfterNanoseconds: map[llotypes.ChannelID]uint64{},
		reportedLastRound:     map[llotypes.ChannelID]bool{},
		carryForward:          map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{},
	}
	require.NoError(t, readHotState(kv, s))
	return s
}
