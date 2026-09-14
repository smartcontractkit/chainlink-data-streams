package llo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Test_channelHashVersionGating pins the two halves of the fix for the channel
// vote hash: the old hash must be preserved byte for byte for protocol versions
// 0 and 1, because changing the hash changes the state transition and would
// split a running DON, and version 2 must use the hash that commits to the
// whole definition.
func Test_channelHashVersionGating(t *testing.T) {
	cd := protocol.ChannelDefinitionWithID{
		ChannelID: 1,
		ChannelDefinition: llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormat(1),
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			Opts:         []byte(`{}`),
		},
	}

	for _, version := range []uint32{0, 1} {
		p := &Plugin{ProtocolVersion: version}
		assert.Equal(t, MakeChannelHash(cd), p.makeChannelHash(cd), "protocol version %d must keep the legacy hash", version)
	}

	p := &Plugin{ProtocolVersion: 2}
	assert.Equal(t, protocol.ChannelHashV2(cd), p.makeChannelHash(cd))
	assert.NotEqual(t, MakeChannelHash(cd), p.makeChannelHash(cd))
}

// Test_channelVoteSubstitution covers the substitution attack the version 2
// hash exists to stop. Three honest oracles vote a live channel; one Byzantine
// oracle votes a definition that differs only in Tombstone, which the legacy
// hash does not cover, and is processed last so that it wins the map
// assignment in decodeObservations.
//
// Under protocol version 1 the honest votes are counted against the attacker's
// definition and the tombstoned variant is installed. Under version 2 the two
// definitions land in different vote buckets, the attacker holds one vote
// which cannot exceed F, and the honest definition is installed.
func Test_channelVoteSubstitution(t *testing.T) {
	const chID = llotypes.ChannelID(42)

	honest := llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
		Streams: []llotypes.Stream{
			{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 3, Aggregator: llotypes.AggregatorMedian},
		},
		Opts: []byte(`{"baseUSDFee":"0.1","expirationWindow":86400,"feedID":"0x0003c317fec7fad514c67aacc6b7e1e9b7e0a1b1b0e5a1a0b7e8c9d0e1f2a3b4","multiplier":"1000000000000000000"}`),
	}
	forged := honest
	forged.Tombstone = true

	// Equals has always considered these different. Only the legacy hash did not.
	require.False(t, honest.Equals(forged))

	outcomeFor := func(t *testing.T, protocolVersion uint32) Outcome {
		t.Helper()
		ctx := t.Context()
		obsCodec, err := NewProtoObservationCodec(logger.Nop(), true)
		require.NoError(t, err)
		p := &Plugin{
			Config:           Config{true},
			OutcomeCodec:     GetOutcomeCodec(protocol.OffchainConfig{ProtocolVersion: protocolVersion, DefaultMinReportIntervalNanoseconds: 1}),
			Logger:           logger.Test(t),
			ObservationCodec: obsCodec,
			DonID:            10000043,
			ConfigDigest:     types.ConfigDigest{1, 2, 3, 4},
			F:                1,
			OptsCache:        protocol.NewOptsCache(),
			ProtocolVersion:  protocolVersion,
		}

		encode := func(def llotypes.ChannelDefinition) types.Observation {
			b, encErr := p.ObservationCodec.Encode(Observation{
				UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{chID: def},
			})
			require.NoError(t, encErr)
			return b
		}
		honestObs, forgedObs := encode(honest), encode(forged)

		aos := []types.AttributedObservation{
			{Observation: honestObs, Observer: commontypes.OracleID(0)},
			{Observation: honestObs, Observer: commontypes.OracleID(1)},
			{Observation: honestObs, Observer: commontypes.OracleID(2)},
			// The attacker orders its own observation last, which it can do
			// deterministically while it leads the round.
			{Observation: forgedObs, Observer: commontypes.OracleID(3)},
		}
		// Every observation here, forged included, passes the plugin's own
		// validation. Validation cannot see the vote hash, so it is not and
		// cannot be the place this is caught.
		for _, ao := range aos {
			require.NoError(t, p.ValidateObservation(ctx, ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, ao))
		}

		encoded, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, aos)
		require.NoError(t, err)
		decoded, err := p.OutcomeCodec.Decode(encoded)
		require.NoError(t, err)
		return decoded
	}

	t.Run("protocol version 1 installs the forged definition", func(t *testing.T) {
		stored, ok := outcomeFor(t, 1).ChannelDefinitions[chID]
		require.True(t, ok)
		assert.True(t, stored.Tombstone, "documents the vulnerable legacy behaviour that version 2 fixes")
	})

	t.Run("protocol version 2 installs the definition the quorum voted for", func(t *testing.T) {
		stored, ok := outcomeFor(t, 2).ChannelDefinitions[chID]
		require.True(t, ok, "the three honest votes must still add the channel")
		assert.False(t, stored.Tombstone, "the lone forged vote must not reach F+1")
		assert.True(t, honest.Equals(stored))
	})
}
