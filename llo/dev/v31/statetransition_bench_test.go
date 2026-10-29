package llo

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-data-streams/llo/dev/v31/llotest"
	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm"
)

// StateTransition benchmarks for a steady round: no channel votes, every
// oracle observing every stream, every channel reportable.
//
// Channels are EVMABIEncodeUnpacked with production shaped opts: the native and
// LINK price streams shared by every channel, followed by
// payloadStreamsPerChannel int192 payload streams.
//
// Each oracle observes slightly different values, so their blob payloads are
// distinct and every one is decoded. The blob payload memo is disabled, which
// is the cost of a round whose observations ValidateObservation did not
// already decode.

type stateTransitionBenchCase struct {
	n, f                     int
	channels, streams        int
	payloadStreamsPerChannel int
}

func (c stateTransitionBenchCase) String() string {
	return fmt.Sprintf("n=%d/channels=%d/streams=%d", c.n, c.channels, c.streams)
}

var stateTransitionBenchCases = []stateTransitionBenchCase{
	{n: 4, f: 1, channels: 100, streams: 300, payloadStreamsPerChannel: 3},
	{n: 16, f: 5, channels: 1000, streams: 3000, payloadStreamsPerChannel: 3},
	{n: 31, f: 10, channels: 2000, streams: 5000, payloadStreamsPerChannel: 3},
}

func benchStateTransitionPlugin(c stateTransitionBenchCase) *Plugin {
	return &Plugin{
		ConfigDigest:                        ocrtypes.ConfigDigest{1, 2, 3},
		Logger:                              logger.Nop(),
		N:                                   c.n,
		F:                                   c.f,
		ReportCodecs:                        map[llotypes.ReportFormat]protocol.ReportCodec{llotypes.ReportFormatEVMABIEncodeUnpacked: evm.NewReportCodecEVMABIEncodeUnpacked(logger.Nop(), 1)},
		ChannelCache:                        protocol.NewChannelCache(),
		DefaultMinReportIntervalNanoseconds: 0,
		AggregationFaultTolerance:           c.f,
	}
}

// The native and LINK price streams every channel reports first.
const (
	benchNativeStreamID = llotypes.StreamID(1)
	benchLinkStreamID   = llotypes.StreamID(2)
)

// benchChannelDefinitions spreads payloadStreamsPerChannel median streams per
// channel over the payload stream pool, so streams are shared between
// channels.
func benchChannelDefinitions(c stateTransitionBenchCase) llotypes.ChannelDefinitions {
	type abiEntry struct {
		Multiplier string            `json:"multiplier"`
		StreamID   llotypes.StreamID `json:"streamID"`
		Type       string            `json:"type"`
	}
	type abiOpts struct {
		ABI              []abiEntry `json:"abi"`
		BaseUSDFee       string     `json:"baseUSDFee"`
		ExpirationWindow uint32     `json:"expirationWindow"`
		FeedID           string     `json:"feedID"`
	}

	defs := make(llotypes.ChannelDefinitions, c.channels)
	for i := range c.channels {
		streams := []llotypes.Stream{
			{StreamID: benchNativeStreamID, Aggregator: llotypes.AggregatorMedian},
			{StreamID: benchLinkStreamID, Aggregator: llotypes.AggregatorMedian},
		}
		opts := abiOpts{
			BaseUSDFee:       "0.32",
			ExpirationWindow: 2592000,
			FeedID:           fmt.Sprintf("0x%064x", i+1),
		}
		for k := range c.payloadStreamsPerChannel {
			sid := benchLinkStreamID + 1 + llotypes.StreamID((i*c.payloadStreamsPerChannel+k)%c.streams) //nolint:gosec // G115 bounded by the case
			streams = append(streams, llotypes.Stream{StreamID: sid, Aggregator: llotypes.AggregatorMedian})
			opts.ABI = append(opts.ABI, abiEntry{Multiplier: "1000000000000000000", StreamID: sid, Type: "int192"})
		}
		raw, err := json.Marshal(opts)
		if err != nil {
			panic(err)
		}
		defs[llotypes.ChannelID(i+1)] = llotypes.ChannelDefinition{ //nolint:gosec // G115 bounded by the case
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpacked,
			Streams:      streams,
			Opts:         raw,
		}
	}
	return defs
}

// benchObservationHandles broadcasts one stream value payload per oracle and
// returns each oracle's marshaled blob handle.
func benchObservationHandles(b *testing.B, c stateTransitionBenchCase, blobs *llotest.BlobBroadcastFetcher) [][]byte {
	b.Helper()
	handles := make([][]byte, c.n)
	for o := range c.n {
		values := make(protocol.StreamValues, c.streams+2)
		for s := range c.streams + 2 {
			values[llotypes.StreamID(s+1)] = protocol.ToDecimal(decimal.New(int64(1_000_000_000+s*1_000+o), -8)) //nolint:gosec // G115 bounded by the case
		}
		payload, err := marshalStreamValues(values, 1)
		require.NoError(b, err)
		handle, err := blobs.BroadcastBlob(context.Background(), payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 0})
		require.NoError(b, err)
		handles[o], err = handle.MarshalBinary()
		require.NoError(b, err)
	}
	return handles
}

func BenchmarkStateTransition(b *testing.B) {
	for _, c := range stateTransitionBenchCases {
		b.Run(c.String(), func(b *testing.B) {
			ctx := context.Background()
			p := benchStateTransitionPlugin(c)
			blobs := llotest.NewBlobBroadcastFetcher()
			handles := benchObservationHandles(b, c, blobs)

			const second = uint64(1_000_000_000)
			ts := 1_000 * second
			round := func() []ocrtypes.AttributedObservation {
				aos := make([]ocrtypes.AttributedObservation, c.n)
				for o := range c.n {
					obs, err := encodeObservation(Observation{
						ObservationTimestampNanoseconds: ts,
						SupportedReportFormats:          formatSet(llotypes.ReportFormatEVMABIEncodeUnpacked),
					}, [][]byte{handles[o]})
					require.NoError(b, err)
					aos[o] = ao(o, obs)
				}
				return aos
			}

			// Bootstrap, then install the channels directly: voting them in
			// takes MaxObservationUpdateChannelDefinitionsLength per round.
			kv := newMemKV()
			seqNr := uint64(1)
			_, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, round(), kv, blobs)
			require.NoError(b, err)
			seqNr++
			require.NoError(b, writeChannelState(kv, seqNr, benchChannelDefinitions(c)))

			// Two warm-up rounds: the first gives every channel a watermark,
			// the second is the first in which they report.
			var reportable int
			for range 2 {
				ts += second
				out, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, round(), kv, blobs)
				require.NoError(b, err)
				pre, err := decodePrecursor(out)
				require.NoError(b, err)
				reportable = len(pre.ReportableChannelIDs)
				seqNr++
			}
			require.Equal(b, c.channels, reportable, "steady round reports every channel")

			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				ts += second
				aos := round()
				b.StartTimer()
				if _, err := p.StateTransition(ctx, seqNr, ocrtypes.AttributedQuery{}, aos, kv, blobs); err != nil {
					b.Fatal(err)
				}
				seqNr++
			}
		})
	}
}
