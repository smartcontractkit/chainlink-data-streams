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

func (m mockReportCodec) Encode(Report, llotypes.ChannelDefinition) ([]byte, error) {
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
