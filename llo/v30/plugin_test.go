package llo

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockShouldRetireCache struct {
	shouldRetire bool
	err          error
}

func (m *mockShouldRetireCache) ShouldRetire(types.ConfigDigest) (bool, error) {
	return m.shouldRetire, m.err
}

type mockChannelDefinitionCache struct {
	definitions llotypes.ChannelDefinitions
}

func (m *mockChannelDefinitionCache) Definitions(previous llotypes.ChannelDefinitions) llotypes.ChannelDefinitions {
	return m.definitions
}

func (m *mockChannelDefinitionCache) Close() error {
	return nil
}

func (m *mockChannelDefinitionCache) HealthReport() map[string]error {
	return nil
}

func (m *mockChannelDefinitionCache) Name() string {
	return "mockChannelDefinitionCache"
}

func (m *mockChannelDefinitionCache) Ready() error {
	return nil
}

func (m *mockChannelDefinitionCache) Start(ctx context.Context) error {
	return nil
}

var _ llotypes.ChannelDefinitionCache = &mockChannelDefinitionCache{}

type mockDataSource struct {
	s   protocol.StreamValues
	err error
}

func (m *mockDataSource) Observe(ctx context.Context, streamValues protocol.StreamValues, opts DSOpts) error {
	for k, v := range m.s {
		streamValues[k] = v
	}
	return m.err
}

func Test_ValidateObservation(t *testing.T) {
	obsCodec, err := NewProtoObservationCodec(logger.Nop(), true)
	require.NoError(t, err)

	p := &Plugin{
		ObservationCodec: obsCodec,
		Config:           Config{true},
	}

	t.Run("SeqNr < 1 is not valid", func(t *testing.T) {
		ctx := tests.Context(t)
		err := p.ValidateObservation(ctx, ocr3types.OutcomeContext{}, types.Query{}, types.AttributedObservation{})
		require.EqualError(t, err, "Invalid SeqNr: 0")
	})
	t.Run("SeqNr == 1 enforces empty observation", func(t *testing.T) {
		ctx := tests.Context(t)
		err := p.ValidateObservation(ctx, ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, types.AttributedObservation{Observation: []byte{1}})
		require.EqualError(t, err, "Expected empty observation for first round, got: 0x01")
	})
	t.Run("nested stream value on TimestampedStreamValue must be a Decimal", func(t *testing.T) {
		ctx := tests.Context(t)
		obs := Observation{
			StreamValues: map[uint32]protocol.StreamValue{
				1: &protocol.TimestampedStreamValue{
					StreamValue: &protocol.TimestampedStreamValue{
						StreamValue: protocol.ToDecimal(decimal.NewFromInt(1)),
					},
				},
			},
		}

		serializedObs, err := obsCodec.Encode(obs)
		require.NoError(t, err)

		err = p.ValidateObservation(ctx, ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, types.AttributedObservation{Observation: serializedObs})
		require.EqualError(t, err, "nested stream value on TimestampedStreamValue must be a Decimal, got: TimestampedStreamValue")
	})
}

type mockOnchainConfigCodec struct{}

func (m *mockOnchainConfigCodec) Decode(b []byte) (protocol.OnchainConfig, error) {
	return protocol.OnchainConfig{}, nil
}

func (m *mockOnchainConfigCodec) Encode(protocol.OnchainConfig) ([]byte, error) {
	return nil, nil
}

func Test_NewReportingPlugin_setsValues(t *testing.T) {
	f := &PluginFactory{
		PluginFactoryParams{
			OnchainConfigCodec: &mockOnchainConfigCodec{},
			Logger:             logger.Test(t),
		},
	}
	t.Run("outputs correct plugin info", func(t *testing.T) {
		_, pInfo, err := f.NewReportingPlugin(context.Background(), ocr3types.ReportingPluginConfig{})
		require.NoError(t, err)

		assert.Equal(t, ocr3types.ReportingPluginInfo{Name: "LLO", Limits: ocr3types.ReportingPluginLimits{MaxQueryLength: 0, MaxObservationLength: 1048576, MaxOutcomeLength: 5242880, MaxReportLength: 5242880, MaxReportCount: 2000}}, pInfo)
	})

	t.Run("with empty offchain config", func(t *testing.T) {
		p, _, err := f.NewReportingPlugin(context.Background(), ocr3types.ReportingPluginConfig{})
		require.NoError(t, err)

		assert.Equal(t, uint32(0), p.(*Plugin).ProtocolVersion)
		assert.Equal(t, uint64(0), p.(*Plugin).DefaultMinReportIntervalNanoseconds)
	})

	t.Run("with version 1 offchain config", func(t *testing.T) {
		encodedOffchainConfig, err := protocol.OffchainConfig{
			ProtocolVersion:                     1,
			DefaultMinReportIntervalNanoseconds: 12345,
		}.Encode()
		require.NoError(t, err)
		p, _, err := f.NewReportingPlugin(context.Background(), ocr3types.ReportingPluginConfig{
			OffchainConfig: encodedOffchainConfig,
		})
		require.NoError(t, err)

		assert.Equal(t, uint32(1), p.(*Plugin).ProtocolVersion)
		assert.Equal(t, uint64(12345), p.(*Plugin).DefaultMinReportIntervalNanoseconds)
	})
}

func Test_withoutSigningSchemes(t *testing.T) {
	plain := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked}
	merkle := plain
	merkle.SigningScheme = llotypes.SigningSchemeMerkle

	committed := llotypes.ChannelDefinitions{1: plain, 2: plain}

	got, unsupported := withoutSigningSchemes(committed, committed)
	assert.Equal(t, committed, got)
	assert.Empty(t, unsupported)

	// A committed channel switched to a scheme keeps its committed
	// definition, and a new channel with a scheme is left out.
	expected := llotypes.ChannelDefinitions{1: plain, 2: merkle, 3: merkle}
	got, unsupported = withoutSigningSchemes(committed, expected)
	assert.Equal(t, llotypes.ChannelDefinitions{1: plain, 2: plain}, got)
	assert.Equal(t, []llotypes.ChannelID{2, 3}, unsupported)
	assert.Equal(t, merkle, expected[2], "the input must not be modified")
}
