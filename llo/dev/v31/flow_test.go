package llo

import (
	"context"
	"errors"
	"math"
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

type mockShouldRetireCache struct {
	retire bool
	err    error
}

func (m *mockShouldRetireCache) ShouldRetire(ocrtypes.ConfigDigest) (bool, error) {
	return m.retire, m.err
}

type mockOnchainConfigCodec struct{}

func (mockOnchainConfigCodec) Decode([]byte) (protocol.OnchainConfig, error) {
	return protocol.OnchainConfig{}, nil
}
func (mockOnchainConfigCodec) Encode(protocol.OnchainConfig) ([]byte, error) { return nil, nil }

// testPredecessorSigners is the signer set a fixture node reads from its local
// retirement report cache and votes on.
var testPredecessorSigners = [][]byte{{0x01}, {0x02}, {0x03}, {0x04}}

type mockPredecessorRetirementReportCache struct {
	report protocol.RetirementReport
	err    error
	// noLocalConfig models a node whose config poller has not stored the
	// predecessor config yet, so it cannot vote on the signer set.
	noLocalConfig bool
	// signers overrides testPredecessorSigners; f is the predecessor's f.
	signers [][]byte
	f       uint8
	// verifyErr makes VerifyAttestedRetirementReport fail deterministically.
	verifyErr error
}

func (m *mockPredecessorRetirementReportCache) localSigners() [][]byte {
	if m.signers != nil {
		return m.signers
	}
	return testPredecessorSigners
}

func (m *mockPredecessorRetirementReportCache) AttestedRetirementReport(ocrtypes.ConfigDigest) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return []byte("attested"), nil
}
func (m *mockPredecessorRetirementReportCache) CheckAttestedRetirementReport(ocrtypes.ConfigDigest, []byte) (protocol.RetirementReport, error) {
	return m.report, nil
}
func (m *mockPredecessorRetirementReportCache) PredecessorConfig(ocrtypes.ConfigDigest) ([][]byte, uint8, bool) {
	if m.noLocalConfig {
		return nil, 0, false
	}
	return m.localSigners(), m.f, true
}
func (m *mockPredecessorRetirementReportCache) VerifyAttestedRetirementReport(_ ocrtypes.ConfigDigest, signers [][]byte, f uint8, _ []byte) (protocol.RetirementReport, error) {
	if m.verifyErr != nil {
		return protocol.RetirementReport{}, m.verifyErr
	}
	// Verification must run against the agreed set, never the local one.
	if len(signers) == 0 {
		return protocol.RetirementReport{}, errors.New("verify called with an empty signer set")
	}
	return m.report, nil
}

// promotionObs is what a staging node observes once its predecessor has
// retired: the attested retirement report, plus a vote for the predecessor's
// signer set so the DON can agree on it and verify the report against it.
func promotionObs(tsNanoseconds uint64) Observation {
	return Observation{
		UnixTimestampNanoseconds:      tsNanoseconds,
		AttestedPredecessorRetirement: []byte("attested"),
		PredecessorSigners:            testPredecessorSigners,
	}
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

// mustEncodeOffchainConfig encodes a v0 offchain config with an explicit
// aggregation fault tolerance, which LLO v31 requires.
func mustEncodeOffchainConfig(t *testing.T, aggregationFaultTolerance uint32) []byte {
	t.Helper()
	b, err := protocol.OffchainConfig{AggregationFaultTolerance: &aggregationFaultTolerance}.Encode()
	require.NoError(t, err)
	return b
}

func Test_Factory_NewReportingPlugin_aggregationFaultTolerance(t *testing.T) {
	ctx := tests.Context(t)
	f := NewPluginFactory(PluginFactoryParams{
		OnchainConfigCodec: mockOnchainConfigCodec{},
		Logger:             logger.Test(t),
	})

	t.Run("refuses to start when unset", func(t *testing.T) {
		_, _, err := f.NewReportingPlugin(ctx, ocr3types.ReportingPluginConfig{N: 4, F: 1, ConfigDigest: ocrtypes.ConfigDigest{9}}, nil)
		require.EqualError(t, err, "NewReportingPlugin: offchain config must set aggregationFaultTolerance explicitly")
	})
	t.Run("refuses to start when it exceeds consensus F", func(t *testing.T) {
		_, _, err := f.NewReportingPlugin(ctx, ocr3types.ReportingPluginConfig{N: 4, F: 1, ConfigDigest: ocrtypes.ConfigDigest{9}, OffchainConfig: mustEncodeOffchainConfig(t, 2)}, nil)
		require.EqualError(t, err, "aggregationFaultTolerance (2) must not exceed consensus F (1): a floor of 5 contributions can never be met from 3 observations")
	})
	t.Run("accepts zero", func(t *testing.T) {
		p, _, err := f.NewReportingPlugin(ctx, ocr3types.ReportingPluginConfig{N: 4, F: 1, ConfigDigest: ocrtypes.ConfigDigest{9}, OffchainConfig: mustEncodeOffchainConfig(t, 0)}, nil)
		require.NoError(t, err)
		pl, ok := p.(*Plugin)
		require.True(t, ok)
		require.Equal(t, 1, pl.minContributions())
		require.NoError(t, pl.Close())
	})
}

func Test_Factory_NewReportingPlugin(t *testing.T) {
	ctx := tests.Context(t)
	f := NewPluginFactory(PluginFactoryParams{
		OnchainConfigCodec: mockOnchainConfigCodec{},
		Logger:             logger.Test(t),
	})
	p, info, err := f.NewReportingPlugin(ctx, ocr3types.ReportingPluginConfig{N: 4, F: 1, ConfigDigest: ocrtypes.ConfigDigest{9}, OffchainConfig: mustEncodeOffchainConfig(t, 1)}, nil)
	require.NoError(t, err)

	info1, ok := info.(interface{ Validate() error })
	require.True(t, ok)
	require.NoError(t, info1.Validate())

	pl, ok := p.(*Plugin)
	require.True(t, ok)
	require.Equal(t, 4, pl.N)
	require.Equal(t, 1, pl.F)
	require.Equal(t, 1, pl.AggregationFaultTolerance)
	require.Equal(t, 3, pl.minContributions())
	require.NotNil(t, pl.ChannelCache)
	require.NotNil(t, pl.pump)
	require.Equal(t, uint64(DefaultMaxSnapshotRounds), pl.pump.maxSnapshotRounds)
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
	_, err = p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 1, jsonChannel()), kv, testBlobs)
	require.NoError(t, err)

	// Observation at seqNr=3: channel 1 is now in KV, so the pump is fed. The
	// first round finds nothing parked yet (the pump runs off the critical path)
	// and returns an observation with votes only; it kicks a cycle whose snapshot
	// the next round picks up.
	first, err := p.Observation(ctx, 3, ocrtypes.AttributedQuery{}, kv, nil)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	decodedFirst, err := decodeObservation(ctx, first, bc, nil)
	require.NoError(t, err)
	require.Empty(t, decodedFirst.StreamValues)

	require.Eventually(t, func() bool { return p.pump.Cycles() >= 1 }, tests.WaitTimeout(t), 10*time.Millisecond)
	require.Positive(t, ds.observeCount(), "DataSource.Observe should have been called by the pump")
	require.Positive(t, bc.Broadcasts(), "stream values must be disseminated as a blob")

	obsBytes, err := p.Observation(ctx, 4, ocrtypes.AttributedQuery{}, kv, nil)
	require.NoError(t, err)
	require.NotEmpty(t, obsBytes)
	decoded, err := decodeObservation(ctx, obsBytes, bc, nil)
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

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 1, jsonChannel()), kv, testBlobs)
	require.NoError(t, err)
	require.Contains(t, kvChannelDefs(t, kv), llotypes.ChannelID(1))

	// Round 3: four oracles vote to remove channel 1.
	removeObs := Observation{UnixTimestampNanoseconds: 2000, RemoveChannelIDs: map[llotypes.ChannelID]struct{}{1: {}}}
	removeAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		removeAOs = append(removeAOs, ao(i, mustEncodeObs(t, removeObs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, removeAOs, kv, testBlobs)
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
	_, err = p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, nextAOs, kv, testBlobs)
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
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, boot, kv, testBlobs)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageStaging), string(kv.m[string(keyLifecycle)]))

	// A round carrying a valid attested predecessor retirement report promotes to production.
	promoObs := promotionObs(1000)
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, promoObs)))
	}
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
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
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)

	// Round 2 (ts=1000): staging adds its own channel 2, which is absent from the
	// predecessor's retirement report. The addition is deferred, so it has no
	// watermark yet.
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 2, jsonChannel()), kv, testBlobs)
	require.NoError(t, err)
	require.NotContains(t, kvHotState(t, kv).validAfterNanoseconds, llotypes.ChannelID(2))

	// Round 3 (ts=2000): channel 2 is now in effect and gets its first watermark.
	seedObs := Observation{UnixTimestampNanoseconds: 2000}
	seedAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		seedAOs = append(seedAOs, ao(i, mustEncodeObs(t, seedObs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, seedAOs, kv, testBlobs)
	require.NoError(t, err)
	require.Equal(t, uint64(2000), kvHotState(t, kv).validAfterNanoseconds[2])

	// Round 4 (ts=3000): a valid attested predecessor retirement report promotes
	// this instance to production.
	promoObs := promotionObs(3000)
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, mustEncodeObs(t, promoObs)))
	}
	_, err = p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, aos, kv, testBlobs)
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

	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 1000, 1, jsonChannel()), kv, testBlobs)
	require.NoError(t, err)

	// Round 3: four oracles vote to retire.
	retireObs := Observation{UnixTimestampNanoseconds: 2000, ShouldRetire: true}
	retireAOs := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		retireAOs = append(retireAOs, ao(i, mustEncodeObs(t, retireObs)))
	}
	prec, err := p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, retireAOs, kv, testBlobs)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageRetired), string(kv.m[string(keyLifecycle)]))

	// Reports emits a retirement report (and nothing else, since we're retired).
	reports, err := p.Reports(ctx, 3, prec)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, llotypes.ReportFormatRetirement, reports[0].ReportWithInfo.Info.ReportFormat)
	require.Equal(t, protocol.LifeCycleStageRetired, reports[0].ReportWithInfo.Info.LifeCycleStage)
}

// Test_ValidateObservation_RemoveAddSwapAtBudget pins that the set verified is
// the one the observation advocates: a swap that keeps the definition set
// inside a whole-set budget must validate, even though the committed set plus
// the update alone exceeds it. Ignoring the removals would reject every honest
// observation voting the swap, and with it the round's observation quorum.
func Test_ValidateObservation_RemoveAddSwapAtBudget(t *testing.T) {
	ctx := tests.Context(t)
	p := testPlugin(t)
	kv := newMemKV()

	// streamRangeChannel builds a channel holding n distinct stream IDs starting
	// at first, so the unique-stream-ID budget can be sat exactly on.
	streamRangeChannel := func(first llotypes.StreamID, n int) llotypes.ChannelDefinition {
		streams := make([]llotypes.Stream, 0, n)
		for i := 0; i < n; i++ {
			streams = append(streams, llotypes.Stream{StreamID: first + llotypes.StreamID(i), Aggregator: llotypes.AggregatorMedian})
		}
		return llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Streams: streams}
	}

	half := protocol.MaxObservationStreamValuesLength / 2
	committed := llotypes.ChannelDefinitions{
		1: streamRangeChannel(1, half),
		2: streamRangeChannel(llotypes.StreamID(half)+1, half),
	}
	require.NoError(t, protocol.VerifyChannelDefinitions(p.ReportCodecs, committed), "committed set must sit exactly on the budget")
	require.NoError(t, writeChannelState(kv, 1, committed))

	// Swap channel 2 out for channel 3, which holds as many streams as the one
	// it replaces, so the resulting set is back on the budget, not over it.
	replacement := llotypes.ChannelDefinitions{3: streamRangeChannel(llotypes.StreamID(2*half)+1, half)}
	swap := Observation{
		UnixTimestampNanoseconds: 1_000,
		RemoveChannelIDs:         map[llotypes.ChannelID]struct{}{2: {}},
		UpdateChannelDefinitions: replacement,
	}
	require.NoError(t, p.ValidateObservation(ctx, 2, ocrtypes.AttributedQuery{}, ao(0, mustEncodeObs(t, swap)), kv, testBlobs))

	// The same update without the removal vote does exceed the budget, which is
	// what makes the assertion above about the removals and not about slack in
	// the limit.
	addOnly := Observation{UnixTimestampNanoseconds: 1_000, UpdateChannelDefinitions: replacement}
	require.Error(t, p.ValidateObservation(ctx, 2, ocrtypes.AttributedQuery{}, ao(0, mustEncodeObs(t, addOnly)), kv, testBlobs))
}

// stagedPlugin returns a staging plugin with a predecessor configured, plus its
// bootstrapped KV.
func stagedPlugin(t *testing.T, prrc *mockPredecessorRetirementReportCache) (*Plugin, *memKV) {
	t.Helper()
	ctx := tests.Context(t)
	p := testPlugin(t)
	predecessor := ocrtypes.ConfigDigest{0xAB}
	p.PredecessorConfigDigest = &predecessor
	p.PredecessorRetirementReportCache = prrc
	kv := newMemKV()
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageStaging), string(kv.m[string(keyLifecycle)]))
	return p, kv
}

// promotionRoundAOs builds a round of four observations with distinct
// timestamps, all carrying an attested retirement report, of which the first
// voters many vote for the predecessor's signer set. The timestamps are chosen
// so the median moves if the last observation is dropped: kept gives 3000,
// dropped gives 2000.
func promotionRoundAOs(t *testing.T, voters int) []ocrtypes.AttributedObservation {
	t.Helper()
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		obs := promotionObs(uint64(1000 * (i + 1))) //nolint:gosec // small test constant
		if i >= voters {
			// This node's config poller has not caught up, so it abstains from
			// the signer-set vote but observes everything else as usual.
			obs.PredecessorSigners = nil
		}
		aos = append(aos, ao(i, mustEncodeObs(t, obs)))
	}
	return aos
}

// Test_StateTransition_PredecessorConfigAgreedAndUsedSameRound covers
// the signer set needed to verify an attested retirement report being agreed by
// vote rather than read from the node-local cache, within the round that
// carries the report, so the handover costs no extra round.
func Test_StateTransition_PredecessorConfigAgreedAndUsedSameRound(t *testing.T) {
	ctx := tests.Context(t)
	p, kv := stagedPlugin(t, &mockPredecessorRetirementReportCache{
		report: protocol.RetirementReport{ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 500}},
	})

	_, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, promotionRoundAOs(t, 4), kv, testBlobs)
	require.NoError(t, err)

	require.Equal(t, string(protocol.LifeCycleStageProduction), string(kv.m[string(keyLifecycle)]))
}

// Test_StateTransition_LaggingPollersDoNotFork is the finding itself: nodes
// whose config poller has not stored the predecessor config cannot verify
// locally. Their observations must still count in full, and the round must
// produce the same state as one where every node was caught up, since f+1
// voters are enough to agree on the signer set.
func Test_StateTransition_LaggingPollersDoNotFork(t *testing.T) {
	ctx := tests.Context(t)
	report := protocol.RetirementReport{ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 500}}

	pAll, kvAll := stagedPlugin(t, &mockPredecessorRetirementReportCache{report: report})
	precursorAll, err := pAll.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, promotionRoundAOs(t, 4), kvAll, testBlobs)
	require.NoError(t, err)

	// Only f+1 = 2 of the 4 nodes had the config; the other two abstained.
	pLagging, kvLagging := stagedPlugin(t, &mockPredecessorRetirementReportCache{report: report})
	precursorLagging, err := pLagging.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, promotionRoundAOs(t, 2), kvLagging, testBlobs)
	require.NoError(t, err)

	require.Equal(t, precursorAll, precursorLagging, "a lagging poller must not change the state transition")
	require.Equal(t, string(protocol.LifeCycleStageProduction), string(kvLagging.m[string(keyLifecycle)]))

	// The abstaining nodes' observations still counted: the median timestamp is
	// over all four, not just the two that voted.
	out, err := decodePrecursor(precursorLagging)
	require.NoError(t, err)
	require.Equal(t, uint64(3000), out.ObservationTimestampNanoseconds)
}

// Test_StateTransition_PredecessorConfigNeedsQuorum checks the vote threshold:
// a single voter is not enough to elect a signer set, so no report is verified
// and the instance stays in staging. The round itself still completes.
func Test_StateTransition_PredecessorConfigNeedsQuorum(t *testing.T) {
	ctx := tests.Context(t)
	p, kv := stagedPlugin(t, &mockPredecessorRetirementReportCache{
		report: protocol.RetirementReport{ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 500}},
	})

	precursor, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, promotionRoundAOs(t, 1), kv, testBlobs)
	require.NoError(t, err)

	require.Equal(t, string(protocol.LifeCycleStageStaging), string(kv.m[string(keyLifecycle)]))

	out, err := decodePrecursor(precursor)
	require.NoError(t, err)
	require.Equal(t, uint64(3000), out.ObservationTimestampNanoseconds)
}

// Test_StateTransition_PredecessorConfigAgreementIsPerRound guards the forgery
// path: agreement is scoped to the round that uses it and nothing carries over,
// so a coalition of f can never build on an earlier round to install a signer
// set of its own and attest a handover that never happened.
func Test_StateTransition_PredecessorConfigAgreementIsPerRound(t *testing.T) {
	ctx := tests.Context(t)
	p, kv := stagedPlugin(t, &mockPredecessorRetirementReportCache{report: protocol.RetirementReport{}})

	// Round 2 reaches quorum on the real signer set but carries no retirement
	// report, so the instance stays in staging and the agreement is discarded.
	votesOnly := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		votesOnly = append(votesOnly, ao(i, mustEncodeObs(t, Observation{UnixTimestampNanoseconds: 1000, PredecessorSigners: testPredecessorSigners})))
	}
	_, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, votesOnly, kv, testBlobs)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageStaging), string(kv.m[string(keyLifecycle)]))

	// Round 3: a single byzantine oracle presents its own signer set and a
	// report attested by it. One vote is not more than f, and round 2 left
	// nothing behind to lean on, so nothing is elected and nothing is verified.
	attacker := [][]byte{{0xFF}, {0xFE}}
	forged := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		obs := Observation{UnixTimestampNanoseconds: 2000}
		if i == 0 {
			obs.AttestedPredecessorRetirement = []byte("attested")
			obs.PredecessorSigners = attacker
		}
		forged = append(forged, ao(i, mustEncodeObs(t, obs)))
	}
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, forged, kv, testBlobs)
	require.NoError(t, err)
	require.Equal(t, string(protocol.LifeCycleStageStaging), string(kv.m[string(keyLifecycle)]),
		"a coalition of f must not promote the instance")
}

// Test_StateTransition_InvalidRetirementReport_KeepsObservation covers the
// verification against the agreed signer set fails identically on every oracle,
// so only the retirement report is ignored: the observation's timestamp,
// votes and stream values still count. Otherwise one malformed field would
// silently remove an oracle from the round.
func Test_StateTransition_InvalidRetirementReport_KeepsObservation(t *testing.T) {
	ctx := tests.Context(t)
	p, kv := stagedPlugin(t, &mockPredecessorRetirementReportCache{verifyErr: errors.New("Verify failed; not enough valid signatures")})

	precursor, err := p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, promotionRoundAOs(t, 4), kv, testBlobs)
	require.NoError(t, err, "a deterministic verification failure must not fail the round")
	require.Equal(t, string(protocol.LifeCycleStageStaging), string(kv.m[string(keyLifecycle)]),
		"an unverifiable retirement report must not promote")

	// Every observation still counted: the median is over all four timestamps.
	out, err := decodePrecursor(precursor)
	require.NoError(t, err)
	require.Equal(t, uint64(3000), out.ObservationTimestampNanoseconds)
}

// Test_Observation_PredecessorConfigVote covers the observation side: a staging
// node votes its local signer set alongside an attested retirement report,
// abstains when its poller has nothing, and never votes without a report to
// verify.
func Test_Observation_PredecessorConfigVote(t *testing.T) {
	ctx := tests.Context(t)

	// Observation reads the node-local caches; the vote is the only one under
	// test, so the rest just answer.
	stagedObserver := func(t *testing.T, prrc *mockPredecessorRetirementReportCache) (*Plugin, *memKV) {
		t.Helper()
		p, kv := stagedPlugin(t, prrc)
		p.ShouldRetireCache = &mockShouldRetireCache{}
		p.ChannelDefinitionCache = &mockChannelDefinitionCache{defs: llotypes.ChannelDefinitions{}}
		return p, kv
	}

	observe := func(t *testing.T, p *Plugin, kv *memKV) Observation {
		t.Helper()
		obsBytes, err := p.Observation(ctx, 2, ocrtypes.AttributedQuery{}, kv, nil)
		require.NoError(t, err)
		obs, err := decodeObservation(ctx, obsBytes, testBlobs, nil)
		require.NoError(t, err)
		return obs
	}

	t.Run("votes alongside an attested retirement report", func(t *testing.T) {
		p, kv := stagedObserver(t, &mockPredecessorRetirementReportCache{})
		require.Equal(t, testPredecessorSigners, observe(t, p, kv).PredecessorSigners)
	})

	t.Run("abstains when the poller has not caught up", func(t *testing.T) {
		p, kv := stagedObserver(t, &mockPredecessorRetirementReportCache{noLocalConfig: true})
		obs := observe(t, p, kv)
		require.NotEmpty(t, obs.AttestedPredecessorRetirement, "a lagging poller must not drop the report")
		require.Empty(t, obs.PredecessorSigners)
	})

	t.Run("does not vote without a report to verify", func(t *testing.T) {
		p, kv := stagedObserver(t, &mockPredecessorRetirementReportCache{err: errors.New("no attested retirement report yet")})
		obs := observe(t, p, kv)
		require.Empty(t, obs.AttestedPredecessorRetirement)
		require.Empty(t, obs.PredecessorSigners, "the signer set is dead weight without a report")
	})
}

// Test_EncodeObservation_IsDeterministic covers the encoder builds repeated fields
// and proto maps from Go maps, so without deterministic marshaling two oracles could
// produce different bytes for the same logical observation.
// Nothing compares observation bytes today and this keeps a future path that does from being silently wrong.
func Test_EncodeObservation_IsDeterministic(t *testing.T) {
	obs := Observation{
		UnixTimestampNanoseconds:      1234,
		AttestedPredecessorRetirement: []byte("attested"),
		PredecessorSigners:            testPredecessorSigners,
		PredecessorF:                  1,
		RemoveChannelIDs:              map[llotypes.ChannelID]struct{}{7: {}, 1: {}, 4: {}, 2: {}, 9: {}},
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{
			5: jsonChannel(), 3: jsonChannel(), 8: jsonChannel(), 1: jsonChannel(),
		},
		SupportedReportFormats: testSupportedReportFormats,
	}

	want, err := encodeObservation(obs, [][]byte{[]byte("handle")})
	require.NoError(t, err)
	for range 64 {
		got, err := encodeObservation(obs, [][]byte{[]byte("handle")})
		require.NoError(t, err)
		require.Equal(t, want, got, "the same logical observation must encode to the same bytes")
	}
}

// Test_MarshalStreamValues_IsDeterministic pins the same property for the blob
// payload, which is content-addressed: the same values must hash to the same
// handle on every oracle, or the round's fetch memo cannot dedupe.
func Test_MarshalStreamValues_IsDeterministic(t *testing.T) {
	sv := protocol.StreamValues{}
	for id := llotypes.StreamID(1); id <= 32; id++ {
		sv[id] = protocol.ToDecimal(decimal.NewFromInt(int64(id)))
	}

	want, err := marshalStreamValues(sv)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	for range 64 {
		got, err := marshalStreamValues(sv)
		require.NoError(t, err)
		require.Equal(t, want, got, "the same stream values must marshal to the same payload")
	}
}

// Test_Observation_EmptyDesiredSetRemovesOnlyTombstones covers that
// the onchain cache reaps a tombstone by having the owner omit it,
// so an all-tombstoned committed set legitimately merges to empty,
// and abstaining there would strand those channels forever.
func Test_Observation_EmptyDesiredSetRemovesOnlyTombstones(t *testing.T) {
	ctx := tests.Context(t)

	live := jsonChannel()
	tombstoned := jsonChannel()
	tombstoned.Tombstone = true

	// observeRemovals commits defs, then asks what the node votes to remove
	// once the definitions source has gone empty.
	observeRemovals := func(t *testing.T, defs llotypes.ChannelDefinitions) map[llotypes.ChannelID]struct{} {
		t.Helper()
		p := testPlugin(t)
		p.ShouldRetireCache = &mockShouldRetireCache{}
		cdc := &mockChannelDefinitionCache{defs: defs}
		p.ChannelDefinitionCache = cdc
		kv := newMemKV()

		_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
		require.NoError(t, err)
		agree := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			agree = append(agree, ao(i, mustEncodeObs(t, Observation{UnixTimestampNanoseconds: 1000, UpdateChannelDefinitions: defs})))
		}
		_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, agree, kv, testBlobs)
		require.NoError(t, err)
		require.Len(t, kvChannelDefs(t, kv), len(defs))

		cdc.defs = llotypes.ChannelDefinitions{}
		obsBytes, err := p.Observation(ctx, 3, ocrtypes.AttributedQuery{}, kv, nil)
		require.NoError(t, err)
		obs, err := decodeObservation(ctx, obsBytes, testBlobs, nil)
		require.NoError(t, err)
		return obs.RemoveChannelIDs
	}

	t.Run("live channels are never removed by an empty set", func(t *testing.T) {
		require.Empty(t, observeRemovals(t, llotypes.ChannelDefinitions{1: live, 2: live}))
	})

	t.Run("tombstoned channels still are", func(t *testing.T) {
		require.Equal(t,
			map[llotypes.ChannelID]struct{}{2: {}},
			observeRemovals(t, llotypes.ChannelDefinitions{1: live, 2: tombstoned}))
	})

	t.Run("an all-tombstoned set can be reaped to empty", func(t *testing.T) {
		require.Equal(t,
			map[llotypes.ChannelID]struct{}{1: {}, 2: {}},
			observeRemovals(t, llotypes.ChannelDefinitions{1: tombstoned, 2: tombstoned}))
	})

	t.Run("a non-empty desired set is still a genuine opinion", func(t *testing.T) {
		p := testPlugin(t)
		p.ShouldRetireCache = &mockShouldRetireCache{}
		cdc := &mockChannelDefinitionCache{defs: llotypes.ChannelDefinitions{1: live, 2: live}}
		p.ChannelDefinitionCache = cdc
		kv := newMemKV()

		_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{ao(0, nil), ao(1, nil), ao(2, nil)}, kv, testBlobs)
		require.NoError(t, err)
		agree := make([]ocrtypes.AttributedObservation, 0, 4)
		for i := 0; i < 4; i++ {
			agree = append(agree, ao(i, mustEncodeObs(t, Observation{UnixTimestampNanoseconds: 1000, UpdateChannelDefinitions: cdc.defs})))
		}
		_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, agree, kv, testBlobs)
		require.NoError(t, err)

		// Dropping one channel while still reporting the other is an opinion,
		// so the dropped one is voted off even though it is live.
		cdc.defs = llotypes.ChannelDefinitions{1: live}
		obsBytes, err := p.Observation(ctx, 3, ocrtypes.AttributedQuery{}, kv, nil)
		require.NoError(t, err)
		obs, err := decodeObservation(ctx, obsBytes, testBlobs, nil)
		require.NoError(t, err)
		require.Equal(t, map[llotypes.ChannelID]struct{}{2: {}}, obs.RemoveChannelIDs)
	})
}

// Test_IsReportable_MinReportIntervalDoesNotOverflow
// validAfter is a nanosecond wall-clock timestamp and the offchain config
// bounds DefaultMinReportIntervalNanoseconds only away from zero, so a large
// enough interval used to wrap validAfter+minReportInterval to a small number.
// The cadence comparison then passed for every channel and the interval
// silently stopped gating anything.
func Test_IsReportable_MinReportIntervalDoesNotOverflow(t *testing.T) {
	const validAfter = uint64(1_700_000_000_000_000_000)

	out := precursor{
		LifeCycleStage:                  protocol.LifeCycleStageProduction,
		ObservationTimestampNanoseconds: validAfter + 1,
		ChannelDefinitions:              llotypes.ChannelDefinitions{1: jsonChannel()},
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{1: validAfter},
		StreamAggregates: protocol.StreamAggregates{
			100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
		},
		SupportByFormat: map[llotypes.ReportFormat]int{llotypes.ReportFormatJSON: 4},
	}
	gen, err := protocol.NewChannelCache().Load(1, func() (llotypes.ChannelDefinitions, error) {
		return out.ChannelDefinitions, nil
	})
	require.NoError(t, err)
	optsCache := gen.Opts()

	// One nanosecond past validAfter, so only the interval can hold it back.
	require.True(t, out.isReportable(1, 1, 1, optsCache, nil),
		"a one nanosecond interval must not gate a report one nanosecond late")

	// An interval that overflows the sum must gate, not wrap into passing.
	for _, interval := range []uint64{math.MaxUint64, math.MaxUint64 - validAfter + 1} {
		require.False(t, out.isReportable(1, interval, 1, optsCache, nil),
			"interval %d must gate the report, not wrap", interval)
	}

	require.Equal(t, uint64(math.MaxUint64), saturatingAdd(validAfter, math.MaxUint64))
	require.Equal(t, uint64(9), saturatingAdd(4, 5))
}
