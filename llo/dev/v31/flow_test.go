package llo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// --- mocks for the plugin dependencies ---

type mockChannelDefinitionCache struct{ defs llotypes.ChannelDefinitions }

func (m *mockChannelDefinitionCache) Definitions(previous llotypes.ChannelDefinitions) llotypes.ChannelDefinitions {
	return m.defs
}
func (m *mockChannelDefinitionCache) Start(context.Context) error    { return nil }
func (m *mockChannelDefinitionCache) Close() error                   { return nil }
func (m *mockChannelDefinitionCache) Ready() error                   { return nil }
func (m *mockChannelDefinitionCache) HealthReport() map[string]error { return nil }
func (m *mockChannelDefinitionCache) Name() string                   { return "mockChannelDefinitionCache" }

// mockDataSource is observed from the blob pump goroutine, so its bookkeeping is
// mutex-guarded.
type mockDataSource struct {
	mu    sync.Mutex
	vals  protocol.StreamValues
	calls int
	err   error
}

func (m *mockDataSource) Observe(ctx context.Context, sv protocol.StreamValues, opts DSOpts) error {
	// Exercise the DSOpts accessors.
	_ = opts.VerboseLogging()
	_ = opts.SeqNr()
	_ = opts.ConfigDigest()
	_ = opts.ObservationTimestamp()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return m.err
	}
	for k, v := range m.vals {
		sv[k] = v
	}
	return nil
}

func (m *mockDataSource) observeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// blockingDataSource blocks in Observe until released, so tests can observe
// how many cycles the pump runs concurrently.
type blockingDataSource struct {
	release  chan struct{}
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	starts   int
}

func (m *blockingDataSource) Observe(ctx context.Context, sv protocol.StreamValues, opts DSOpts) error {
	m.mu.Lock()
	m.starts++
	m.inFlight++
	if m.inFlight > m.maxSeen {
		m.maxSeen = m.inFlight
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
	}()
	select {
	case <-m.release:
	case <-ctx.Done():
	}
	return nil
}

func (m *blockingDataSource) started() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.starts
}

func (m *blockingDataSource) concurrent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxSeen
}

type mockShouldRetireCache struct{ retire bool }

func (m *mockShouldRetireCache) ShouldRetire(ocrtypes.ConfigDigest) (bool, error) {
	return m.retire, nil
}

type mockOnchainConfigCodec struct{}

func (mockOnchainConfigCodec) Decode([]byte) (protocol.OnchainConfig, error) {
	return protocol.OnchainConfig{}, nil
}
func (mockOnchainConfigCodec) Encode(protocol.OnchainConfig) ([]byte, error) { return nil, nil }

type mockPredecessorRetirementReportCache struct{ report protocol.RetirementReport }

func (m *mockPredecessorRetirementReportCache) AttestedRetirementReport(ocrtypes.ConfigDigest) ([]byte, error) {
	return []byte("attested"), nil
}
func (m *mockPredecessorRetirementReportCache) CheckAttestedRetirementReport(ocrtypes.ConfigDigest, []byte) (protocol.RetirementReport, error) {
	return m.report, nil
}

func jsonChannel() llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}}}
}

// addChannelRound feeds 4 identical observations voting to add the given channel.
func addChannelRound(t *testing.T, ts uint64, cid llotypes.ChannelID, cd llotypes.ChannelDefinition) []ocrtypes.AttributedObservation {
	obs := Observation{UnixTimestampNanoseconds: ts, UpdateChannelDefinitions: llotypes.ChannelDefinitions{cid: cd}}
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, obs)))
	}
	return aos
}

// --- tests ---

func Test_Factory_NewReportingPlugin(t *testing.T) {
	ctx := tests.Context(t)
	f := NewPluginFactory(PluginFactoryParams{
		OnchainConfigCodec: mockOnchainConfigCodec{},
		Logger:             logger.Test(t),
	})
	p, info, err := f.NewReportingPlugin(ctx, ocr3types.ReportingPluginConfig{N: 4, F: 1, ConfigDigest: ocrtypes.ConfigDigest{9}}, nil)
	require.NoError(t, err)

	info1, ok := info.(interface{ Validate() error })
	require.True(t, ok)
	require.NoError(t, info1.Validate())

	pl, ok := p.(*Plugin)
	require.True(t, ok)
	require.Equal(t, 4, pl.N)
	require.Equal(t, 1, pl.F)
	require.NotNil(t, pl.ChannelCache)
	require.NotNil(t, pl.pump)
	require.Equal(t, uint64(DefaultBlobLifetimeRounds), pl.pump.blobLifetimeRounds)
	require.NoError(t, pl.Close())
}

func Test_Observation_And_Validate_Flow(t *testing.T) {
	ctx := tests.Context(t)
	ds := &mockDataSource{vals: protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(5))}}
	bc := newFakeBroadcaster()
	p := testPlugin(t)
	p.ChannelDefinitionCache = &mockChannelDefinitionCache{defs: llotypes.ChannelDefinitions{1: jsonChannel()}}
	p.ShouldRetireCache = &mockShouldRetireCache{}
	attachPump(t, p, ds, bc)
	kv := newMemKV()

	// Query is empty; misc callbacks return their fixed values.
	q, err := p.Query(ctx, 2, kv, nil)
	require.NoError(t, err)
	require.Nil(t, q)
	require.NoError(t, p.Committed(ctx, 2, kv))
	acc, err := p.ShouldAcceptAttestedReport(ctx, 2, ocr3types.ReportWithInfo[llotypes.ReportInfo]{})
	require.NoError(t, err)
	require.True(t, acc)
	tr, err := p.ShouldTransmitAcceptedReport(ctx, 2, ocr3types.ReportWithInfo[llotypes.ReportInfo]{})
	require.NoError(t, err)
	require.True(t, tr)

	// Bootstrap, then add channel 1 via a voting round.
	_, err = p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 1, jsonChannel()), kv, nil)
	require.NoError(t, err)

	// Observation at seqNr=3: channel 1 is now in KV, so the pump is fed. The
	// first round finds nothing parked yet (the pump runs off the critical path)
	// and returns an observation with votes only; it kicks a cycle whose snapshot
	// the next round picks up.
	first, err := p.Observation(ctx, 3, ocrtypes.AttributedQuery{}, kv, nil)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	decodedFirst, err := decodeObservation(ctx, first, bc)
	require.NoError(t, err)
	require.Empty(t, decodedFirst.StreamValues)

	require.Eventually(t, func() bool { return p.pump.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Positive(t, ds.observeCount(), "DataSource.Observe should have been called by the pump")
	require.Positive(t, bc.Broadcasts(), "stream values must be disseminated as a blob")

	obsBytes, err := p.Observation(ctx, 4, ocrtypes.AttributedQuery{}, kv, nil)
	require.NoError(t, err)
	require.NotEmpty(t, obsBytes)
	decoded, err := decodeObservation(ctx, obsBytes, bc)
	require.NoError(t, err)
	require.Contains(t, decoded.StreamValues, llotypes.StreamID(100))

	// Quorum + validation of the produced observation.
	aos := []ocrtypes.AttributedObservation{ao(0, obsBytes), ao(1, obsBytes), ao(2, obsBytes)}
	reached, err := p.ObservationQuorum(ctx, 4, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)
	require.True(t, reached)
	require.NoError(t, p.ValidateObservation(ctx, 4, ocrtypes.AttributedQuery{}, ao(0, obsBytes), kv, bc))

	// seqNr==1 observation must be empty.
	require.Error(t, p.ValidateObservation(ctx, 1, ocrtypes.AttributedQuery{}, ao(0, []byte{1}), kv, nil))
}

func Test_StateTransition_ChannelRemoval(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 1, jsonChannel()), kv, nil)
	require.NoError(t, err)
	require.Contains(t, kvChannelDefs(t, kv), llotypes.ChannelID(1))

	// Round 3: four oracles vote to remove channel 1.
	removeObs := Observation{UnixTimestampNanoseconds: 2000, RemoveChannelIDs: map[llotypes.ChannelID]struct{}{1: {}}}
	removeAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		removeAOs = append(removeAOs, ao(i, mustEncodeObs(t, removeObs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, removeAOs, kv, nil)
	require.NoError(t, err)

	// The removal is deferred: the definition is already out of the persisted
	// (pending) set, but the channel was still in effect for round 3, so its
	// round-3 state is still present.
	require.Empty(t, kvChannelDefs(t, kv))
	require.Contains(t, kvHotState(t, kv).validAfterNanoseconds, llotypes.ChannelID(1))

	// Round 4: the removal takes effect and the channel's state is dropped.
	nextObs := Observation{UnixTimestampNanoseconds: 3000}
	nextAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		nextAOs = append(nextAOs, ao(i, mustEncodeObs(t, nextObs)))
	}
	_, err = p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, nextAOs, kv, nil)
	require.NoError(t, err)

	require.Empty(t, kvChannelDefs(t, kv))
	hot := kvHotState(t, kv)
	require.NotContains(t, hot.validAfterNanoseconds, llotypes.ChannelID(1))
	require.NotContains(t, hot.reportedLastRound, llotypes.ChannelID(1))
}

func Test_StateTransition_Promotion(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	predecessor := ocrtypes.ConfigDigest{0xAB}
	p.PredecessorConfigDigest = &predecessor
	p.PredecessorRetirementReportCache = &mockPredecessorRetirementReportCache{
		report: protocol.RetirementReport{ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 500}},
	}
	kv := newMemKV()
	boot := []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}

	// Bootstrap: staging, because a predecessor is configured.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, boot, kv, nil)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageStaging), string(kv.m[string(keyLifecycle)]))

	// A round carrying a valid attested predecessor retirement report promotes to production.
	promoObs := Observation{UnixTimestampNanoseconds: 1000, AttestedPredecessorRetirement: []byte("attested")}
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, promoObs)))
	}
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)

	require.Equal(t, string(protocol.LifeCycleStageProduction), string(kv.m[string(keyLifecycle)]))
	// validAfter is seeded from the predecessor's retirement report (gapless handover).
	require.Equal(t, uint64(500), kvHotState(t, kv).validAfterNanoseconds[1])
}

// Test_StateTransition_Promotion_StagingOnlyChannelTreatedAsNew guards the
// promotion path (Finding 4): a channel the staging instance added itself, that
// is absent from the predecessor's retirement report, was never covered by the
// predecessor's production reports. On promotion it must be reseeded to the
// promotion round's observation timestamp (treated as new), NOT keep its
// carried-forward staging watermark — matching v30.
func Test_StateTransition_Promotion_StagingOnlyChannelTreatedAsNew(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	predecessor := ocrtypes.ConfigDigest{0xAB}
	p.PredecessorConfigDigest = &predecessor
	// The predecessor's retirement report covers channel 1 only.
	p.PredecessorRetirementReportCache = &mockPredecessorRetirementReportCache{
		report: protocol.RetirementReport{ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 500}},
	}
	kv := newMemKV()

	// Bootstrap -> staging.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)

	// Round 2 (ts=1000): staging adds its own channel 2, which is absent from the
	// predecessor's retirement report. The addition is deferred, so it has no
	// watermark yet.
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 2, jsonChannel()), kv, nil)
	require.NoError(t, err)
	require.NotContains(t, kvHotState(t, kv).validAfterNanoseconds, llotypes.ChannelID(2))

	// Round 3 (ts=2000): channel 2 is now in effect and gets its first watermark.
	seedObs := Observation{UnixTimestampNanoseconds: 2000}
	seedAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		seedAOs = append(seedAOs, ao(i, mustEncodeObs(t, seedObs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, seedAOs, kv, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(2000), kvHotState(t, kv).validAfterNanoseconds[2])

	// Round 4 (ts=3000): a valid attested predecessor retirement report promotes
	// this instance to production.
	promoObs := Observation{UnixTimestampNanoseconds: 3000, AttestedPredecessorRetirement: []byte("attested")}
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, promoObs)))
	}
	_, err = p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, aos, kv, nil)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageProduction), string(kv.m[string(keyLifecycle)]))

	// Channel 2 must be reseeded to the promotion round's obs timestamp (3000),
	// NOT keep its carried-forward staging watermark (2000).
	require.Equal(t, uint64(3000), kvHotState(t, kv).validAfterNanoseconds[2],
		"staging-only channel must be treated as new on promotion, not carried forward")
}

func Test_ValidateObservation_Errors(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t) // no predecessor configured
	kv := newMemKV()

	// AttestedPredecessorRetirement present but no predecessor -> error.
	o1 := Observation{UnixTimestampNanoseconds: 1, AttestedPredecessorRetirement: []byte("x")}
	require.Error(t, p.ValidateObservation(ctx, 2, ocrtypes.AttributedQuery{}, ao(0, mustEncodeObs(t, o1)), kv, nil))

	// Too many channel-definition updates -> error.
	defs := llotypes.ChannelDefinitions{}
	for i := uint32(1); i <= 6; i++ {
		defs[llotypes.ChannelID(i)] = jsonChannel()
	}
	o2 := Observation{UnixTimestampNanoseconds: 1, UpdateChannelDefinitions: defs}
	require.Error(t, p.ValidateObservation(ctx, 2, ocrtypes.AttributedQuery{}, ao(0, mustEncodeObs(t, o2)), kv, nil))

	// A TimestampedStreamValue whose nested value is not a Decimal -> error.
	o3 := Observation{UnixTimestampNanoseconds: 1, StreamValues: protocol.StreamValues{
		1: &protocol.TimestampedStreamValue{StreamValue: &protocol.TimestampedStreamValue{StreamValue: protocol.ToDecimal(decimal.NewFromInt(1))}},
	}}
	require.Error(t, p.ValidateObservation(ctx, 2, ocrtypes.AttributedQuery{}, ao(0, mustEncodeObs(t, o3)), kv, nil))
}

func Test_StateTransition_Retirement(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	p.RetirementReportCodec = protocol.StandardRetirementReportCodec{}
	kv := newMemKV()

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, nil)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 1, jsonChannel()), kv, nil)
	require.NoError(t, err)

	// Round 3: four oracles vote to retire.
	retireObs := Observation{UnixTimestampNanoseconds: 2000, ShouldRetire: true}
	retireAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		retireAOs = append(retireAOs, ao(i, mustEncodeObs(t, retireObs)))
	}
	prec, err := p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, retireAOs, kv, nil)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageRetired), string(kv.m[string(keyLifecycle)]))

	// Reports emits a retirement report (and nothing else, since we're retired).
	reports, err := p.Reports(ctx, 3, prec)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, llotypes.ReportFormatRetirement, reports[0].ReportWithInfo.Info.ReportFormat)
	require.Equal(t, protocol.LifeCycleStageRetired, reports[0].ReportWithInfo.Info.LifeCycleStage)
}
