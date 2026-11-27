package llo

import (
	"context"
	"fmt"
	"maps"
	"testing"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm"
)

// BenchmarkObservation measures the channel definition verification of one
// Observation with a warm ChannelAnalysisCache.
//
//   - steady: the desired set equals the committed one, so nothing is admitted.
//   - changing: MaxObservationUpdateChannelDefinitionsLength channels differ,
//     so the admission checks run over the desired set.
func BenchmarkObservation(b *testing.B) {
	ctx := context.Background()
	codecs := map[llotypes.ReportFormat]protocol.ReportCodec{
		llotypes.ReportFormatEVMABIEncodeUnpacked: evm.NewReportCodecEVMABIEncodeUnpacked(logger.Nop(), 1),
	}
	obsCodec, err := NewProtoObservationCodec(logger.Nop(), false)
	require.NoError(b, err)

	for _, n := range []int{500, protocol.MaxOutcomeChannelDefinitionsLength} {
		committed := make(llotypes.ChannelDefinitions, n)
		for i := range n {
			committed[llotypes.ChannelID(i+1)] = benchChannelDefinition(llotypes.ChannelID(i+1), "0.1")
		}
		changing := maps.Clone(committed)
		for i := range protocol.MaxObservationUpdateChannelDefinitionsLength {
			changing[llotypes.ChannelID(i+1)] = benchChannelDefinition(llotypes.ChannelID(i+1), "0.2")
		}
		previousOutcome, err := protoOutcomeCodecV1{}.Encode(Outcome{
			LifeCycleStage:     protocol.LifeCycleStageProduction,
			ChannelDefinitions: committed,
		})
		require.NoError(b, err)
		outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: previousOutcome}

		for _, bc := range []struct {
			name    string
			desired llotypes.ChannelDefinitions
		}{
			{"steady", committed},
			{"changing", changing},
		} {
			b.Run(fmt.Sprintf("channels=%d/%s", n, bc.name), func(b *testing.B) {
				p := &Plugin{
					OutcomeCodec:           protoOutcomeCodecV1{},
					ObservationCodec:       obsCodec,
					ShouldRetireCache:      &mockShouldRetireCache{},
					ChannelDefinitionCache: &mockChannelDefinitionCache{definitions: bc.desired},
					DataSource:             &mockDataSource{},
					Logger:                 logger.Nop(),
					ReportCodecs:           codecs,
					ChannelAnalysisCache:   protocol.NewChannelAnalysisCache(),
				}
				_, err := p.Observation(ctx, outctx, types.Query{})
				require.NoError(b, err)

				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if _, err := p.Observation(ctx, outctx, types.Query{}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
