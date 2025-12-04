package llo

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type mockReportCodec struct {
	err error
}

func (m mockReportCodec) Encode(Report, llotypes.ChannelDefinition, interface{}) ([]byte, error) {
	return nil, nil
}

func (m mockReportCodec) Verify(llotypes.ChannelDefinition) error {
	return m.err
}

func Test_VerifyChannelDefinitions(t *testing.T) {
	mockReportFormat := llotypes.ReportFormat(0)
	codecs := make(map[llotypes.ReportFormat]ReportCodec)
	codecs[mockReportFormat] = mockReportCodec{}

	t.Run("fails with too many channels", func(t *testing.T) {
		channelDefs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength+1)
		for i := uint32(0); i < MaxOutcomeChannelDefinitionsLength+1; i++ {
			channelDefs[i] = llotypes.ChannelDefinition{}
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "too many channels, got: 2001/2000")
	})
	t.Run("fails if channel has too many streams", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: make([]llotypes.Stream, MaxStreamsPerChannel+1),
			},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 has too many streams, got: 10001/10000")
	})
	t.Run("fails for channel with no streams", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 has no streams")
	})

	t.Run("fails for channel with zero aggregator", func(t *testing.T) {
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{llotypes.Stream{}},
			},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "ChannelDefinition with ID 1 has stream 0 with zero aggregator (this may indicate an uninitialized struct)")
	})

	t.Run("fails if too many total unique stream IDs", func(t *testing.T) {
		streams := make([]llotypes.Stream, MaxObservationStreamValuesLength)
		for i := uint32(0); i < MaxObservationStreamValuesLength; i++ {
			streams[i] = llotypes.Stream{StreamID: i, Aggregator: llotypes.AggregatorMedian}
		}
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				Streams: streams,
			},
			2: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{llotypes.Stream{StreamID: MaxObservationStreamValuesLength + 1, Aggregator: llotypes.AggregatorMedian}},
			},
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.EqualError(t, err, "too many unique stream IDs, got: 10001/10000")
	})
	t.Run("fails if codec.Verify fails", func(t *testing.T) {
		failingCodecs := make(map[llotypes.ReportFormat]ReportCodec)
		failingCodecs[mockReportFormat] = mockReportCodec{err: errors.New("codec error")}
		channelDefs := llotypes.ChannelDefinitions{
			1: llotypes.ChannelDefinition{
				ReportFormat: mockReportFormat,
				Streams: []llotypes.Stream{
					llotypes.Stream{
						StreamID:   1,
						Aggregator: llotypes.AggregatorMedian,
					},
				},
			},
		}
		err := VerifyChannelDefinitions(failingCodecs, channelDefs)
		require.EqualError(t, err, "invalid ChannelDefinition with ID 1: codec error")
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
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.NoError(t, err)
	})

	t.Run("succeeds with exact maxes", func(t *testing.T) {
		streams := make([]llotypes.Stream, MaxObservationStreamValuesLength)
		for i := uint32(0); i < MaxObservationStreamValuesLength; i++ {
			streams[i] = llotypes.Stream{StreamID: i, Aggregator: llotypes.AggregatorMedian}
		}
		channelDefs := make(llotypes.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength)
		for i := uint32(0); i < MaxOutcomeChannelDefinitionsLength; i++ {
			channelDefs[i] = llotypes.ChannelDefinition{Streams: streams}
		}
		err := VerifyChannelDefinitions(codecs, channelDefs)
		require.NoError(t, err)
	})
}

func Test_ChannelDefinitionsOptsCache(t *testing.T) {
	t.Run("Set and Get with OptsParser codec", func(t *testing.T) {
		cache := NewChannelDefinitionOptsCache()
		codec := mockCodec{timeResolution: 4}
		channelID := llotypes.ChannelID(1)

		setErr := cache.Set(channelID, llotypes.ChannelOpts{}, codec)
		require.NoError(t, setErr)

		val, exists := cache.Get(channelID)
		require.True(t, exists)
		require.NotNil(t, val)
	})

	t.Run("Set returns error when codec does not implement OptsParser", func(t *testing.T) {
		cache := NewChannelDefinitionOptsCache()
		codec := mockReportCodec{}

		err := cache.Set(llotypes.ChannelID(1), llotypes.ChannelOpts{}, codec)
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not implement OptsParser")

		val, exists := cache.Get(llotypes.ChannelID(1))
		require.False(t, exists)
		require.Nil(t, val)
	})

	t.Run("Set replaces existing value", func(t *testing.T) {
		cache := NewChannelDefinitionOptsCache()
		codec1 := mockCodec{timeResolution: 4}
		codec2 := mockCodec{timeResolution: 3}
		channelID := llotypes.ChannelID(1)

		cache.Set(channelID, llotypes.ChannelOpts{}, codec1)
		cache.Set(channelID, llotypes.ChannelOpts{}, codec2)

		val, exists := cache.Get(channelID)
		require.True(t, exists)
		mc := val.(mockCodec)
		require.Equal(t, codec2.timeResolution, mc.timeResolution)
	})

	t.Run("Multiple items in cache", func(t *testing.T) {
		cache := NewChannelDefinitionOptsCache()
		codec := mockCodec{timeResolution: 4}

		cache.Set(llotypes.ChannelID(1), llotypes.ChannelOpts{}, codec)
		cache.Set(llotypes.ChannelID(2), llotypes.ChannelOpts{}, codec)
		cache.Set(llotypes.ChannelID(3), llotypes.ChannelOpts{}, codec)

		val1, exists1 := cache.Get(llotypes.ChannelID(1))
		require.True(t, exists1)
		require.Equal(t, codec.timeResolution, val1.(mockCodec).timeResolution)

		val2, exists2 := cache.Get(llotypes.ChannelID(2))
		require.True(t, exists2)
		require.Equal(t, codec.timeResolution, val2.(mockCodec).timeResolution)

		val3, exists3 := cache.Get(llotypes.ChannelID(3))
		require.True(t, exists3)
		require.Equal(t, codec.timeResolution, val3.(mockCodec).timeResolution)
	})

	t.Run("Delete removes item", func(t *testing.T) {
		cache := NewChannelDefinitionOptsCache()
		codec := mockCodec{}
		channelID := llotypes.ChannelID(1)

		cache.Set(channelID, llotypes.ChannelOpts{}, codec)
		cache.Delete(channelID)

		val, exists := cache.Get(channelID)
		require.False(t, exists)
		require.Nil(t, val)
	})

	t.Run("Delete does not affect other items", func(t *testing.T) {
		cache := NewChannelDefinitionOptsCache()
		codec := mockCodec{timeResolution: 4}

		cache.Set(llotypes.ChannelID(1), llotypes.ChannelOpts{}, codec)
		cache.Set(llotypes.ChannelID(2), llotypes.ChannelOpts{}, codec)

		val1, exists1 := cache.Get(llotypes.ChannelID(1))
		require.True(t, exists1)
		require.NotNil(t, val1)
		
		val2, exists2 := cache.Get(llotypes.ChannelID(2))
		require.True(t, exists2)
		require.NotNil(t, val2)

		cache.Delete(llotypes.ChannelID(1))

		// ChannelID 1 should be deleted
		val, exists := cache.Get(llotypes.ChannelID(1))
		require.False(t, exists)
		require.Nil(t, val)

		// ChannelID 2 should still exist
		val, exists = cache.Get(llotypes.ChannelID(2))
		require.True(t, exists)
		require.NotNil(t, val)
	})
}
