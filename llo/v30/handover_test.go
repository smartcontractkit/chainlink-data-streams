package llo

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/libocr/commontypes"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

var errUnverifiable = errors.New("Verify failed; not enough valid signatures")

// checkingPredecessorRetirementReportCache returns a fixed verdict for any
// attested report handed to it, standing in for the plugin-scoped cache.
type checkingPredecessorRetirementReportCache struct {
	report protocol.RetirementReport
	err    error
}

func (c *checkingPredecessorRetirementReportCache) AttestedRetirementReport(ocrtypes.ConfigDigest) ([]byte, error) {
	return []byte("attested"), nil
}

func (c *checkingPredecessorRetirementReportCache) CheckAttestedRetirementReport(ocrtypes.ConfigDigest, []byte) (protocol.RetirementReport, error) {
	return c.report, c.err
}

// The v3.0 plugin verifies against its local cache, so these two are only here
// to satisfy the interface.
func (c *checkingPredecessorRetirementReportCache) PredecessorConfig(ocrtypes.ConfigDigest) ([][]byte, uint8, bool) {
	panic("not implemented")
}

func (c *checkingPredecessorRetirementReportCache) VerifyAttestedRetirementReport(ocrtypes.ConfigDigest, [][]byte, uint8, []byte) (protocol.RetirementReport, error) {
	panic("not implemented")
}

// stagingPlugin builds a v3.0 plugin in the staging stage behind the given
// predecessor retirement report cache.
func stagingPlugin(t *testing.T, prrc protocol.PredecessorRetirementReportCache) *Plugin {
	t.Helper()
	obsCodec, err := NewProtoObservationCodec(logger.Test(t), false)
	require.NoError(t, err)
	predecessor := ocrtypes.ConfigDigest{0xAB}
	return &Plugin{
		Config:                           Config{VerboseLogging: true},
		ConfigDigest:                     ocrtypes.ConfigDigest{0x30},
		PredecessorConfigDigest:          &predecessor,
		PredecessorRetirementReportCache: prrc,
		ShouldRetireCache:                &mockShouldRetireCache{},
		Logger:                           logger.Test(t),
		N:                                4,
		F:                                1,
		ObservationCodec:                 obsCodec,
		OutcomeCodec:                     protoOutcomeCodecV1{},
		RetirementReportCodec:            protocol.StandardRetirementReportCodec{},
		OptsCache:                        protocol.NewOptsCache(),
		ProtocolVersion:                  1,
	}
}

// handoverRound runs one Outcome cycle with all four oracles sending obs.
func handoverRound(t *testing.T, p *Plugin, seqNr uint64, prev ocr3types.Outcome, obs Observation) Outcome {
	t.Helper()
	encoded, err := p.ObservationCodec.Encode(obs)
	require.NoError(t, err)
	aos := make([]ocrtypes.AttributedObservation, 0, 4)
	for i := range 4 {
		aos = append(aos, ocrtypes.AttributedObservation{Observer: commontypes.OracleID(i), Observation: encoded}) //nolint:gosec // small loop index
	}
	raw, err := p.Outcome(tests.Context(t), ocr3types.OutcomeContext{SeqNr: seqNr, PreviousOutcome: prev}, nil, aos)
	require.NoError(t, err)
	out, err := p.OutcomeCodec.Decode(raw)
	require.NoError(t, err)
	return out
}

// Test_Outcome_BadPredecessorRetirementDoesNotStallRound covers the case where
// EVERY oracle attaches an attested retirement report that cannot be verified
// locally — the normal case, since honest oracles all attach the same bytes,
// e.g. while the predecessor's ConfigSet row is still missing from the local
// RetirementReportCache. Only the retirement field is dropped: the round still
// completes on the remaining observation contents (timestamp, channel votes) and
// the instance stays in staging until the report becomes verifiable.
func Test_Outcome_BadPredecessorRetirementDoesNotStallRound(t *testing.T) {
	prrc := &checkingPredecessorRetirementReportCache{err: errUnverifiable}
	p := stagingPlugin(t, prrc)

	cid := llotypes.ChannelID(1)
	cd := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatJSON,
		Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
	}
	obs := Observation{
		UnixTimestampNanoseconds:      1_000_000_000,
		AttestedPredecessorRetirement: []byte("attested"),
		UpdateChannelDefinitions:      llotypes.ChannelDefinitions{cid: cd},
	}
	// seqNr 1 is the bootstrap round and ignores observations.
	boot := handoverRound(t, p, 1, nil, Observation{})
	require.Equal(t, protocol.LifeCycleStageStaging, boot.LifeCycleStage)
	bootRaw, err := p.OutcomeCodec.Encode(boot)
	require.NoError(t, err)

	out := handoverRound(t, p, 2, bootRaw, obs)

	require.Equal(t, protocol.LifeCycleStageStaging, out.LifeCycleStage)
	require.Equal(t, uint64(1_000_000_000), out.ObservationTimestampNanoseconds,
		"the observation timestamp must still be counted")
	require.Contains(t, out.ChannelDefinitions, cid,
		"channel vote carried alongside the unverifiable retirement report must still be counted")

	// Once the report verifies, the next round promotes.
	prrc.err = nil
	prrc.report = protocol.RetirementReport{ProtocolVersion: 1, ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{cid: 500}}
	raw, err := p.OutcomeCodec.Encode(out)
	require.NoError(t, err)
	out = handoverRound(t, p, 3, raw, Observation{
		UnixTimestampNanoseconds:      2_000_000_000,
		AttestedPredecessorRetirement: []byte("attested"),
	})
	require.Equal(t, protocol.LifeCycleStageProduction, out.LifeCycleStage)
	require.Equal(t, uint64(500), out.ValidAfterNanoseconds[cid])
}

// Test_Outcome_RejectsIncompatibleProtocolVersion covers the LLO protocol
// version guard: a valid, correctly signed retirement report from a predecessor
// running a protocol version this build does not understand must not promote the
// successor, since its ValidAfterNanoseconds may not mean what the successor
// assumes. The round itself must still complete.
func Test_Outcome_RejectsIncompatibleProtocolVersion(t *testing.T) {
	cid := llotypes.ChannelID(1)
	p := stagingPlugin(t, &checkingPredecessorRetirementReportCache{
		report: protocol.RetirementReport{
			ProtocolVersion:       protocol.MaxSupportedProtocolVersion + 1,
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{cid: 500},
		},
	})

	boot := handoverRound(t, p, 1, nil, Observation{})
	bootRaw, err := p.OutcomeCodec.Encode(boot)
	require.NoError(t, err)
	out := handoverRound(t, p, 2, bootRaw, Observation{
		UnixTimestampNanoseconds:      1_000_000_000,
		AttestedPredecessorRetirement: []byte("attested"),
	})

	require.Equal(t, protocol.LifeCycleStageStaging, out.LifeCycleStage,
		"instance must stay in staging on a retirement report from an unsupported protocol version")
	require.NotContains(t, out.ValidAfterNanoseconds, cid)
}

// backfillDefs builds a target channel plus a history-backfill channel holding
// three observations, at 100s, 150s and 200s.
func backfillDefs(targetID, backfillID llotypes.ChannelID) llotypes.ChannelDefinitions {
	streams := []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}
	return llotypes.ChannelDefinitions{
		targetID: {ReportFormat: llotypes.ReportFormatJSON, Streams: streams},
		backfillID: {
			ReportFormat: llotypes.ReportFormatHistoryBackfill,
			Streams:      streams,
			Opts:         []byte(`{"targetChannelId":2,"observations":{"100":{"1":"1"},"150":{"1":"3"},"200":{"1":"2"}}}`),
		},
	}
}

// Test_IsReportable_StagingSkipsBackfill covers the backfill guard. Both
// protocol instances of a blue/green job share one ChannelDefinitionCache, so a
// backfill channel is present on the staging instance too, where it is a new
// channel with a watermark of 0. Without the guard the staging instance replays
// the whole backfill from the beginning for the length of the overlap window.
func Test_IsReportable_StagingSkipsBackfill(t *testing.T) {
	targetID, backfillID := llotypes.ChannelID(2), llotypes.ChannelID(10)
	out := Outcome{
		ObservationTimestampNanoseconds: uint64(300 * time.Second),
		ChannelDefinitions:              backfillDefs(targetID, backfillID),
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{backfillID: 0},
	}

	t.Run("staging is not reportable", func(t *testing.T) {
		out.LifeCycleStage = protocol.LifeCycleStageStaging
		uerr := out.IsReportable(backfillID, 1, 0, nil)
		require.NotNil(t, uerr)
		require.Contains(t, uerr.Error(), "backfill is not performed by a staging instance")
	})

	t.Run("production is reportable", func(t *testing.T) {
		out.LifeCycleStage = protocol.LifeCycleStageProduction
		require.Nil(t, out.IsReportable(backfillID, 1, 0, nil))
	})

	t.Run("retired is not reportable", func(t *testing.T) {
		out.LifeCycleStage = protocol.LifeCycleStageRetired
		uerr := out.IsReportable(backfillID, 1, 0, nil)
		require.NotNil(t, uerr)
		require.Contains(t, uerr.Error(), "retired channel")
	})
}

// Test_Outcome_StagingDoesNotAdvanceBackfillWatermark is the other half of the
// guard: skipping the report while still advancing the watermark would silently
// consume backfill observations that were never emitted, so that by the time the
// instance is promoted the backfill would look further along than it is.
func Test_Outcome_StagingDoesNotAdvanceBackfillWatermark(t *testing.T) {
	targetID, backfillID := llotypes.ChannelID(2), llotypes.ChannelID(10)
	defs := backfillDefs(targetID, backfillID)

	// A staging instance, several rounds in, with the backfill channel already
	// established and its watermark still at the start.
	p := stagingPlugin(t, &checkingPredecessorRetirementReportCache{err: errUnverifiable})
	prev := Outcome{
		LifeCycleStage:                  protocol.LifeCycleStageStaging,
		ObservationTimestampNanoseconds: uint64(300 * time.Second),
		ChannelDefinitions:              defs,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{backfillID: 0},
	}
	prevRaw, err := p.OutcomeCodec.Encode(prev)
	require.NoError(t, err)

	out := handoverRound(t, p, 5, prevRaw, Observation{
		UnixTimestampNanoseconds: uint64(400 * time.Second),
	})

	require.Equal(t, protocol.LifeCycleStageStaging, out.LifeCycleStage)
	require.Equal(t, uint64(0), out.ValidAfterNanoseconds[backfillID],
		"a staging instance emits no backfill report, so its watermark must not move")

	// The same round on a production instance does advance it, to the first
	// eligible observation.
	prev.LifeCycleStage = protocol.LifeCycleStageProduction
	prevRaw, err = p.OutcomeCodec.Encode(prev)
	require.NoError(t, err)

	out = handoverRound(t, p, 5, prevRaw, Observation{
		UnixTimestampNanoseconds: uint64(400 * time.Second),
	})
	require.Equal(t, uint64(100*time.Second), out.ValidAfterNanoseconds[backfillID])
}

// Test_Outcome_PromotionResumesBackfillAtPredecessorWatermark is why skipping is
// correct rather than merely safe: promotion replaces ValidAfterNanoseconds
// wholesale with the predecessor's map, so the staging instance's backfill
// watermark is discarded unread and the backfill resumes exactly where the
// predecessor left it. Nothing was lost by not replaying it.
func Test_Outcome_PromotionResumesBackfillAtPredecessorWatermark(t *testing.T) {
	targetID, backfillID := llotypes.ChannelID(2), llotypes.ChannelID(10)
	defs := backfillDefs(targetID, backfillID)

	// The predecessor retired having already backfilled up to the 150s
	// observation.
	predecessorWatermark := uint64(150 * time.Second)
	p := stagingPlugin(t, &checkingPredecessorRetirementReportCache{
		report: protocol.RetirementReport{
			ProtocolVersion:       1,
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{backfillID: predecessorWatermark},
		},
	})

	prev := Outcome{
		LifeCycleStage:                  protocol.LifeCycleStageStaging,
		ObservationTimestampNanoseconds: uint64(300 * time.Second),
		ChannelDefinitions:              defs,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{backfillID: 0},
	}
	prevRaw, err := p.OutcomeCodec.Encode(prev)
	require.NoError(t, err)

	out := handoverRound(t, p, 5, prevRaw, Observation{
		UnixTimestampNanoseconds:      uint64(400 * time.Second),
		AttestedPredecessorRetirement: []byte("attested"),
	})

	require.Equal(t, protocol.LifeCycleStageProduction, out.LifeCycleStage)
	require.Equal(t, predecessorWatermark, out.ValidAfterNanoseconds[backfillID],
		"promotion must resume the backfill at the predecessor's watermark, not at the staging instance's")
}

// Test_Outcome_BackfillAbsentFromRetirementReportStartsAtZero pins the existing
// new-channel semantics, which the guard does not change: a backfill channel the
// predecessor never reported is absent from its retirement report and falls
// through to the new-channel path, starting from the beginning. That is the
// correct reading — the predecessor never backfilled it.
func Test_Outcome_BackfillAbsentFromRetirementReportStartsAtZero(t *testing.T) {
	targetID, backfillID := llotypes.ChannelID(2), llotypes.ChannelID(10)
	otherID := llotypes.ChannelID(3)
	defs := backfillDefs(targetID, backfillID)

	p := stagingPlugin(t, &checkingPredecessorRetirementReportCache{
		report: protocol.RetirementReport{
			ProtocolVersion: 1,
			// Mentions some other channel, but not the backfill one.
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{otherID: 500},
		},
	})

	prev := Outcome{
		LifeCycleStage:                  protocol.LifeCycleStageStaging,
		ObservationTimestampNanoseconds: uint64(300 * time.Second),
		ChannelDefinitions:              defs,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{backfillID: 0},
	}
	prevRaw, err := p.OutcomeCodec.Encode(prev)
	require.NoError(t, err)

	out := handoverRound(t, p, 5, prevRaw, Observation{
		UnixTimestampNanoseconds:      uint64(400 * time.Second),
		AttestedPredecessorRetirement: []byte("attested"),
	})

	require.Equal(t, protocol.LifeCycleStageProduction, out.LifeCycleStage)
	require.Equal(t, uint64(0), out.ValidAfterNanoseconds[backfillID])
}
