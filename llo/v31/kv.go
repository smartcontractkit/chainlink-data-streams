package llo

import (
	"encoding/binary"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"google.golang.org/protobuf/proto"
)

// KeyValueState key schema. Keys are byte slices; integer key components are
// encoded big-endian so that, although the in-round reader offers no range
// scan, the persisted ordering is deterministic and human-inspectable.
//
//	m/lifecycle        -> lifecycle stage string bytes
//	m/ts               -> uint64 BE observation timestamp (nanoseconds)
//	idx                -> concatenated uint32 BE sorted channel IDs (the channel index)
//	c/<channelID BE>   -> deterministic LLOChannelDefinitionProto
//	v/<channelID BE>   -> uint64 BE validAfterNanoseconds
//	r/<channelID BE>   -> 1 byte (0/1): was the channel reportable in this round
//	t/<streamID BE><aggregator BE> -> deterministic LLOStreamValue (carry-forward timestamped aggregate)
var (
	keyLifecycle = []byte("m/lifecycle")
	keyObsTS     = []byte("m/ts")
	keyIndex     = []byte("idx")

	prefixChannel     = []byte("c/")
	prefixValidAfter  = []byte("v/")
	prefixReported    = []byte("r/")
	prefixTimestamped = []byte("t/")
)

// deterministicMarshal marshals a proto message deterministically. All KV
// values and the precursor rely on this for cross-oracle agreement.
var deterministicMarshal = proto.MarshalOptions{Deterministic: true}

func beU32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func beU64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func channelKey(id llotypes.ChannelID) []byte {
	return append(append([]byte{}, prefixChannel...), beU32(id)...)
}

func validAfterKey(id llotypes.ChannelID) []byte {
	return append(append([]byte{}, prefixValidAfter...), beU32(id)...)
}

func reportedKey(id llotypes.ChannelID) []byte {
	return append(append([]byte{}, prefixReported...), beU32(id)...)
}

func timestampedKey(streamID llotypes.StreamID, agg llotypes.Aggregator) []byte {
	k := append([]byte{}, prefixTimestamped...)
	k = append(k, beU32(streamID)...)
	return append(k, beU32(uint32(agg))...)
}

// kvState is the in-memory projection of the replicated KeyValueState for a
// single StateTransition round. It is loaded from the reader at the start of
// the round and mutations are flushed back through the writer.
type kvState struct {
	lifeCycleStage         llotypes.LifeCycleStage
	observationTimestampNs uint64
	channelDefinitions     llotypes.ChannelDefinitions
	validAfterNanoseconds  map[llotypes.ChannelID]uint64
	// reportedLastRound[cid] is the persisted reportability decision from the
	// previous round's StateTransition (see reportedKey). It bakes in every
	// reportability check (min-interval, seconds-resolution, DisableNilStreamValues),
	// so the current round can advance validAfter faithfully without re-deriving
	// it from aggregates that are not persisted.
	reportedLastRound map[llotypes.ChannelID]bool
	channelIDs        []llotypes.ChannelID // sorted; mirrors the idx key
}

// loadKVState reconstructs the state from the replicated store. A fresh
// protocol instance (empty store) yields a zero-valued kvState.
func loadKVState(r ocr3_1types.KeyValueStateReader) (*kvState, error) {
	s := &kvState{
		channelDefinitions:    llotypes.ChannelDefinitions{},
		validAfterNanoseconds: map[llotypes.ChannelID]uint64{},
		reportedLastRound:     map[llotypes.ChannelID]bool{},
	}

	lc, err := r.Read(keyLifecycle)
	if err != nil {
		return nil, fmt.Errorf("read lifecycle: %w", err)
	}
	s.lifeCycleStage = llotypes.LifeCycleStage(lc)

	ts, err := r.Read(keyObsTS)
	if err != nil {
		return nil, fmt.Errorf("read obsTS: %w", err)
	}
	if len(ts) == 8 {
		s.observationTimestampNs = binary.BigEndian.Uint64(ts)
	}

	idx, err := r.Read(keyIndex)
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	s.channelIDs = decodeChannelIndex(idx)

	for _, id := range s.channelIDs {
		cdBytes, err := r.Read(channelKey(id))
		if err != nil {
			return nil, fmt.Errorf("read channel %d: %w", id, err)
		}
		if len(cdBytes) == 0 {
			// index/def divergence; treat defensively as absent
			continue
		}
		pb := &llocommon.LLOChannelDefinitionProto{}
		if err := proto.Unmarshal(cdBytes, pb); err != nil {
			return nil, fmt.Errorf("unmarshal channel %d: %w", id, err)
		}
		s.channelDefinitions[id] = channelDefinitionFromProto(pb)

		vaBytes, err := r.Read(validAfterKey(id))
		if err != nil {
			return nil, fmt.Errorf("read validAfter %d: %w", id, err)
		}
		if len(vaBytes) == 8 {
			s.validAfterNanoseconds[id] = binary.BigEndian.Uint64(vaBytes)
		}

		repBytes, err := r.Read(reportedKey(id))
		if err != nil {
			return nil, fmt.Errorf("read reported %d: %w", id, err)
		}
		if len(repBytes) == 1 && repBytes[0] == 1 {
			s.reportedLastRound[id] = true
		}
	}

	return s, nil
}

func decodeChannelIndex(b []byte) []llotypes.ChannelID {
	n := len(b) / 4
	ids := make([]llotypes.ChannelID, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, binary.BigEndian.Uint32(b[i*4:]))
	}
	return ids
}

func encodeChannelIndex(ids []llotypes.ChannelID) []byte {
	sorted := append([]llotypes.ChannelID{}, ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	b := make([]byte, 0, len(sorted)*4)
	for _, id := range sorted {
		b = append(b, beU32(id)...)
	}
	return b
}

// writeLifecycle persists the lifecycle stage.
func writeLifecycle(w ocr3_1types.KeyValueStateReadWriter, stage llotypes.LifeCycleStage) error {
	return w.Write(keyLifecycle, []byte(stage))
}

// writeObsTS persists the round's observation timestamp.
func writeObsTS(w ocr3_1types.KeyValueStateReadWriter, ts uint64) error {
	return w.Write(keyObsTS, beU64(ts))
}

// writeChannelIndex persists the sorted set of live channel IDs.
func writeChannelIndex(w ocr3_1types.KeyValueStateReadWriter, ids []llotypes.ChannelID) error {
	return w.Write(keyIndex, encodeChannelIndex(ids))
}

// writeChannelDefinition persists a single channel definition deterministically.
func writeChannelDefinition(w ocr3_1types.KeyValueStateReadWriter, id llotypes.ChannelID, cd llotypes.ChannelDefinition) error {
	b, err := deterministicMarshal.Marshal(makeChannelDefinitionProto(cd))
	if err != nil {
		return fmt.Errorf("marshal channel %d: %w", id, err)
	}
	return w.Write(channelKey(id), b)
}

// deleteChannel removes a channel's definition, validAfter, and reported entries.
func deleteChannel(w ocr3_1types.KeyValueStateReadWriter, id llotypes.ChannelID) error {
	if err := w.Delete(channelKey(id)); err != nil {
		return err
	}
	if err := w.Delete(validAfterKey(id)); err != nil {
		return err
	}
	return w.Delete(reportedKey(id))
}

// writeValidAfter persists a channel's validAfter watermark.
func writeValidAfter(w ocr3_1types.KeyValueStateReadWriter, id llotypes.ChannelID, validAfterNs uint64) error {
	return w.Write(validAfterKey(id), beU64(validAfterNs))
}

// writeReported persists the per-channel reportability decision for this round.
func writeReported(w ocr3_1types.KeyValueStateReadWriter, id llotypes.ChannelID, reported bool) error {
	b := byte(0)
	if reported {
		b = 1
	}
	return w.Write(reportedKey(id), []byte{b})
}

// readTimestampedAggregate returns the carry-forward TimestampedStreamValue for
// a (stream, aggregator) pair, or nil if none is stored.
func readTimestampedAggregate(r ocr3_1types.KeyValueStateReader, sid llotypes.StreamID, agg llotypes.Aggregator) (*llocommon.TimestampedStreamValue, error) {
	b, err := r.Read(timestampedKey(sid, agg))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	pb := &llocommon.LLOStreamValue{}
	if err := proto.Unmarshal(b, pb); err != nil {
		return nil, err
	}
	sv, err := llocommon.UnmarshalProtoStreamValue(pb)
	if err != nil {
		return nil, err
	}
	tsv, ok := sv.(*llocommon.TimestampedStreamValue)
	if !ok {
		return nil, nil
	}
	return tsv, nil
}

// writeTimestampedAggregate persists a timestamped aggregate deterministically.
func writeTimestampedAggregate(w ocr3_1types.KeyValueStateReadWriter, sid llotypes.StreamID, agg llotypes.Aggregator, tsv *llocommon.TimestampedStreamValue) error {
	value, err := tsv.MarshalBinary()
	if err != nil {
		return err
	}
	pb := &llocommon.LLOStreamValue{Type: tsv.Type(), Value: value}
	b, err := deterministicMarshal.Marshal(pb)
	if err != nil {
		return err
	}
	return w.Write(timestampedKey(sid, agg), b)
}

// deleteTimestampedAggregate removes a carry-forward timestamped aggregate.
func deleteTimestampedAggregate(w ocr3_1types.KeyValueStateReadWriter, sid llotypes.StreamID, agg llotypes.Aggregator) error {
	return w.Delete(timestampedKey(sid, agg))
}
