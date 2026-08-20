package llo

import (
	"encoding/binary"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"google.golang.org/protobuf/proto"
)

// KeyValueState key schema. State is split by write frequency so that a round
// touches a small, constant number of keys regardless of how many channels and
// streams exist.
//
//	c/lifecycle -> lifecycle stage string bytes (rarely written)
//	c/defs      -> LLOChannelStateProto: every live channel definition
//	               (written only when the definitions change)
//	c/seqnr     -> uint64 BE seqNr of the last c/defs write
//	r/agg       -> LLOHotStateProto: observation timestamp, validAfter
//	               watermarks, per-channel reportability, and carry-forward
//	               timestamped aggregates (written every round)
//	hh/<streamID BE><aggregator BE>          -> deterministic LLOStreamHistoryHeaderProto (history window index)
//	hc/<streamID BE><aggregator BE><slot BE> -> deterministic LLOStreamHistoryChunkProto (one ring slot of records)
//	hidx        -> concatenated (uint32 BE streamID, uint32 BE aggregator) pairs,
//	               sorted (the history index)
//	hv          -> 1 byte: history layout version
//
// c/seqnr lets readers cache the decoded c/defs in memory across rounds and
// re-read it only when the stored sequence number differs from the cached one
// (see protocol.ChannelCache).
//
// History is stored as a chunked ring rather than one blob per pair: a slot
// holds MaxHistoryChunkRecords records, and a round rewrites only the newest
// one. That makes the per-round write cost a function of the chunk size instead
// of the window depth (~2 KiB rather than ~60 KiB for a full quote window), at
// a read cost of depth/chunkSize point reads instead of one. See
// protocol.RingWindow.
var (
	keyLifecycle    = []byte("c/lifecycle")
	keyChannelState = []byte("c/defs")
	keyChannelSeqNr = []byte("c/seqnr")
	keyHotState     = []byte("r/agg")

	keyHistoryIndex   = []byte("hidx")
	keyHistoryVersion = []byte("hv")

	prefixHistoryHead  = []byte("hh/")
	prefixHistoryChunk = []byte("hc/")
)

// historyLayoutVersion is the schema version of the history keys. A stored
// value other than this one means the layout changed and every window must be
// dropped and re-warmed.
//
// v31 lives under llo/dev and carries no compatibility promise, so a layout
// change is handled by resetting rather than by a dual-read shim — cheap now,
// expensive after graduation.
const historyLayoutVersion byte = 1

// deterministicMarshal marshals a proto message deterministically. All KV
// values and the precursor rely on this for cross-oracle agreement.
var deterministicMarshal = proto.MarshalOptions{Deterministic: true}

func beU64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func beU32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func historyHeaderKey(streamID llotypes.StreamID, agg llotypes.Aggregator) []byte {
	k := append([]byte{}, prefixHistoryHead...)
	k = append(k, beU32(streamID)...)
	return append(k, beU32(uint32(agg))...)
}

func historyChunkKey(streamID llotypes.StreamID, agg llotypes.Aggregator, slot uint32) []byte {
	k := append([]byte{}, prefixHistoryChunk...)
	k = append(k, beU32(streamID)...)
	k = append(k, beU32(uint32(agg))...)
	return append(k, beU32(slot)...)
}

// kvState is the in-memory projection of the replicated KeyValueState for a
// single round. It is loaded from the reader at the start of the round and
// mutations are flushed back through the writer.
type kvState struct {
	lifeCycleStage         llotypes.LifeCycleStage
	observationTimestampNs uint64
	// channelDefinitions is owned by a protocol.ChannelGeneration and is shared
	// with other concurrently-running rounds: treat it as read-only. Callers
	// that mutate must clone first (see cloneChannelDefinitions).
	channelDefinitions llotypes.ChannelDefinitions
	// opts holds the decoded channel opts of the same generation as
	// channelDefinitions. Binding the two together is what stops a
	// concurrently-running round from swapping decoded opts out from under this
	// one; always read opts from here rather than from plugin-wide state.
	opts *protocol.OptsCache
	// channelStateSeqNr is the seqNr at which channelDefinitions were written.
	channelStateSeqNr     uint64
	validAfterNanoseconds map[llotypes.ChannelID]uint64
	// reportedLastRound[cid] is the persisted reportability decision from the
	// previous round's StateTransition. It bakes in every reportability check
	// (min-interval, seconds-resolution, DisableNilStreamValues), so the current
	// round can advance validAfter faithfully without re-deriving it from
	// aggregates that are not persisted.
	reportedLastRound map[llotypes.ChannelID]bool
	// carryForward holds the timestamped aggregates that survive across rounds
	// (newer-wins monotonicity). Regular aggregates are recomputed fresh every
	// round and are never persisted.
	carryForward map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue
}

// loadKVState reconstructs the full state from the replicated store, including
// the hot (per-round) record. A fresh protocol instance (empty store) yields a
// zero-valued kvState.
//
// cache may be nil, in which case the channel definitions are always re-read
// and decoded.
func loadKVState(r ocr3_1types.KeyValueStateReader, cache *protocol.ChannelCache) (*kvState, error) {
	s, err := loadColdKVState(r, cache)
	if err != nil {
		return nil, err
	}
	if err := readHotState(r, s); err != nil {
		return nil, err
	}
	return s, nil
}

// loadColdKVState reconstructs only the rarely-written part of the state: the
// lifecycle stage and the channel definitions (plus their seqNr). It skips the
// r/agg record entirely, avoiding a read and the per-stream decode of the
// carry-forward aggregates.
//
// Callers that only inspect lifeCycleStage / channelDefinitions (Observation,
// ValidateObservation) should use this; the hot fields are left zero-valued.
// StateTransition needs the hot state and must use loadKVState.
func loadColdKVState(r ocr3_1types.KeyValueStateReader, cache *protocol.ChannelCache) (*kvState, error) {
	s := &kvState{
		channelDefinitions:    llotypes.ChannelDefinitions{},
		validAfterNanoseconds: map[llotypes.ChannelID]uint64{},
		reportedLastRound:     map[llotypes.ChannelID]bool{},
		carryForward:          map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{},
	}

	lc, err := r.Read(keyLifecycle)
	if err != nil {
		return nil, fmt.Errorf("read lifecycle: %w", err)
	}
	s.lifeCycleStage = llotypes.LifeCycleStage(lc)

	seqNrBytes, err := r.Read(keyChannelSeqNr)
	if err != nil {
		return nil, fmt.Errorf("read channel seqNr: %w", err)
	}
	if len(seqNrBytes) == 8 {
		s.channelStateSeqNr = binary.BigEndian.Uint64(seqNrBytes)
	}

	gen, err := cache.Load(s.channelStateSeqNr, func() (llotypes.ChannelDefinitions, error) {
		return readChannelState(r)
	})
	if err != nil {
		return nil, err
	}
	s.channelDefinitions = gen.Definitions()
	s.opts = gen.Opts()

	return s, nil
}

// readChannelState reads and decodes the c/defs record.
func readChannelState(r ocr3_1types.KeyValueStateReader) (llotypes.ChannelDefinitions, error) {
	b, err := r.Read(keyChannelState)
	if err != nil {
		return nil, fmt.Errorf("read channel state: %w", err)
	}
	defs := llotypes.ChannelDefinitions{}
	if len(b) == 0 {
		return defs, nil
	}
	pb := &protocol.LLOChannelStateProto{}
	if err := proto.Unmarshal(b, pb); err != nil {
		return nil, fmt.Errorf("unmarshal channel state: %w", err)
	}
	for _, entry := range pb.ChannelDefinitions {
		if entry.ChannelDefinition == nil {
			return nil, fmt.Errorf("nil channel definition for channel %d", entry.ChannelID)
		}
		defs[entry.ChannelID] = protocol.ChannelDefinitionFromProto(entry.ChannelDefinition)
	}
	return defs, nil
}

// readHotState reads and decodes the r/agg record into s.
func readHotState(r ocr3_1types.KeyValueStateReader, s *kvState) error {
	b, err := r.Read(keyHotState)
	if err != nil {
		return fmt.Errorf("read hot state: %w", err)
	}
	if len(b) == 0 {
		return nil
	}
	pb := &protocol.LLOHotStateProto{}
	if err := proto.Unmarshal(b, pb); err != nil {
		return fmt.Errorf("unmarshal hot state: %w", err)
	}
	s.observationTimestampNs = pb.ObservationTimestampNanoseconds
	for _, va := range pb.ValidAfterNanoseconds {
		s.validAfterNanoseconds[va.ChannelID] = va.ValidAfterNanoseconds
	}
	for _, cid := range pb.ReportableChannelIDs {
		s.reportedLastRound[cid] = true
	}
	for _, sa := range pb.StreamAggregates {
		sv, err := protocol.UnmarshalProtoStreamValue(sa.StreamValue)
		if err != nil {
			return fmt.Errorf("unmarshal carry-forward aggregate for stream %d: %w", sa.StreamID, err)
		}
		tsv, ok := sv.(*protocol.TimestampedStreamValue)
		if !ok {
			// Only timestamped values are persisted; ignore anything else.
			continue
		}
		agg := llotypes.Aggregator(sa.Aggregator)
		if s.carryForward[sa.StreamID] == nil {
			s.carryForward[sa.StreamID] = map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		}
		s.carryForward[sa.StreamID][agg] = tsv
	}
	return nil
}

// writeLifecycle persists the lifecycle stage.
func writeLifecycle(w ocr3_1types.KeyValueStateReadWriter, stage llotypes.LifeCycleStage) error {
	return w.Write(keyLifecycle, []byte(stage))
}

// writeChannelState persists the full set of channel definitions along with the
// sequence number of this write. Call it only when the definitions actually
// changed, so that readers can keep serving their in-memory copy.
func writeChannelState(w ocr3_1types.KeyValueStateReadWriter, seqNr uint64, defs llotypes.ChannelDefinitions) error {
	pb := &protocol.LLOChannelStateProto{
		ChannelDefinitions: make([]*protocol.LLOChannelIDAndDefinitionProto, 0, len(defs)),
	}
	for id, cd := range defs {
		pb.ChannelDefinitions = append(pb.ChannelDefinitions, &protocol.LLOChannelIDAndDefinitionProto{
			ChannelID:         id,
			ChannelDefinition: protocol.ChannelDefinitionToProto(cd),
		})
	}
	sort.Slice(pb.ChannelDefinitions, func(i, j int) bool {
		return pb.ChannelDefinitions[i].ChannelID < pb.ChannelDefinitions[j].ChannelID
	})
	b, err := deterministicMarshal.Marshal(pb)
	if err != nil {
		return fmt.Errorf("marshal channel state: %w", err)
	}
	if err := w.Write(keyChannelState, b); err != nil {
		return err
	}
	return w.Write(keyChannelSeqNr, beU64(seqNr))
}

// writeHotState persists the per-round state: observation timestamp, validAfter
// watermarks, reportability decisions, and carry-forward timestamped
// aggregates. It is written every round.
func writeHotState(
	w ocr3_1types.KeyValueStateReadWriter,
	observationTimestampNs uint64,
	validAfterNanoseconds map[llotypes.ChannelID]uint64,
	reportable map[llotypes.ChannelID]bool,
	carryForward map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue,
) error {
	pb := &protocol.LLOHotStateProto{
		ObservationTimestampNanoseconds: observationTimestampNs,
	}

	pb.ValidAfterNanoseconds = make([]*protocol.LLOChannelIDAndValidAfterNanosecondsProto, 0, len(validAfterNanoseconds))
	for id, va := range validAfterNanoseconds {
		pb.ValidAfterNanoseconds = append(pb.ValidAfterNanoseconds, &protocol.LLOChannelIDAndValidAfterNanosecondsProto{
			ChannelID:             id,
			ValidAfterNanoseconds: va,
		})
	}
	sort.Slice(pb.ValidAfterNanoseconds, func(i, j int) bool {
		return pb.ValidAfterNanoseconds[i].ChannelID < pb.ValidAfterNanoseconds[j].ChannelID
	})

	for id, ok := range reportable {
		if ok {
			pb.ReportableChannelIDs = append(pb.ReportableChannelIDs, id)
		}
	}
	sort.Slice(pb.ReportableChannelIDs, func(i, j int) bool {
		return pb.ReportableChannelIDs[i] < pb.ReportableChannelIDs[j]
	})

	for sid, aggregates := range carryForward {
		for agg, tsv := range aggregates {
			if tsv == nil {
				continue
			}
			value, err := tsv.MarshalBinary()
			if err != nil {
				return fmt.Errorf("marshal carry-forward aggregate for stream %d aggregator %v: %w", sid, agg, err)
			}
			pb.StreamAggregates = append(pb.StreamAggregates, &protocol.LLOStreamAggregate{
				StreamID:    sid,
				StreamValue: &protocol.LLOStreamValue{Type: tsv.Type(), Value: value},
				Aggregator:  uint32(agg),
			})
		}
	}
	sort.Slice(pb.StreamAggregates, func(i, j int) bool {
		if pb.StreamAggregates[i].StreamID == pb.StreamAggregates[j].StreamID {
			return pb.StreamAggregates[i].Aggregator < pb.StreamAggregates[j].Aggregator
		}
		return pb.StreamAggregates[i].StreamID < pb.StreamAggregates[j].StreamID
	})

	b, err := deterministicMarshal.Marshal(pb)
	if err != nil {
		return fmt.Errorf("marshal hot state: %w", err)
	}
	return w.Write(keyHotState, b)
}

// histKey identifies one history window. The aggregator is part of the identity
// because the same stream can be aggregated differently by different channels,
// and mixing those series would be silently wrong.
type histKey struct {
	streamID   llotypes.StreamID
	aggregator llotypes.Aggregator
}

// readHistoryHeader returns the stored header for a pair, or nil if none is
// stored.
//
// Decode failures are returned wrapped in protocol.ErrCorruptStreamHistory so
// callers can tell untrusted stored state apart from an actual read failure:
// the first is discarded and re-warmed, the second fails the round.
func readHistoryHeader(r ocr3_1types.KeyValueStateReader, sid llotypes.StreamID, agg llotypes.Aggregator) (*protocol.StreamHistoryHeader, error) {
	b, err := r.Read(historyHeaderKey(sid, agg))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	header, err := protocol.UnmarshalStreamHistoryHeader(b)
	if err != nil {
		return nil, fmt.Errorf("read history header for stream %d aggregator %d: %w", sid, agg, err)
	}
	return header, nil
}

// writeHistoryHeader persists a history header deterministically, returning the
// number of bytes written for telemetry.
func writeHistoryHeader(w ocr3_1types.KeyValueStateReadWriter, sid llotypes.StreamID, agg llotypes.Aggregator, header *protocol.StreamHistoryHeader) (int, error) {
	b, err := header.MarshalBinary()
	if err != nil {
		return 0, fmt.Errorf("marshal history header for stream %d aggregator %d: %w", sid, agg, err)
	}
	return len(b), w.Write(historyHeaderKey(sid, agg), b)
}

// deleteHistoryHeader removes a history header.
func deleteHistoryHeader(w ocr3_1types.KeyValueStateReadWriter, sid llotypes.StreamID, agg llotypes.Aggregator) error {
	return w.Delete(historyHeaderKey(sid, agg))
}

// readHistoryChunk returns the chunk stored in a ring slot, or nil if the slot
// is empty. As with the header, a decode failure is corruption rather than a
// read error.
//
// A chunk left behind by an earlier lap of the ring decodes fine here; it is
// rejected when it is matched against the header, which is what makes slot
// reuse safe. See protocol.RingWindow.Provide.
func readHistoryChunk(r ocr3_1types.KeyValueStateReader, sid llotypes.StreamID, agg llotypes.Aggregator, slot uint32) (*protocol.StreamHistoryChunk, error) {
	b, err := r.Read(historyChunkKey(sid, agg, slot))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	chunk, err := protocol.UnmarshalStreamHistoryChunk(b)
	if err != nil {
		return nil, fmt.Errorf("read history chunk for stream %d aggregator %d slot %d: %w", sid, agg, slot, err)
	}
	return chunk, nil
}

// writeHistoryChunk persists one ring slot deterministically, returning the
// number of bytes written for telemetry.
func writeHistoryChunk(w ocr3_1types.KeyValueStateReadWriter, sid llotypes.StreamID, agg llotypes.Aggregator, chunk *protocol.StreamHistoryChunk) (int, error) {
	b, err := chunk.MarshalBinary()
	if err != nil {
		return 0, fmt.Errorf("marshal history chunk for stream %d aggregator %d slot %d: %w", sid, agg, chunk.Slot(), err)
	}
	return len(b), w.Write(historyChunkKey(sid, agg, chunk.Slot()), b)
}

// deleteHistoryChunk removes one ring slot.
func deleteHistoryChunk(w ocr3_1types.KeyValueStateReadWriter, sid llotypes.StreamID, agg llotypes.Aggregator, slot uint32) error {
	return w.Delete(historyChunkKey(sid, agg, slot))
}

// readHistoryLayoutVersion returns the stored history layout version, or zero
// when nothing has been written yet.
func readHistoryLayoutVersion(r ocr3_1types.KeyValueStateReader) (byte, error) {
	b, err := r.Read(keyHistoryVersion)
	if err != nil {
		return 0, err
	}
	if len(b) != 1 {
		return 0, nil
	}
	return b[0], nil
}

// writeHistoryLayoutVersion records the layout the stored history was written
// with.
func writeHistoryLayoutVersion(w ocr3_1types.KeyValueStateReadWriter) error {
	return w.Write(keyHistoryVersion, []byte{historyLayoutVersion})
}

// readHistoryIndex returns the sorted set of pairs that have history.
//
// The index exists only because the in-round reader offers no range scan: orphan
// cleanup and telemetry need to enumerate history keys. A trailing partial
// entry is ignored rather than failing the round.
func readHistoryIndex(r ocr3_1types.KeyValueStateReader) ([]histKey, error) {
	b, err := r.Read(keyHistoryIndex)
	if err != nil {
		return nil, err
	}
	return decodeHistoryIndex(b), nil
}

// writeHistoryIndex persists the sorted set of pairs that have history.
func writeHistoryIndex(w ocr3_1types.KeyValueStateReadWriter, keys []histKey) error {
	return w.Write(keyHistoryIndex, encodeHistoryIndex(keys))
}

func decodeHistoryIndex(b []byte) []histKey {
	n := len(b) / 8
	keys := make([]histKey, 0, n)
	for i := 0; i < n; i++ {
		keys = append(keys, histKey{
			streamID:   binary.BigEndian.Uint32(b[i*8:]),
			aggregator: llotypes.Aggregator(binary.BigEndian.Uint32(b[i*8+4:])),
		})
	}
	return keys
}

func encodeHistoryIndex(keys []histKey) []byte {
	sorted := append([]histKey{}, keys...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].streamID != sorted[j].streamID {
			return sorted[i].streamID < sorted[j].streamID
		}
		return sorted[i].aggregator < sorted[j].aggregator
	})
	b := make([]byte, 0, len(sorted)*8)
	for _, k := range sorted {
		b = append(b, beU32(k.streamID)...)
		b = append(b, beU32(uint32(k.aggregator))...)
	}
	return b
}
