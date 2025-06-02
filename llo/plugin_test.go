package llo

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

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

func (m *mockChannelDefinitionCache) Definitions() llotypes.ChannelDefinitions {
	return m.definitions
}

type mockDataSource struct {
	s   StreamValues
	err error
}

func (m *mockDataSource) Observe(ctx context.Context, streamValues StreamValues, opts DSOpts) error {
	for k, v := range m.s {
		streamValues[k] = v
	}
	return m.err
}

func Test_ValidateObservation(t *testing.T) {
	p := &Plugin{
		ObservationCodec: NewProtoObservationCodec(logger.Nop()),
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
			StreamValues: map[uint32]StreamValue{
				1: &TimestampedStreamValue{
					StreamValue: &TimestampedStreamValue{
						StreamValue: ToDecimal(decimal.NewFromInt(1)),
					},
				},
			},
		}

		serializedObs, err := NewProtoObservationCodec(logger.Nop()).Encode(obs)
		require.NoError(t, err)

		err = p.ValidateObservation(ctx, ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, types.AttributedObservation{Observation: serializedObs})
		require.EqualError(t, err, "nested stream value on TimestampedStreamValue must be a Decimal, got: TimestampedStreamValue")
	})
}

type mockOnchainConfigCodec struct{}

func (m *mockOnchainConfigCodec) Decode(b []byte) (OnchainConfig, error) {
	return OnchainConfig{}, nil
}

func (m *mockOnchainConfigCodec) Encode(OnchainConfig) ([]byte, error) {
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
		encodedOffchainConfig, err := OffchainConfig{
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
