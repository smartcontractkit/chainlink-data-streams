package llo

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// The decoders fuzzed here all consume bytes the local node did not produce:
// observations come from peers, and the precursor and the KeyValueState records
// come from replicated state a restored snapshot or an earlier version may have
// written. The contract in every case is the same: return a value or an error,
// never panic, and never let a decoded value exceed the bounds the protocol
// enforces.

// payloadFetcher returns the same payload for every handle, so a fuzzer can
// drive the blob path of decodeObservation with arbitrary bytes.
type payloadFetcher struct{ payload []byte }

func (f payloadFetcher) FetchBlob(context.Context, ocr3_1types.BlobHandle) ([]byte, error) {
	return f.payload, nil
}

var _ ocr3_1types.BlobFetcher = payloadFetcher{}

// FuzzDecodeObservation feeds arbitrary bytes through the observation framing,
// proto decode, and blob merge. Observations are attacker-controlled: a
// byzantine peer picks these bytes, and every other oracle decodes them.
func FuzzDecodeObservation(f *testing.F) {
	obs := Observation{
		AttestedPredecessorRetirement: []byte("retirement"),
		ShouldRetire:                  true,
		UnixTimestampNanoseconds:      1_700_000_000_000_000_000,
		RemoveChannelIDs:              map[llotypes.ChannelID]struct{}{7: {}},
		UpdateChannelDefinitions:      llotypes.ChannelDefinitions{1: jsonChannel()},
		SupportedReportFormats:        formatSet(llotypes.ReportFormatJSON, llotypes.ReportFormatEVMPremiumLegacy),
	}
	encoded, err := encodeObservation(obs, nil)
	if err != nil {
		f.Fatal(err)
	}

	handle := make([]byte, 32)
	withHandle, err := encodeObservation(obs, [][]byte{handle})
	if err != nil {
		f.Fatal(err)
	}

	payload, err := marshalStreamValues(protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(123))})
	if err != nil {
		f.Fatal(err)
	}

	f.Add([]byte(encoded), []byte(nil))
	f.Add([]byte(withHandle), payload)
	f.Add([]byte{observationWireVersion}, []byte(nil))
	f.Add([]byte{observationWireVersion, 0xff}, []byte(nil))
	f.Add([]byte{}, []byte(nil))
	f.Add([]byte("not an observation"), []byte("not a payload"))

	f.Fuzz(func(t *testing.T, raw, payload []byte) {
		decoded, err := decodeObservation(context.Background(), ocrtypes.Observation(raw), payloadFetcher{payload: payload}, nil)
		if err != nil {
			return
		}
		// v31 never accepts inline values, and the formats it accepts are
		// bounded and canonicalized, because both feed the state transition.
		if len(decoded.SupportedReportFormats) > protocol.MaxObservationSupportedReportFormatsLength {
			t.Fatalf("decoded %d report formats, max %d", len(decoded.SupportedReportFormats), protocol.MaxObservationSupportedReportFormatsLength)
		}
		// Re-encoding the decoded set is what the wire has to be canonical in.
		wire := sortedFormatsToWire(decoded.SupportedReportFormats)
		for i := 1; i < len(wire); i++ {
			if wire[i-1] >= wire[i] {
				t.Fatalf("report formats are not strictly ascending: %v", wire)
			}
		}
		for id, cd := range decoded.UpdateChannelDefinitions {
			if len(cd.Streams) > protocol.MaxStreamsPerChannel {
				t.Fatalf("channel %d decoded with %d streams, max %d", id, len(cd.Streams), protocol.MaxStreamsPerChannel)
			}
		}
	})
}

// FuzzDecodePrecursor feeds arbitrary bytes through the precursor decoder. The
// precursor crosses from StateTransition to Reports as opaque bytes, so a
// decoded one must be re-encodable and stable: Reports is the only consumer and
// it has no other source for this state.
func FuzzDecodePrecursor(f *testing.F) {
	full, err := encodePrecursor(goldenPrecursor())
	if err != nil {
		f.Fatal(err)
	}
	empty, err := encodePrecursor(precursor{})
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte(full))
	f.Add([]byte(empty))
	f.Add([]byte{0x0a, 0x00})
	f.Add([]byte("not a precursor"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := decodePrecursor(b)
		if err != nil {
			return
		}
		if len(p.SupportByFormat) > protocol.MaxObservationSupportedReportFormatsLength {
			t.Fatalf("decoded %d report format support entries, max %d", len(p.SupportByFormat), protocol.MaxObservationSupportedReportFormatsLength)
		}
		// Re-encoding a decoded precursor must be a fixed point: encode is
		// deterministic, so a value that survives decode has exactly one
		// encoding, and every oracle must agree on it.
		reencoded, err := encodePrecursor(p)
		if err != nil {
			t.Fatalf("re-encode a decoded precursor: %v", err)
		}
		again, err := decodePrecursor(reencoded)
		if err != nil {
			t.Fatalf("re-decode a re-encoded precursor: %v", err)
		}
		twice, err := encodePrecursor(again)
		if err != nil {
			t.Fatalf("re-encode twice: %v", err)
		}
		if string(reencoded) != string(twice) {
			t.Fatal("re-encoding a decoded precursor is not stable")
		}
	})
}

// FuzzLoadKVState feeds arbitrary bytes into every KeyValueState record the
// plugin reads at the start of a round. The store is replicated and may have
// been written by a different version or restored from a snapshot, so a
// corrupt record must fail the load rather than crash the node.
func FuzzLoadKVState(f *testing.F) {
	seeded := newMemKV()
	defs := llotypes.ChannelDefinitions{1: jsonChannel()}
	if err := writeChannelState(seeded, 9, defs); err != nil {
		f.Fatal(err)
	}
	f.Add([]byte("production"), seeded.m[string(keyChannelState)], beU64(9), []byte(nil))
	f.Add([]byte{}, []byte{}, []byte{}, []byte{})
	f.Add([]byte("staging"), []byte("not a proto"), []byte{0x01}, []byte{0x01, 0x02})
	f.Add([]byte(nil), []byte{0x0a, 0x02, 0x08, 0x01}, beU64(1), append(beU32(100), beU32(1)...))

	f.Fuzz(func(t *testing.T, lifecycle, channelState, seqNr, hotState []byte) {
		kv := newMemKV()
		if err := kv.Write(keyLifecycle, lifecycle); err != nil {
			t.Fatal(err)
		}
		if err := kv.Write(keyChannelState, channelState); err != nil {
			t.Fatal(err)
		}
		if err := kv.Write(keyChannelSeqNr, seqNr); err != nil {
			t.Fatal(err)
		}
		if err := kv.Write(keyHotState, hotState); err != nil {
			t.Fatal(err)
		}

		// The cold load is what Observation and ValidateObservation run, and the
		// full load is what StateTransition runs; both must survive the same
		// bytes.
		if _, err := loadColdKVState(kv, protocol.NewChannelCache()); err != nil {
			return
		}
		s, err := loadKVState(kv, nil)
		if err != nil {
			return
		}
		for id, cd := range s.channelDefinitions {
			if len(cd.Streams) > protocol.MaxStreamsPerChannel {
				t.Fatalf("channel %d decoded with %d streams, max %d", id, len(cd.Streams), protocol.MaxStreamsPerChannel)
			}
		}
	})
}

// FuzzDecodeHistoryRecords feeds arbitrary bytes into the three history records:
// the index, a window header, and a ring chunk. Corrupt history is discarded
// and re-warmed rather than trusted, so the decoders must reject it without
// sizing an allocation from it.
func FuzzDecodeHistoryRecords(f *testing.F) {
	f.Add(append(beU32(100), beU32(1)...), []byte(nil), []byte(nil))
	f.Add([]byte{0x01}, []byte("not a proto"), []byte("not a proto"))
	f.Add([]byte{}, []byte{}, []byte{})

	f.Fuzz(func(t *testing.T, index, header, chunk []byte) {
		const (
			sid = llotypes.StreamID(100)
			agg = llotypes.AggregatorMedian
		)

		kv := newMemKV()
		if err := kv.Write(keyHistoryIndex, index); err != nil {
			t.Fatal(err)
		}
		if err := kv.Write(historyHeaderKey(sid, agg), header); err != nil {
			t.Fatal(err)
		}
		if err := kv.Write(historyChunkKey(sid, agg, 0), chunk); err != nil {
			t.Fatal(err)
		}

		if keys, err := readHistoryIndex(kv); err == nil && len(keys) > protocol.MaxHistoryPairs {
			t.Fatalf("decoded %d history pairs, max %d", len(keys), protocol.MaxHistoryPairs)
		}
		if _, err := readHistoryLayoutVersion(kv); err != nil {
			t.Fatalf("reading the layout version must not fail: %v", err)
		}

		decodedHeader, err := readHistoryHeader(kv, sid, agg)
		if err == nil && decodedHeader != nil {
			if len(decodedHeader.Sequences()) != len(decodedHeader.Counts()) {
				t.Fatalf("header decoded with %d sequences and %d counts", len(decodedHeader.Sequences()), len(decodedHeader.Counts()))
			}
			// A decoded header must be usable as a window: that is the only
			// thing the store does with it.
			protocol.NewRingWindow(decodedHeader).AppendPlan()
		}
		decodedChunk, err := readHistoryChunk(kv, sid, agg, 0)
		if err == nil && decodedChunk != nil && decodedChunk.Len() > protocol.MaxHistoryChunkRecords {
			t.Fatalf("chunk decoded with %d records, max %d", decodedChunk.Len(), protocol.MaxHistoryChunkRecords)
		}
	})
}
