package llo

import (
	"errors"
	"testing"

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
