package llo

import (
	"context"
	"testing"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/stretchr/testify/assert"
)

type mockShouldRetireCache struct {
	shouldRetire bool
	err          error
}

func (m *mockShouldRetireCache) ShouldRetire() (bool, error) {
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
		Config: Config{true},
	}

	t.Run("SeqNr < 1 is not valid", func(t *testing.T) {
		err := p.ValidateObservation(ocr3types.OutcomeContext{}, types.Query{}, types.AttributedObservation{})
		assert.EqualError(t, err, "Invalid SeqNr: 0")
	})
	t.Run("SeqNr == 1 enforces empty observation", func(t *testing.T) {
		err := p.ValidateObservation(ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, types.AttributedObservation{Observation: []byte{1}})
		assert.EqualError(t, err, "Expected empty observation for first round, got: 0x01")
	})
}
