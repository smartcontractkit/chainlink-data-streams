package llo

import (
	"context"
	"fmt"
	"testing"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm"
)

// benchChannelDefinition is an EVMABIEncodeUnpacked channel shaped like a
// production one: native and link price streams shared by every channel, and
// one stream of its own.
func benchChannelDefinition(channelID llotypes.ChannelID, baseUSDFee string) llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{
		ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked,
		Streams: []llotypes.Stream{
			{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
			{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
			{StreamID: llotypes.StreamID(channelID) + 2, Aggregator: llotypes.AggregatorMedian},
		},
		Opts: fmt.Appendf(nil, `{"baseUSDFee":%q,"expirationWindow":86400,"feedID":"0x%064x","abi":[{"type":"int192","multiplier":"1"}],"TimeResolution":"s"}`, baseUSDFee, channelID),
	}
}

// BenchmarkValidateObservation measures one ValidateObservation of an
// observation that votes MaxObservationUpdateChannelDefinitionsLength updates,
// which verifies the whole committed set merged with them.
//
//   - nocache verifies every channel from scratch, as before the memoization.
//   - cache/same votes the updates this node voted for, so every channel is
//     served from what Observation recorded.
//   - cache/peer-differs votes updates this node did not, so those channels
//     miss the read-only cache on every call.
func BenchmarkValidateObservation(b *testing.B) {
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
		ours := make(llotypes.ChannelDefinitions, protocol.MaxObservationUpdateChannelDefinitionsLength)
		theirs := make(llotypes.ChannelDefinitions, protocol.MaxObservationUpdateChannelDefinitionsLength)
		for i := range protocol.MaxObservationUpdateChannelDefinitionsLength {
			id := llotypes.ChannelID(i + 1)
			ours[id] = benchChannelDefinition(id, "0.2")
			theirs[id] = benchChannelDefinition(id, "0.3")
		}
		desired := make(llotypes.ChannelDefinitions, n)
		for id, cd := range committed {
			desired[id] = cd
		}
		for id, cd := range ours {
			desired[id] = cd
		}

		previousOutcome, err := protoOutcomeCodecV1{}.Encode(Outcome{
			LifeCycleStage:     protocol.LifeCycleStageProduction,
			ChannelDefinitions: committed,
		})
		require.NoError(b, err)
		outctx := ocr3types.OutcomeContext{SeqNr: 3, PreviousOutcome: previousOutcome}

		newPlugin := func(cache *protocol.ChannelAnalysisCache) *Plugin {
			return &Plugin{
				OutcomeCodec:           protoOutcomeCodecV1{},
				ObservationCodec:       obsCodec,
				ShouldRetireCache:      &mockShouldRetireCache{},
				ChannelDefinitionCache: &mockChannelDefinitionCache{definitions: desired},
				DataSource:             &mockDataSource{},
				Logger:                 logger.Nop(),
				ReportCodecs:           codecs,
				ChannelAnalysisCache:   cache,
			}
		}
		encode := func(updates llotypes.ChannelDefinitions) types.AttributedObservation {
			obs, err := obsCodec.Encode(Observation{UpdateChannelDefinitions: updates})
			require.NoError(b, err)
			return types.AttributedObservation{Observation: obs}
		}

		for _, bc := range []struct {
			name  string
			cache *protocol.ChannelAnalysisCache
			ao    types.AttributedObservation
		}{
			{"nocache", nil, encode(ours)},
			{"cache/same", protocol.NewChannelAnalysisCache(), encode(ours)},
			{"cache/peer-differs", protocol.NewChannelAnalysisCache(), encode(theirs)},
		} {
			b.Run(fmt.Sprintf("channels=%d/%s", n, bc.name), func(b *testing.B) {
				p := newPlugin(bc.cache)
				// Observation records the committed and the desired set.
				_, err := p.Observation(ctx, outctx, types.Query{})
				require.NoError(b, err)

				b.ReportAllocs()
				for b.Loop() {
					if err := p.ValidateObservation(ctx, outctx, types.Query{}, bc.ao); err != nil {
						b.Fatal(err)
					}
				}
			})
		}

		// The previous outcome decode ValidateObservation does before any
		// verification, which no memoization of the checks can remove.
		b.Run(fmt.Sprintf("channels=%d/decode-previous-outcome", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := (protoOutcomeCodecV1{}).Decode(previousOutcome); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkChannelFacts compares, for one channel, the Verify a baseline-only
// analysis needs with the Verify, VerifyForAdmission and FeedID the full
// analysis derives on a cache miss.
func BenchmarkChannelFacts(b *testing.B) {
	codec := evm.NewReportCodecEVMABIEncodeUnpacked(logger.Nop(), 1)
	cd := benchChannelDefinition(1, "0.1")

	b.Run("verify", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := codec.Verify(cd); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("verify+admission+feedID", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := codec.Verify(cd); err != nil {
				b.Fatal(err)
			}
			if err := codec.VerifyForAdmission(cd); err != nil {
				b.Fatal(err)
			}
			if _, _, err := codec.FeedID(cd); err != nil {
				b.Fatal(err)
			}
		}
	})
}
