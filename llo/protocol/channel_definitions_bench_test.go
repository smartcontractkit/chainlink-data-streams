package protocol

import (
	"encoding/json"
	"fmt"
	"testing"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// benchOpts mirrors the shape of a real channel's opts: a feed ID plus one ABI
// entry per stream. Decoding it is what verification actually spends its time
// on.
type benchOpts struct {
	FeedID           *[32]byte  `json:"feedID"`
	ABI              []benchABI `json:"abi"`
	BaseUSDFee       string     `json:"baseUSDFee"`
	ExpirationWindow uint32     `json:"expirationWindow"`
}

type benchABI struct {
	StreamID   uint32 `json:"streamID"`
	Multiplier string `json:"multiplier"`
	Type       string `json:"type"`
}

// benchReportCodec decodes the opts on every call, the way every real codec
// does (see reportcodec/evm).
type benchReportCodec struct{}

func (benchReportCodec) Encode(Report, llotypes.ChannelDefinition, *OptsCache) ([]byte, error) {
	return nil, nil
}

func (benchReportCodec) Verify(cd llotypes.ChannelDefinition) error {
	var o benchOpts
	if err := json.Unmarshal(cd.Opts, &o); err != nil {
		return fmt.Errorf("failed to decode opts: %w", err)
	}
	if len(o.ABI) != len(cd.Streams) {
		return fmt.Errorf("ABI length mismatch; expected: %d, got: %d", len(cd.Streams), len(o.ABI))
	}
	return nil
}

func (benchReportCodec) FeedID(cd llotypes.ChannelDefinition) ([32]byte, bool, error) {
	var o benchOpts
	if err := json.Unmarshal(cd.Opts, &o); err != nil {
		return [32]byte{}, false, fmt.Errorf("failed to decode opts: %w", err)
	}
	if o.FeedID == nil {
		return [32]byte{}, false, nil
	}
	return *o.FeedID, true, nil
}

// benchChannelDefs builds n channels of three streams each, with distinct feed
// IDs and roughly 250 bytes of opts apiece.
func benchChannelDefs(tb testing.TB, n int) llotypes.ChannelDefinitions {
	tb.Helper()
	defs := make(llotypes.ChannelDefinitions, n)
	for i := 0; i < n; i++ {
		channelID := llotypes.ChannelID(i + 1)
		var feedID [32]byte
		feedID[0], feedID[1], feedID[2], feedID[3] = byte(i), byte(i>>8), byte(i>>16), byte(i>>24)

		streams := make([]llotypes.Stream, 0, 3)
		abi := make([]benchABI, 0, 3)
		for j := 0; j < 3; j++ {
			streamID := llotypes.StreamID(channelID*10 + llotypes.StreamID(j))
			streams = append(streams, llotypes.Stream{StreamID: streamID, Aggregator: llotypes.AggregatorMedian})
			abi = append(abi, benchABI{StreamID: streamID, Multiplier: "100000000000000000000000000", Type: "int192"})
		}
		opts, err := json.Marshal(benchOpts{
			FeedID:           &feedID,
			ABI:              abi,
			BaseUSDFee:       "0.1",
			ExpirationWindow: 86400,
		})
		if err != nil {
			tb.Fatal(err)
		}
		defs[channelID] = llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMStreamlined,
			Streams:      streams,
			Opts:         opts,
		}
	}
	return defs
}

func benchCodecs() map[llotypes.ReportFormat]ReportCodec {
	return map[llotypes.ReportFormat]ReportCodec{
		llotypes.ReportFormatEVMStreamlined: benchReportCodec{},
	}
}

// BenchmarkVerifyChannelDefinitions measures one analysis of an unchanged set,
// which is what every round pays several times over.
func BenchmarkVerifyChannelDefinitions(b *testing.B) {
	codecs := benchCodecs()
	defs := benchChannelDefs(b, 2000)

	b.Run("uncached", func(b *testing.B) {
		for b.Loop() {
			if err := VerifyChannelDefinitions(codecs, defs); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("cached", func(b *testing.B) {
		cache := NewChannelAnalysisCache()
		if err := VerifyChannelDefinitionsWithCache(codecs, defs, cache); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if err := VerifyChannelDefinitionsWithCache(codecs, defs, cache); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkObservationChannelVerification measures what Observation does per
// round: attribute the baseline findings of the committed set, then verify the
// desired set for admission.
func BenchmarkObservationChannelVerification(b *testing.B) {
	codecs := benchCodecs()
	committed := benchChannelDefs(b, 2000)
	desired := CloneChannelDefinitions(committed)

	run := func(b *testing.B, cache *ChannelAnalysisCache) {
		for b.Loop() {
			if _, err := UnverifiableChannelIDsWithCache(codecs, committed, cache); err != nil {
				b.Fatal(err)
			}
			admitting := ChangedChannelIDs(committed, desired)
			if err := VerifyChannelDefinitionsForAdmissionWithCache(codecs, desired, admitting, cache); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("uncached", func(b *testing.B) { run(b, nil) })
	b.Run("cached", func(b *testing.B) {
		cache := NewChannelAnalysisCache()
		run(b, cache)
	})
}

// BenchmarkValidateObservationChannelVerification measures the verification an
// update-carrying observation triggers, with the cache warmed by the committed
// set the way Observation warms it.
func BenchmarkValidateObservationChannelVerification(b *testing.B) {
	codecs := benchCodecs()
	committed := benchChannelDefs(b, 2000)

	// One channel's opts are changed, which is what an observation that votes an
	// update advocates: the committed set with that update applied.
	updated := CloneChannelDefinitions(committed)
	cd := updated[1]
	cd.Opts = append([]byte(nil), committed[2].Opts...)
	updated[1] = cd

	b.Run("uncached", func(b *testing.B) {
		for b.Loop() {
			// Errors are expected here (the update duplicates a feed ID); the
			// cost is what is being measured.
			_ = VerifyChannelDefinitions(codecs, updated)
		}
	})

	b.Run("cached", func(b *testing.B) {
		cache := NewChannelAnalysisCache()
		if _, err := UnverifiableChannelIDsWithCache(codecs, committed, cache); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			_ = VerifyChannelDefinitionsWithCache(codecs, updated, cache)
		}
	})
}
