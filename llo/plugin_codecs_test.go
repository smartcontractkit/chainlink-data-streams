package llo

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// TODO: probably ought to have fuzz testing to detect crashes
// TODO: what about resource starvation attacks? maximum length? Does OCR
// protect us from this?

func Test_protoObservationCodec(t *testing.T) {
	t.Run("encode and decode empty struct", func(t *testing.T) {
		obs := Observation{}
		obsBytes, err := (protoObservationCodec{}).Encode(obs)
		require.NoError(t, err)

		obs2, err := (protoObservationCodec{}).Decode(obsBytes)
		require.NoError(t, err)

		assert.Equal(t, obs, obs2)
	})
	t.Run("encode and decode with values", func(t *testing.T) {
		obs := Observation{
			AttestedPredecessorRetirement: []byte{1, 2, 3},
			ShouldRetire:                  true,
			UnixTimestampNanoseconds:      1234567890,
			RemoveChannelIDs: map[llotypes.ChannelID]struct{}{
				1: {},
				2: {},
			},
			UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				3: {
					ReportFormat:  llotypes.ReportFormatJSON,
					ChainSelector: 12345,
					StreamIDs:     []llotypes.StreamID{3, 4},
				},
			},
			StreamValues: map[llotypes.StreamID]*big.Int{
				4: big.NewInt(123),
				5: big.NewInt(456),
				6: (*big.Int)(nil),
			},
		}

		obsBytes, err := (protoObservationCodec{}).Encode(obs)
		require.NoError(t, err)

		obs2, err := (protoObservationCodec{}).Decode(obsBytes)
		require.NoError(t, err)

		expectedObs := obs
		delete(expectedObs.StreamValues, 6) // nils will be dropped

		assert.Equal(t, expectedObs, obs2)
	})
}

func Test_protoOutcomeCodec(t *testing.T) {
	t.Run("encode and decode empty struct", func(t *testing.T) {
		outcome := Outcome{}
		outcomeBytes, err := (protoOutcomeCodec{}).Encode(outcome)
		require.NoError(t, err)

		outcome2, err := (protoOutcomeCodec{}).Decode(outcomeBytes)
		require.NoError(t, err)

		assert.Equal(t, outcome, outcome2)
	})
	t.Run("encode and decode with values", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                   llotypes.LifeCycleStage("staging"),
			ObservationsTimestampNanoseconds: 1234567890,
			ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				3: {
					ReportFormat:  llotypes.ReportFormatJSON,
					ChainSelector: 12345,
					StreamIDs:     []llotypes.StreamID{1, 2},
				},
			},

			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				3: 123,
			},
			StreamMedians: map[llotypes.StreamID]*big.Int{
				1: big.NewInt(123),
				2: big.NewInt(456),
				3: nil,
			},
		}

		outcomeBytes, err := (protoOutcomeCodec{}).Encode(outcome)
		require.NoError(t, err)

		outcome2, err := (protoOutcomeCodec{}).Decode(outcomeBytes)
		require.NoError(t, err)

		expectedOutcome := outcome
		delete(expectedOutcome.StreamMedians, 3) // nils will be dropped

		assert.Equal(t, outcome, outcome2)
	})
}
