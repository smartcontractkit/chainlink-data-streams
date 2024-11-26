package llo

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

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
					ReportFormat: llotypes.ReportFormatJSON,
					Streams:      []llotypes.Stream{{StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorQuote}},
					Opts:         []byte(`{"foo":"bar"}`),
				},
			},
			StreamValues: map[llotypes.StreamID]StreamValue{
				4: ToDecimal(decimal.NewFromInt(123)),
				5: ToDecimal(decimal.NewFromInt(456)),
				6: (*Decimal)(nil),
				7: nil,
			},
		}

		obsBytes, err := (protoObservationCodec{}).Encode(obs)
		require.NoError(t, err)

		obs2, err := (protoObservationCodec{}).Decode(obsBytes)
		require.NoError(t, err)

		expectedObs := obs
		delete(expectedObs.StreamValues, 6) // nils will be dropped
		delete(expectedObs.StreamValues, 7) // nils will be dropped

		assert.Equal(t, expectedObs, obs2)
	})
	t.Run("decoding with invalid data", func(t *testing.T) {
		t.Run("not a protobuf", func(t *testing.T) {
			_, err := (protoObservationCodec{}).Decode([]byte("not a protobuf"))
			require.Error(t, err)

			assert.Contains(t, err.Error(), "cannot parse invalid wire-format data")
		})
		t.Run("duplicate RemoveChannelIDs", func(t *testing.T) {
			pbuf := &LLOObservationProto{
				RemoveChannelIDs: []uint32{1, 1},
			}

			obsBytes, err := proto.Marshal(pbuf)
			require.NoError(t, err)

			_, err = (protoObservationCodec{}).Decode(obsBytes)
			require.EqualError(t, err, "failed to decode observation; duplicate channel ID in RemoveChannelIDs: 1")
		})
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
					ReportFormat: llotypes.ReportFormatJSON,
					Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorQuote}},
					Opts:         []byte(`{"foo":"bar"}`),
				},
			},

			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				3: 123,
			},
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(123)),
				},
				2: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(456)),
				},
				3: map[llotypes.Aggregator]StreamValue{},
				4: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorQuote: &Quote{
						Bid:       decimal.NewFromInt(1010),
						Benchmark: decimal.NewFromInt(1011),
						Ask:       decimal.NewFromInt(1012),
					},
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(457)),
				},
				5: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorQuote: &Quote{
						Bid:       decimal.NewFromInt(1013),
						Benchmark: decimal.NewFromInt(1014),
						Ask:       decimal.NewFromInt(1015),
					},
				},
			},
		}

		outcomeBytes, err := (protoOutcomeCodec{}).Encode(outcome)
		require.NoError(t, err)

		outcome2, err := (protoOutcomeCodec{}).Decode(outcomeBytes)
		require.NoError(t, err)

		expectedOutcome := outcome
		delete(expectedOutcome.StreamAggregates, 3) // nils will be dropped

		assert.Equal(t, outcome, outcome2)
	})
}
