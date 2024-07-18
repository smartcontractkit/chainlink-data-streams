package llo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_VerifyChannelDefinitions(t *testing.T) {
	t.Run("fails with too many channels", func(t *testing.T) {
		channelDefs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength+1)
		for i := uint32(0); i < MaxOutcomeChannelDefinitionsLength+1; i++ {
			channelDefs[i] = llotypes.ChannelDefinition{}
		}
		err := VerifyChannelDefinitions(channelDefs)
		assert.EqualError(t, err, "too many channels, got: 10001/10000")
	})

	t.Run("fails for channel with no streams", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{},
		}
		err := VerifyChannelDefinitions(channelDefs)
		assert.EqualError(t, err, "ChannelDefinition with ID 1 has no streams")
	})

	t.Run("fails for channel with zero aggregator", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{llotypes.Stream{}},
			},
		}
		err := VerifyChannelDefinitions(channelDefs)
		assert.EqualError(t, err, "ChannelDefinition with ID 1 has stream 0 with zero aggregator (this may indicate an uninitialized struct)")
	})

	t.Run("fails if too many total unique stream IDs", func(t *testing.T) {
		streams := make([]llotypes.Stream, MaxObservationStreamValuesLength)
		for i := 0; i < MaxObservationStreamValuesLength; i++ {
			streams[i] = llotypes.Stream{StreamID: uint32(i), Aggregator: llotypes.AggregatorMedian}
		}
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: streams,
			},
			2: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{llotypes.Stream{StreamID: MaxObservationStreamValuesLength + 1, Aggregator: llotypes.AggregatorMedian}},
			},
		}
		err := VerifyChannelDefinitions(channelDefs)
		assert.EqualError(t, err, "too many unique stream IDs, got: 10001/10000")
	})

	t.Run("succeeds with valid channel definitions", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{
					llotypes.Stream{
						StreamID:   1,
						Aggregator: llotypes.AggregatorMedian,
					},
				},
			},
		}
		err := VerifyChannelDefinitions(channelDefs)
		assert.NoError(t, err)
	})

	t.Run("succeeds with exact maxes", func(t *testing.T) {
		streams := make([]llotypes.Stream, MaxObservationStreamValuesLength)
		for i := 0; i < MaxObservationStreamValuesLength; i++ {
			streams[i] = llotypes.Stream{StreamID: uint32(i), Aggregator: llotypes.AggregatorMedian}
		}
		channelDefs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength)
		for i := uint32(0); i < MaxOutcomeChannelDefinitionsLength; i++ {
			channelDefs[i] = llotypes.ChannelDefinition{Streams: streams}
		}
		err := VerifyChannelDefinitions(channelDefs)
		assert.NoError(t, err)
	})
}
