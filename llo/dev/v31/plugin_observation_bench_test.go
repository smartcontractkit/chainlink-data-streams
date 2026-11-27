package llo

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31/llotest"
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// BenchmarkObservation measures one Observation with a warm ChannelCache and
// ChannelAnalysisCache, over production shaped EVMABIEncodeUnpacked channels.
// No blob pump is attached, so this is the cost of loading the state and
// voting on channels.
//
//   - steady: the desired set equals the committed one, so nothing is admitted.
//   - changing: MaxObservationUpdateChannelDefinitionsLength channels differ,
//     so the admission checks run over the desired set.
func BenchmarkObservation(b *testing.B) {
	ctx := context.Background()
	for _, channels := range []int{500, protocol.MaxOutcomeChannelDefinitionsLength} {
		c := stateTransitionBenchCase{n: 4, f: 1, channels: channels, streams: 3 * channels, payloadStreamsPerChannel: 3}
		committed := benchChannelDefinitions(c)
		changing := maps.Clone(committed)
		for i := range protocol.MaxObservationUpdateChannelDefinitionsLength {
			id := llotypes.ChannelID(i + 1) //nolint:gosec // G115 small constant
			cd := changing[id]
			cd.Opts = bytes.Replace(cd.Opts, []byte(`"0.32"`), []byte(`"0.33"`), 1)
			changing[id] = cd
		}

		for _, bc := range []struct {
			name    string
			desired llotypes.ChannelDefinitions
		}{
			{"steady", committed},
			{"changing", changing},
		} {
			b.Run(fmt.Sprintf("channels=%d/%s", channels, bc.name), func(b *testing.B) {
				p := benchStateTransitionPlugin(c)
				p.ChannelAnalysisCache = protocol.NewChannelAnalysisCache()
				p.ChannelDefinitionCache = &mockChannelDefinitionCache{defs: bc.desired}
				p.ShouldRetireCache = &mockShouldRetireCache{}

				const seqNr = 3
				kv := newMemKV()
				require.NoError(b, writeChannelState(kv, seqNr, committed))

				obs, err := p.Observation(ctx, seqNr, ocrtypes.AttributedQuery{}, kv, nil)
				require.NoError(b, err)
				decoded, err := fetchBlobsAndDecodeObservation(ctx, obs, llotest.NewBlobBroadcastFetcher(), nil)
				require.NoError(b, err)
				if bc.name == "changing" {
					require.Len(b, decoded.UpdateChannelDefinitions, protocol.MaxObservationUpdateChannelDefinitionsLength)
				} else {
					require.Empty(b, decoded.UpdateChannelDefinitions)
				}

				b.ReportAllocs()
				for b.Loop() {
					if _, err := p.Observation(ctx, seqNr, ocrtypes.AttributedQuery{}, kv, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
