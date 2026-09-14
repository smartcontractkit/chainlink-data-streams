package llo

import (
	"testing"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec"
	"github.com/smartcontractkit/chainlink-data-streams/llo/retirement"
	v30 "github.com/smartcontractkit/chainlink-data-streams/llo/v30"
)

// Blue/green handover between the v3.0 (OCR3.0) and v3.1 (OCR3.1) plugins.
//
// The two plugins are separate protocol instances with separate config digests.
// They hand over through the shared, version-agnostic machinery in llo/protocol
// and llo/retirement: the retiring instance emits a ReportFormatRetirement
// report carrying its ValidAfterNanoseconds map, the transmitter stores the
// attested form in the global RetirementReportCache, and the successor's staging
// instance observes it and promotes itself to production seeded with exactly
// those watermarks.
//
// These tests drive both plugins in one process and assert that the handover is
// gapless and non-overlapping in both directions, i.e. that for every channel
// the (validAfter, observationTimestamp] intervals of the reports emitted by the
// predecessor and the successor are contiguous and disjoint.

const (
	handoverChannelID  = llotypes.ChannelID(1)
	handoverStreamID   = llotypes.StreamID(100)
	handoverTickNanos  = uint64(1_000_000_000) // 1s per round
	handoverProtocolV  = uint32(1)
	handoverV30SeqNrRR = uint64(42) // seqNr the retirement report is attested at
)

// emittedReport is a single report emitted by either plugin, decoded back into
// the interval it covers.
type emittedReport struct {
	instance   string
	seqNr      uint64
	channelID  llotypes.ChannelID
	validAfter uint64
	obsTS      uint64
	specimen   bool
}

func handoverChannel() llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: handoverStreamID, Aggregator: llotypes.AggregatorMedian}},
	}
}

func handoverStreamValues() protocol.StreamValues {
	return protocol.StreamValues{handoverStreamID: protocol.ToDecimal(decimal.NewFromInt(123))}
}

// --- v3.0 driver ---

// v30Instance drives a v3.0 plugin round by round, feeding it four identical
// observations per round and collecting the reports it emits.
type v30Instance struct {
	t            *testing.T
	p            *v30.Plugin
	obsCodec     v30.ObservationCodec
	prev         ocr3types.Outcome
	seqNr        uint64
	reports      []emittedReport
	retirementRR []byte // raw retirement report, once retired
}

func newV30Instance(t *testing.T, digest ocrtypes.ConfigDigest, predecessor *ocrtypes.ConfigDigest, prrc protocol.PredecessorRetirementReportCache) *v30Instance {
	obsCodec, err := v30.NewProtoObservationCodec(logger.Test(t), false)
	require.NoError(t, err)
	return &v30Instance{
		t:        t,
		obsCodec: obsCodec,
		p: &v30.Plugin{
			Config:                           v30.Config{VerboseLogging: true},
			ConfigDigest:                     digest,
			PredecessorConfigDigest:          predecessor,
			PredecessorRetirementReportCache: prrc,
			ShouldRetireCache:                &handoverShouldRetireCache{},
			Logger:                           logger.Test(t),
			N:                                4,
			F:                                1,
			ObservationCodec:                 obsCodec,
			OutcomeCodec:                     v30.GetOutcomeCodec(protocol.OffchainConfig{ProtocolVersion: handoverProtocolV}),
			RetirementReportCodec:            protocol.StandardRetirementReportCodec{},
			ReportCodecs: map[llotypes.ReportFormat]protocol.ReportCodec{
				llotypes.ReportFormatJSON: reportcodec.JSONReportCodec{},
			},
			OptsCache:       protocol.NewOptsCache(),
			ProtocolVersion: handoverProtocolV,
		},
	}
}

// round runs one full Outcome + Reports cycle on the given observation.
func (v *v30Instance) round(obs v30.Observation) {
	v.t.Helper()
	ctx := tests.Context(v.t)
	encoded, err := v.obsCodec.Encode(obs)
	require.NoError(v.t, err)

	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := 0; i < 4; i++ {
		aos = append(aos, ao(i, encoded))
	}

	v.seqNr++
	outctx := ocr3types.OutcomeContext{SeqNr: v.seqNr, PreviousOutcome: v.prev}
	outcome, err := v.p.Outcome(ctx, outctx, nil, aos)
	require.NoError(v.t, err)
	v.prev = outcome

	rwis, err := v.p.Reports(ctx, v.seqNr, outcome)
	require.NoError(v.t, err)
	for _, rwi := range rwis {
		if rwi.ReportWithInfo.Info.ReportFormat == llotypes.ReportFormatRetirement {
			v.retirementRR = rwi.ReportWithInfo.Report
			continue
		}
		v.reports = append(v.reports, decodeJSONReport(v.t, "v30", v.seqNr, rwi))
	}
}

// outcome decodes the instance's latest outcome.
func (v *v30Instance) outcome() v30.Outcome {
	v.t.Helper()
	out, err := v.p.OutcomeCodec.Decode(v.prev)
	require.NoError(v.t, err)
	return out
}

// --- v3.1 driver ---

// v31Instance drives a v3.1 plugin round by round against an in-memory KV.
type v31Instance struct {
	t       *testing.T
	p       *Plugin
	kv      *memKV
	seqNr   uint64
	reports []emittedReport
	// retirementRR is the raw retirement report, once retired.
	retirementRR []byte
}

func newV31Instance(t *testing.T, digest ocrtypes.ConfigDigest, predecessor *ocrtypes.ConfigDigest, prrc protocol.PredecessorRetirementReportCache) *v31Instance {
	p := testPlugin(t)
	p.ConfigDigest = digest
	p.PredecessorConfigDigest = predecessor
	p.PredecessorRetirementReportCache = prrc
	p.ShouldRetireCache = &mockShouldRetireCache{}
	p.RetirementReportCodec = protocol.StandardRetirementReportCodec{}
	p.ProtocolVersion = handoverProtocolV
	return &v31Instance{t: t, p: p, kv: newMemKV()}
}

// round runs one full StateTransition + Reports cycle with all four oracles
// sending the same observation.
func (v *v31Instance) round(obs Observation) {
	v.t.Helper()
	require.NoError(v.t, v.tryRound(obs, obs, obs, obs))
}

// tryRound runs one round with a per-oracle observation each, returning the
// StateTransition error instead of failing, so tests can assert on it.
func (v *v31Instance) tryRound(obs ...Observation) error {
	v.t.Helper()
	ctx := tests.Context(v.t)

	aos := make([]ocrtypes.AttributedObservation, 0, len(obs))
	for i, o := range obs {
		var encoded []byte
		if v.seqNr > 0 { // seqNr==1 observations must be empty
			encoded = mustEncodeObs(v.t, o)
		}
		aos = append(aos, ao(i, encoded))
	}

	v.seqNr++
	precursor, err := v.p.StateTransition(ctx, v.seqNr, ocrtypes.AttributedQuery{}, aos, v.kv, testBlobs)
	if err != nil {
		return err
	}

	rwis, err := v.p.Reports(ctx, v.seqNr, precursor)
	require.NoError(v.t, err)
	for _, rwi := range rwis {
		if rwi.ReportWithInfo.Info.ReportFormat == llotypes.ReportFormatRetirement {
			v.retirementRR = rwi.ReportWithInfo.Report
			continue
		}
		v.reports = append(v.reports, decodeJSONReport(v.t, "v31", v.seqNr, rwi))
	}
	return nil
}

func (v *v31Instance) lifeCycleStage() llotypes.LifeCycleStage {
	return llotypes.LifeCycleStage(v.kv.m[string(keyLifecycle)])
}

func (v *v31Instance) validAfter() map[llotypes.ChannelID]uint64 {
	return kvHotState(v.t, v.kv).validAfterNanoseconds
}

// --- shared doubles ---

// handoverShouldRetireCache lets a test flip the retire vote for the v3.0
// instance. v3.1 reuses the equivalent mockShouldRetireCache from flow_test.go.
type handoverShouldRetireCache struct{ retire bool }

func (m *handoverShouldRetireCache) ShouldRetire(ocrtypes.ConfigDigest) (bool, error) {
	return m.retire, nil
}

// handoverPredecessorSigners is the predecessor's ConfigSet signer set, which
// every staging node reads from its local cache and votes on so the DON can
// agree on it and verify the attested report against it.
var handoverPredecessorSigners = [][]byte{{1}, {2}, {3}, {4}}

// handoverRRCReader stands in for the global RetirementReportCache: it holds the
// predecessor's attested retirement report and its ConfigSet signer set.
type handoverRRCReader struct {
	attested []byte
	config   retirement.Config
}

func (r *handoverRRCReader) AttestedRetirementReport(ocrtypes.ConfigDigest) ([]byte, bool) {
	return r.attested, r.attested != nil
}

func (r *handoverRRCReader) Config(ocrtypes.ConfigDigest) (retirement.Config, bool) {
	return r.config, len(r.config.Signers) > 0
}

// handoverVerifier accepts or rejects every signature wholesale, standing in for
// the onchain keyring. OCR3.1 reuses ocr3types.OnchainKeyring verbatim, so one
// verifier covers both protocol versions.
type handoverVerifier struct{ valid bool }

func (v handoverVerifier) Verify(_ ocrtypes.OnchainPublicKey, _ ocrtypes.ConfigDigest, _ uint64, _ ocr3types.ReportWithInfo[llotypes.ReportInfo], _ []byte) bool {
	return v.valid
}

// attest wraps a raw retirement report in the attested protobuf form the
// transmitter stores, signed by two of four signers (f+1 with F=1).
func attest(t *testing.T, raw []byte) []byte {
	t.Helper()
	b, err := proto.Marshal(&protocol.AttestedRetirementReport{
		RetirementReport: raw,
		SeqNr:            handoverV30SeqNrRR,
		Sigs: []*protocol.AttributedOnchainSignature{
			{Signer: 0, Signature: []byte("sig0")},
			{Signer: 1, Signature: []byte("sig1")},
		},
	})
	require.NoError(t, err)
	return b
}

// newPredecessorCache builds the real plugin-scoped cache over the given
// attested report, so the handover exercises the production protobuf unwrap,
// signature quorum check and shared retirement report codec.
func newPredecessorCache(t *testing.T, attested []byte, sigsValid bool) protocol.PredecessorRetirementReportCache {
	t.Helper()
	reader := &handoverRRCReader{
		attested: attested,
		config: retirement.Config{
			Signers: pq.ByteaArray(handoverPredecessorSigners),
			F:       1,
		},
	}
	return retirement.NewPluginScopedRetirementReportCache(reader, handoverVerifier{valid: sigsValid}, protocol.StandardRetirementReportCodec{})
}

func decodeJSONReport(t *testing.T, instance string, seqNr uint64, rwi ocr3types.ReportPlus[llotypes.ReportInfo]) emittedReport {
	t.Helper()
	r, err := reportcodec.JSONReportCodec{}.Decode(rwi.ReportWithInfo.Report)
	require.NoError(t, err)
	return emittedReport{
		instance:   instance,
		seqNr:      seqNr,
		channelID:  r.ChannelID,
		validAfter: r.ValidAfterNanoseconds,
		obsTS:      r.ObservationTimestampNanoseconds,
		specimen:   r.Specimen,
	}
}

// requireGaplessAndNonOverlapping asserts the fundamental handover invariant:
// concatenated in emission order, the production reports for a channel cover
// (validAfter, obsTS] intervals that are contiguous (no gap) and disjoint (no
// overlap). Specimen reports from the staging instance are ignored: they are not
// consumed downstream.
func requireGaplessAndNonOverlapping(t *testing.T, reports []emittedReport) {
	t.Helper()
	perChannel := map[llotypes.ChannelID][]emittedReport{}
	for _, r := range reports {
		if r.specimen {
			continue
		}
		perChannel[r.channelID] = append(perChannel[r.channelID], r)
	}
	require.NotEmpty(t, perChannel, "no production reports were emitted")

	for cid, rs := range perChannel {
		require.Greater(t, len(rs), 1, "channel %d: need reports from both instances to check the boundary", cid)
		for i, r := range rs {
			// Zero-width intervals are tolerated: with protocolVersion>0 and no
			// min report interval, v3.0 emits one on the round a channel is
			// first added (validAfter == obsTS). Inverted intervals are not.
			require.LessOrEqual(t, r.validAfter, r.obsTS, "channel %d report %d: inverted interval", cid, i)
			if i == 0 {
				continue
			}
			prev := rs[i-1]
			require.Equal(t, prev.obsTS, r.validAfter,
				"channel %d: %s report at seqNr %d does not resume exactly where %s report at seqNr %d left off",
				cid, r.instance, r.seqNr, prev.instance, prev.seqNr)
		}
	}
}

// requireCrossesInstances asserts the report stream actually spans the handover,
// so a test cannot pass by only ever exercising one plugin.
func requireCrossesInstances(t *testing.T, reports []emittedReport, first, second string) {
	t.Helper()
	var sawFirst, sawSecondAfterFirst bool
	for _, r := range reports {
		if r.specimen {
			continue
		}
		switch r.instance {
		case first:
			require.False(t, sawSecondAfterFirst, "%s emitted a production report after %s took over", first, second)
			sawFirst = true
		case second:
			if sawFirst {
				sawSecondAfterFirst = true
			}
		}
	}
	require.True(t, sawFirst, "%s never emitted a production report", first)
	require.True(t, sawSecondAfterFirst, "%s never took over from %s", second, first)
}

// --- tests ---

// Test_Handover_V30ToV31 is the migration path for a live DON: a production v3.0
// instance retires and a staging v3.1 instance takes over its watermarks.
func Test_Handover_V30ToV31(t *testing.T) {
	v30Digest := ocrtypes.ConfigDigest{0x30}
	v31Digest := ocrtypes.ConfigDigest{0x31}

	// --- v3.0 runs as the production instance ---
	old := newV30Instance(t, v30Digest, nil, nil)
	ts := handoverTickNanos

	// Round 1: bootstrap into production (no predecessor).
	old.round(v30.Observation{UnixTimestampNanoseconds: ts})
	require.Equal(t, protocol.LifeCycleStageProduction, old.outcome().LifeCycleStage)

	// Round 2: agree to add the channel (takes effect next round).
	ts += handoverTickNanos
	old.round(v30.Observation{
		UnixTimestampNanoseconds: ts,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
		StreamValues:             handoverStreamValues(),
	})

	// Rounds 3-5: the channel is in effect and reporting.
	for range 3 {
		ts += handoverTickNanos
		old.round(v30.Observation{UnixTimestampNanoseconds: ts, StreamValues: handoverStreamValues()})
	}
	require.NotEmpty(t, old.reports, "v3.0 must be emitting reports before the handover")

	// --- v3.1 comes up alongside as the staging instance ---
	// It has no retirement report to observe yet, so it stays in staging and its
	// reports are specimens.
	stagingCache := newPredecessorCache(t, nil, true)
	newInst := newV31Instance(t, v31Digest, &v30Digest, stagingCache)

	newInst.round(Observation{}) // bootstrap -> staging
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage())

	stagingTS := ts
	stagingTS += handoverTickNanos
	newInst.round(Observation{
		UnixTimestampNanoseconds: stagingTS,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
		StreamValues:             handoverStreamValues(),
	})
	for range 2 {
		stagingTS += handoverTickNanos
		newInst.round(Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})
	}
	for _, r := range newInst.reports {
		require.True(t, r.specimen, "staging instance must only emit specimen reports")
	}

	// --- retire v3.0 ---
	ts += handoverTickNanos
	old.round(v30.Observation{
		UnixTimestampNanoseconds: ts,
		ShouldRetire:             true,
		StreamValues:             handoverStreamValues(),
	})
	require.Equal(t, protocol.LifeCycleStageRetired, old.outcome().LifeCycleStage)
	require.NotEmpty(t, old.retirementRR, "retired v3.0 instance must emit a retirement report")

	// The retirement report carries exactly the retiring instance's watermarks.
	handedOver, err := protocol.StandardRetirementReportCodec{}.Decode(old.retirementRR)
	require.NoError(t, err)
	require.Equal(t, handoverProtocolV, handedOver.ProtocolVersion)
	require.Equal(t, old.outcome().ValidAfterNanoseconds, handedOver.ValidAfterNanoseconds)
	require.Contains(t, handedOver.ValidAfterNanoseconds, handoverChannelID)

	// --- v3.1 observes the attested report and promotes ---
	promoted := newPredecessorCache(t, attest(t, old.retirementRR), true)
	newInst.p.PredecessorRetirementReportCache = promoted

	attestedBytes, err := promoted.AttestedRetirementReport(v30Digest)
	require.NoError(t, err)
	require.NotEmpty(t, attestedBytes)

	stagingTS += handoverTickNanos
	newInst.round(Observation{
		UnixTimestampNanoseconds:      stagingTS,
		AttestedPredecessorRetirement: attestedBytes,
		PredecessorSigners:            handoverPredecessorSigners,
		PredecessorF:                  1,
		StreamValues:                  handoverStreamValues(),
	})
	require.Equal(t, protocol.LifeCycleStageProduction, newInst.lifeCycleStage())

	// Watermarks were seeded verbatim from the predecessor: no gap, no overlap.
	require.Equal(t, handedOver.ValidAfterNanoseconds[handoverChannelID], newInst.validAfter()[handoverChannelID])

	// Subsequent rounds emit real (non-specimen) reports resuming exactly at the
	// predecessor's last observation timestamp.
	for range 3 {
		stagingTS += handoverTickNanos
		newInst.round(Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})
	}

	all := append(append([]emittedReport{}, old.reports...), newInst.reports...)
	requireCrossesInstances(t, all, "v30", "v31")
	requireGaplessAndNonOverlapping(t, all)
}

// Test_Handover_V31ToV30 is the rollback path: a production v3.1 instance
// retires back to a staging v3.0 instance.
func Test_Handover_V31ToV30(t *testing.T) {
	v31Digest := ocrtypes.ConfigDigest{0x31}
	v30Digest := ocrtypes.ConfigDigest{0x30}

	// --- v3.1 runs as the production instance ---
	old := newV31Instance(t, v31Digest, nil, nil)
	ts := handoverTickNanos

	old.round(Observation{}) // bootstrap -> production (no predecessor)
	require.Equal(t, protocol.LifeCycleStageProduction, old.lifeCycleStage())

	ts += handoverTickNanos
	old.round(Observation{
		UnixTimestampNanoseconds: ts,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
		StreamValues:             handoverStreamValues(),
	})
	for range 3 {
		ts += handoverTickNanos
		old.round(Observation{UnixTimestampNanoseconds: ts, StreamValues: handoverStreamValues()})
	}
	require.NotEmpty(t, old.reports, "v3.1 must be emitting reports before the handover")
	for _, r := range old.reports {
		require.False(t, r.specimen, "production instance must not emit specimen reports")
	}

	// --- v3.0 comes up alongside as the staging instance ---
	newInst := newV30Instance(t, v30Digest, &v31Digest, newPredecessorCache(t, nil, true))
	stagingTS := ts

	newInst.round(v30.Observation{UnixTimestampNanoseconds: stagingTS})
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.outcome().LifeCycleStage)

	stagingTS += handoverTickNanos
	newInst.round(v30.Observation{
		UnixTimestampNanoseconds: stagingTS,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
		StreamValues:             handoverStreamValues(),
	})
	for range 2 {
		stagingTS += handoverTickNanos
		newInst.round(v30.Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})
	}
	for _, r := range newInst.reports {
		require.True(t, r.specimen, "staging instance must only emit specimen reports")
	}

	// --- retire v3.1 ---
	old.p.ShouldRetireCache = &mockShouldRetireCache{retire: true}
	ts += handoverTickNanos
	old.round(Observation{UnixTimestampNanoseconds: ts, ShouldRetire: true, StreamValues: handoverStreamValues()})
	require.Equal(t, protocol.LifeCycleStageRetired, old.lifeCycleStage())
	require.NotEmpty(t, old.retirementRR, "retired v3.1 instance must emit a retirement report")

	handedOver, err := protocol.StandardRetirementReportCodec{}.Decode(old.retirementRR)
	require.NoError(t, err)
	require.Contains(t, handedOver.ValidAfterNanoseconds, handoverChannelID)

	// --- v3.0 observes the attested report and promotes ---
	promoted := newPredecessorCache(t, attest(t, old.retirementRR), true)
	newInst.p.PredecessorRetirementReportCache = promoted
	attestedBytes, err := promoted.AttestedRetirementReport(v31Digest)
	require.NoError(t, err)

	stagingTS += handoverTickNanos
	newInst.round(v30.Observation{
		UnixTimestampNanoseconds:      stagingTS,
		AttestedPredecessorRetirement: attestedBytes,
		StreamValues:                  handoverStreamValues(),
	})
	require.Equal(t, protocol.LifeCycleStageProduction, newInst.outcome().LifeCycleStage)
	require.Equal(t, handedOver.ValidAfterNanoseconds[handoverChannelID], newInst.outcome().ValidAfterNanoseconds[handoverChannelID])

	for range 3 {
		stagingTS += handoverTickNanos
		newInst.round(v30.Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})
	}

	all := append(append([]emittedReport{}, old.reports...), newInst.reports...)
	requireCrossesInstances(t, all, "v31", "v30")
	requireGaplessAndNonOverlapping(t, all)
}

// Test_Handover_V30ToV31_RejectsBadSignatures guards the security boundary: a
// retirement report that does not carry f+1 valid signatures from the
// predecessor's signer set must not promote the successor.
//
// Two oracles send the bad attested report and two send none, so the round still
// has valid observations to work with; see
// Test_Handover_AllObservationsCarryBadRetirement for what happens when every
// oracle sends one.
func Test_Handover_V30ToV31_RejectsBadSignatures(t *testing.T) {
	v30Digest := ocrtypes.ConfigDigest{0x30}
	raw, err := protocol.StandardRetirementReportCodec{}.Encode(protocol.RetirementReport{
		ProtocolVersion:       handoverProtocolV,
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{handoverChannelID: 500},
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		attested []byte
		valid    bool
	}{
		{"signatures do not verify", attest(t, raw), false},
		{"not enough signatures", func() []byte {
			b, merr := proto.Marshal(&protocol.AttestedRetirementReport{
				RetirementReport: raw,
				SeqNr:            handoverV30SeqNrRR,
				Sigs:             []*protocol.AttributedOnchainSignature{{Signer: 0, Signature: []byte("sig0")}},
			})
			require.NoError(t, merr)
			return b
		}(), true},
		{"malformed attested report", []byte("not a protobuf"), true},
		{"signer index out of bounds", func() []byte {
			b, merr := proto.Marshal(&protocol.AttestedRetirementReport{
				RetirementReport: raw,
				SeqNr:            handoverV30SeqNrRR,
				Sigs: []*protocol.AttributedOnchainSignature{
					{Signer: 0, Signature: []byte("sig0")},
					{Signer: 99, Signature: []byte("sig99")},
				},
			})
			require.NoError(t, merr)
			return b
		}(), true},
		{"duplicate signer", func() []byte {
			b, merr := proto.Marshal(&protocol.AttestedRetirementReport{
				RetirementReport: raw,
				SeqNr:            handoverV30SeqNrRR,
				Sigs: []*protocol.AttributedOnchainSignature{
					{Signer: 0, Signature: []byte("sig0")},
					{Signer: 0, Signature: []byte("sig0")},
				},
			})
			require.NoError(t, merr)
			return b
		}(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newInst := newV31Instance(t, ocrtypes.ConfigDigest{0x31}, &v30Digest, newPredecessorCache(t, tc.attested, tc.valid))

			newInst.round(Observation{})
			require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage())

			bad := Observation{UnixTimestampNanoseconds: handoverTickNanos, AttestedPredecessorRetirement: tc.attested, PredecessorSigners: handoverPredecessorSigners, PredecessorF: 1}
			clean := Observation{UnixTimestampNanoseconds: handoverTickNanos}
			require.NoError(t, newInst.tryRound(bad, bad, clean, clean))

			require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage(),
				"instance must stay in staging when the predecessor retirement report is not valid")
			require.NotContains(t, newInst.validAfter(), handoverChannelID)
		})
	}
}

// Test_Handover_BadRetirementDoesNotStallRound covers the case where EVERY
// oracle attaches an attested retirement report the successor cannot verify —
// the normal case, since honest oracles all attach the same bytes, e.g. while
// the predecessor's ConfigSet row is still missing from the local
// RetirementReportCache. Only the retirement field is dropped: the round still
// completes on the remaining observation contents, and the instance simply stays
// in staging until the report becomes verifiable.
func Test_Handover_BadRetirementDoesNotStallRound(t *testing.T) {
	v30Digest := ocrtypes.ConfigDigest{0x30}
	raw, err := protocol.StandardRetirementReportCodec{}.Encode(protocol.RetirementReport{
		ProtocolVersion:       handoverProtocolV,
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{handoverChannelID: 500},
	})
	require.NoError(t, err)

	// Signatures never verify.
	cache := newPredecessorCache(t, attest(t, raw), false)
	newInst := newV31Instance(t, ocrtypes.ConfigDigest{0x31}, &v30Digest, cache)
	newInst.round(Observation{})
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage())

	// The round completes and the rest of the observation still counts: the
	// channel vote carried alongside the unverifiable report takes effect.
	bad := Observation{
		UnixTimestampNanoseconds:      handoverTickNanos,
		AttestedPredecessorRetirement: attest(t, raw),
		PredecessorSigners:            handoverPredecessorSigners,
		PredecessorF:                  1,
		UpdateChannelDefinitions:      llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
	}
	require.NoError(t, newInst.tryRound(bad, bad, bad, bad))
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage())
	require.Contains(t, kvChannelDefs(t, newInst.kv), handoverChannelID,
		"channel vote carried alongside the unverifiable retirement report must still be counted")

	// Once the report verifies, the very next round promotes.
	newInst.p.PredecessorRetirementReportCache = newPredecessorCache(t, attest(t, raw), true)
	newInst.round(Observation{
		UnixTimestampNanoseconds:      2 * handoverTickNanos,
		AttestedPredecessorRetirement: attest(t, raw),
		PredecessorSigners:            handoverPredecessorSigners,
		PredecessorF:                  1,
	})
	require.Equal(t, protocol.LifeCycleStageProduction, newInst.lifeCycleStage())
	require.Equal(t, uint64(500), newInst.validAfter()[handoverChannelID])
}

// Test_Handover_RejectsIncompatibleProtocolVersion covers the LLO protocol
// version guard: a correctly signed retirement report from a predecessor running
// a protocol version this build does not understand must not promote the
// successor, since its ValidAfterNanoseconds may not mean what the successor
// assumes. The round itself must still complete.
func Test_Handover_RejectsIncompatibleProtocolVersion(t *testing.T) {
	v30Digest := ocrtypes.ConfigDigest{0x30}
	raw, err := protocol.StandardRetirementReportCodec{}.Encode(protocol.RetirementReport{
		ProtocolVersion:       protocol.MaxSupportedProtocolVersion + 1,
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{handoverChannelID: 500},
	})
	require.NoError(t, err)

	newInst := newV31Instance(t, ocrtypes.ConfigDigest{0x31}, &v30Digest, newPredecessorCache(t, attest(t, raw), true))
	newInst.round(Observation{})
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage())

	obs := Observation{UnixTimestampNanoseconds: handoverTickNanos, AttestedPredecessorRetirement: attest(t, raw), PredecessorSigners: handoverPredecessorSigners, PredecessorF: 1}
	require.NoError(t, newInst.tryRound(obs, obs, obs, obs))
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage(),
		"instance must stay in staging on a retirement report from an unsupported protocol version")
	require.NotContains(t, newInst.validAfter(), handoverChannelID)
}

// --- backfill guard ---

const handoverBackfillChannelID = llotypes.ChannelID(10)

// handoverBackfillDefs is the target channel plus a history-backfill channel
// holding three observations, at 1s, 2s and 3s.
func handoverBackfillDefs() llotypes.ChannelDefinitions {
	return llotypes.ChannelDefinitions{
		handoverChannelID: handoverChannel(),
		handoverBackfillChannelID: {
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Streams:      handoverChannel().Streams,
			Opts: []byte(`{"targetChannelId":1,"observations":{` +
				`"1":{"100":"1"},"2":{"100":"2"},"3":{"100":"3"}}}`),
		},
	}
}

// backfillReports returns the reports attributed to the backfill's target
// channel that were emitted by the backfill channel, i.e. those whose interval
// lies wholly in the past relative to the round that emitted them.
func backfillReports(reports []emittedReport, roundTS uint64) (out []emittedReport) {
	for _, r := range reports {
		if r.channelID == handoverChannelID && r.obsTS < roundTS {
			out = append(out, r)
		}
	}
	return out
}

// Test_Handover_StagingSkipsBackfill covers the backfill guard. Both protocol
// instances of a blue/green job share one ChannelDefinitionCache, so the
// backfill channel is present on the staging instance too, where it is a new
// channel with a watermark of 0. Without the guard the staging instance replays
// the whole backfill from the beginning for the length of the overlap window.
func Test_Handover_StagingSkipsBackfill(t *testing.T) {
	v30Digest := ocrtypes.ConfigDigest{0x30}

	run := func(t *testing.T, predecessor *ocrtypes.ConfigDigest) *v31Instance {
		t.Helper()
		inst := newV31Instance(t, ocrtypes.ConfigDigest{0x31}, predecessor, newPredecessorCache(t, nil, true))

		inst.round(Observation{}) // bootstrap
		ts := handoverTickNanos * 10
		inst.round(Observation{
			UnixTimestampNanoseconds: ts,
			UpdateChannelDefinitions: handoverBackfillDefs(),
			StreamValues:             handoverStreamValues(),
		})
		// Definitions are deferred one round, so give it several rounds in
		// effect: enough for the backfill to emit all three observations.
		for range 5 {
			ts += handoverTickNanos
			inst.round(Observation{UnixTimestampNanoseconds: ts, StreamValues: handoverStreamValues()})
		}
		return inst
	}

	t.Run("staging emits no backfill report and holds its watermark", func(t *testing.T) {
		inst := run(t, &v30Digest)
		require.Equal(t, protocol.LifeCycleStageStaging, inst.lifeCycleStage())

		require.Empty(t, backfillReports(inst.reports, handoverTickNanos*10),
			"a staging instance must not replay the backfill")
		require.Equal(t, uint64(0), inst.validAfter()[handoverBackfillChannelID],
			"a staging instance emits no backfill report, so its watermark must not move")
	})

	t.Run("production backfills as usual", func(t *testing.T) {
		inst := run(t, nil)
		require.Equal(t, protocol.LifeCycleStageProduction, inst.lifeCycleStage())

		require.NotEmpty(t, backfillReports(inst.reports, handoverTickNanos*10),
			"a production instance must replay the backfill")
		require.NotZero(t, inst.validAfter()[handoverBackfillChannelID],
			"a production instance must advance the backfill watermark")
	})
}

// Test_Handover_PromotionResumesBackfillAtPredecessorWatermark is why skipping is
// correct rather than merely safe: promotion seeds ValidAfterNanoseconds wholesale
// from the predecessor's retirement report, so the staging instance's backfill
// watermark is discarded unread and the backfill resumes exactly where the
// predecessor left it. Nothing is lost by not replaying it while staging.
func Test_Handover_PromotionResumesBackfillAtPredecessorWatermark(t *testing.T) {
	v30Digest := ocrtypes.ConfigDigest{0x30}
	v31Digest := ocrtypes.ConfigDigest{0x31}

	// --- v3.0 runs as production with the backfill channel, then retires ---
	old := newV30Instance(t, v30Digest, nil, nil)
	ts := handoverTickNanos * 10
	old.round(v30.Observation{UnixTimestampNanoseconds: ts})
	ts += handoverTickNanos
	old.round(v30.Observation{
		UnixTimestampNanoseconds: ts,
		UpdateChannelDefinitions: handoverBackfillDefs(),
		StreamValues:             handoverStreamValues(),
	})
	for range 2 {
		ts += handoverTickNanos
		old.round(v30.Observation{UnixTimestampNanoseconds: ts, StreamValues: handoverStreamValues()})
	}
	require.NotZero(t, old.outcome().ValidAfterNanoseconds[handoverBackfillChannelID],
		"v3.0 must have made backfill progress before retiring")

	ts += handoverTickNanos
	old.round(v30.Observation{
		UnixTimestampNanoseconds: ts,
		ShouldRetire:             true,
		StreamValues:             handoverStreamValues(),
	})
	require.Equal(t, protocol.LifeCycleStageRetired, old.outcome().LifeCycleStage)
	require.NotEmpty(t, old.retirementRR)

	handedOver, err := protocol.StandardRetirementReportCodec{}.Decode(old.retirementRR)
	require.NoError(t, err)
	predecessorWatermark := handedOver.ValidAfterNanoseconds[handoverBackfillChannelID]
	require.NotZero(t, predecessorWatermark, "the retirement report must carry the backfill watermark")

	// --- v3.1 runs as staging over the same definitions, then promotes ---
	newInst := newV31Instance(t, v31Digest, &v30Digest, newPredecessorCache(t, nil, true))
	newInst.round(Observation{}) // bootstrap -> staging
	require.Equal(t, protocol.LifeCycleStageStaging, newInst.lifeCycleStage())

	stagingTS := ts
	stagingTS += handoverTickNanos
	newInst.round(Observation{
		UnixTimestampNanoseconds: stagingTS,
		UpdateChannelDefinitions: handoverBackfillDefs(),
		StreamValues:             handoverStreamValues(),
	})
	for range 3 {
		stagingTS += handoverTickNanos
		newInst.round(Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})
	}
	require.Equal(t, uint64(0), newInst.validAfter()[handoverBackfillChannelID],
		"the staging instance must not have advanced the backfill watermark")

	promoted := newPredecessorCache(t, attest(t, old.retirementRR), true)
	newInst.p.PredecessorRetirementReportCache = promoted
	attestedBytes, err := promoted.AttestedRetirementReport(v30Digest)
	require.NoError(t, err)

	stagingTS += handoverTickNanos
	newInst.round(Observation{
		UnixTimestampNanoseconds:      stagingTS,
		AttestedPredecessorRetirement: attestedBytes,
		PredecessorSigners:            handoverPredecessorSigners,
		PredecessorF:                  1,
		StreamValues:                  handoverStreamValues(),
	})

	require.Equal(t, protocol.LifeCycleStageProduction, newInst.lifeCycleStage())
	require.Equal(t, predecessorWatermark, newInst.validAfter()[handoverBackfillChannelID],
		"promotion must resume the backfill at the predecessor's watermark, not at the staging instance's 0")
}

// Test_Handover_BackfillAbsentFromRetirementReportStartsAtZero pins the existing
// new-channel semantics, which the guard does not change: a backfill channel the
// predecessor never reported is absent from its retirement report and falls
// through to the new-channel path, starting from the beginning. That is the
// correct reading — the predecessor never backfilled it.
func Test_Handover_BackfillAbsentFromRetirementReportStartsAtZero(t *testing.T) {
	v30Digest := ocrtypes.ConfigDigest{0x30}

	// The predecessor ran without the backfill channel, so its retirement report
	// mentions only the regular one.
	old := newV30Instance(t, v30Digest, nil, nil)
	ts := handoverTickNanos * 10
	old.round(v30.Observation{UnixTimestampNanoseconds: ts})
	ts += handoverTickNanos
	old.round(v30.Observation{
		UnixTimestampNanoseconds: ts,
		UpdateChannelDefinitions: llotypes.ChannelDefinitions{handoverChannelID: handoverChannel()},
		StreamValues:             handoverStreamValues(),
	})
	ts += handoverTickNanos
	old.round(v30.Observation{
		UnixTimestampNanoseconds: ts,
		ShouldRetire:             true,
		StreamValues:             handoverStreamValues(),
	})
	require.NotEmpty(t, old.retirementRR)
	handedOver, err := protocol.StandardRetirementReportCodec{}.Decode(old.retirementRR)
	require.NoError(t, err)
	require.NotContains(t, handedOver.ValidAfterNanoseconds, handoverBackfillChannelID)

	// The successor adds the backfill channel itself while staging.
	newInst := newV31Instance(t, ocrtypes.ConfigDigest{0x31}, &v30Digest, newPredecessorCache(t, nil, true))
	newInst.round(Observation{})
	stagingTS := ts + handoverTickNanos
	newInst.round(Observation{
		UnixTimestampNanoseconds: stagingTS,
		UpdateChannelDefinitions: handoverBackfillDefs(),
		StreamValues:             handoverStreamValues(),
	})
	stagingTS += handoverTickNanos
	newInst.round(Observation{UnixTimestampNanoseconds: stagingTS, StreamValues: handoverStreamValues()})

	promoted := newPredecessorCache(t, attest(t, old.retirementRR), true)
	newInst.p.PredecessorRetirementReportCache = promoted
	attestedBytes, err := promoted.AttestedRetirementReport(v30Digest)
	require.NoError(t, err)

	stagingTS += handoverTickNanos
	newInst.round(Observation{
		UnixTimestampNanoseconds:      stagingTS,
		AttestedPredecessorRetirement: attestedBytes,
		PredecessorSigners:            handoverPredecessorSigners,
		PredecessorF:                  1,
		StreamValues:                  handoverStreamValues(),
	})

	require.Equal(t, protocol.LifeCycleStageProduction, newInst.lifeCycleStage())
	require.Equal(t, uint64(0), newInst.validAfter()[handoverBackfillChannelID],
		"a backfill channel the predecessor never reported starts from the beginning")
}
