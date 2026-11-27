package llo

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// decodeCountingOutcomeCodec counts how often an outcome is decoded.
type decodeCountingOutcomeCodec struct {
	protoOutcomeCodecV1
	decodes atomic.Int64
}

func (c *decodeCountingOutcomeCodec) Decode(b ocr3types.Outcome) (Outcome, error) {
	c.decodes.Add(1)
	return c.protoOutcomeCodecV1.Decode(b)
}

func Test_previousChannelDefinitions(t *testing.T) {
	encode := func(t *testing.T, defs llotypes.ChannelDefinitions) []byte {
		b, err := protoOutcomeCodecV1{}.Encode(Outcome{LifeCycleStage: protocol.LifeCycleStageProduction, ChannelDefinitions: defs})
		require.NoError(t, err)
		return b
	}
	defsA := llotypes.ChannelDefinitions{1: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}}}}
	defsB := llotypes.ChannelDefinitions{2: {ReportFormat: llotypes.ReportFormatJSON, Streams: []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}}}}

	t.Run("decodes an outcome once and again only when it changes", func(t *testing.T) {
		codec := &decodeCountingOutcomeCodec{}
		var m previousChannelDefinitions
		a, b := encode(t, defsA), encode(t, defsB)

		for range 3 {
			got, err := m.get(codec, a)
			require.NoError(t, err)
			require.Equal(t, defsA, got)
		}
		require.Equal(t, int64(1), codec.decodes.Load())

		got, err := m.get(codec, b)
		require.NoError(t, err)
		require.Equal(t, defsB, got)
		require.Equal(t, int64(2), codec.decodes.Load())
	})

	t.Run("does not alias the caller's bytes", func(t *testing.T) {
		codec := &decodeCountingOutcomeCodec{}
		var m previousChannelDefinitions
		a := encode(t, defsA)

		_, err := m.get(codec, a)
		require.NoError(t, err)
		b := encode(t, defsB)
		copy(a, b) // the caller reuses its buffer for a different outcome
		got, err := m.get(codec, a[:len(b)])
		require.NoError(t, err)
		require.Equal(t, defsB, got)
		require.Equal(t, int64(2), codec.decodes.Load())
	})

	t.Run("does not remember a decode error", func(t *testing.T) {
		codec := &decodeCountingOutcomeCodec{}
		var m previousChannelDefinitions
		bad := []byte{0xff, 0xff, 0xff}

		for range 2 {
			_, err := m.get(codec, bad)
			require.Error(t, err)
		}
		require.Equal(t, int64(2), codec.decodes.Load())
	})

	t.Run("concurrent callers with the same outcome share one decode", func(t *testing.T) {
		codec := &decodeCountingOutcomeCodec{}
		var m previousChannelDefinitions
		a := encode(t, defsA)

		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := m.get(codec, a)
				if err == nil && len(got) != 1 {
					err = errors.New("unexpected definitions")
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, int64(1), codec.decodes.Load())
	})
}
