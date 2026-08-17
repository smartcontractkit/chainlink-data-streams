package llo

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"

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
//
// c/seqnr lets readers cache the decoded c/defs in memory across rounds and
// re-read it only when the stored sequence number differs from the cached one
// (see channelCache).
var (
	keyLifecycle    = []byte("c/lifecycle")
	keyChannelState = []byte("c/defs")
	keyChannelSeqNr = []byte("c/seqnr")
	keyHotState     = []byte("r/agg")
)

// deterministicMarshal marshals a proto message deterministically. All KV
// values and the precursor rely on this for cross-oracle agreement.
var deterministicMarshal = proto.MarshalOptions{Deterministic: true}

func beU64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// channelCache memoizes the decoded c/defs record across rounds. c/defs is a
// pure function of c/seqnr (both are written by the same StateTransition into
// the same replicated, atomically-committed store), so serving cached
// definitions when the stored sequence number matches the cached one is
// indistinguishable from re-reading them, and therefore consensus-safe.
//
// The comparison is equality, not "cached is older": a node replaying history
// or restoring from a snapshot can legitimately observe an older c/seqnr, and
// serving newer definitions into an older round would diverge.
type channelCache struct {
	mu     sync.Mutex
	loaded bool
	seqNr  uint64
	defs   llotypes.ChannelDefinitions // treated as immutable once stored
}

func newChannelCache() *channelCache {
	return &channelCache{}
}

// get returns the cached definitions if they were loaded at seqNr.
func (c *channelCache) get(seqNr uint64) (llotypes.ChannelDefinitions, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded || c.seqNr != seqNr {
		return nil, false
	}
	return c.defs, true
}

// put replaces the cache with definitions decoded from the c/defs record
// written at seqNr.
func (c *channelCache) put(seqNr uint64, defs llotypes.ChannelDefinitions) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loaded = true
	c.seqNr = seqNr
	c.defs = defs
}

// invalidate drops the cached definitions, forcing the next load to re-read
// them from the store.
func (c *channelCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loaded = false
	c.seqNr = 0
	c.defs = nil
}

// kvState is the in-memory projection of the replicated KeyValueState for a
// single round. It is loaded from the reader at the start of the round and
// mutations are flushed back through the writer.
type kvState struct {
	lifeCycleStage         llotypes.LifeCycleStage
	observationTimestampNs uint64
	// channelDefinitions may be shared with channelCache and with other
	// concurrently-running rounds: treat it as read-only. Callers that mutate
	// must clone first (see cloneChannelDefinitions).
	channelDefinitions llotypes.ChannelDefinitions
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

// loadKVState reconstructs the state from the replicated store. A fresh
// protocol instance (empty store) yields a zero-valued kvState.
//
// cache may be nil, in which case the channel definitions are always re-read
// and decoded.
func loadKVState(r ocr3_1types.KeyValueStateReader, cache *channelCache) (*kvState, error) {
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

	if defs, ok := cache.get(s.channelStateSeqNr); ok {
		s.channelDefinitions = defs
	} else {
		defs, err := readChannelState(r)
		if err != nil {
			return nil, err
		}
		s.channelDefinitions = defs
		cache.put(s.channelStateSeqNr, defs)
	}

	if err := readHotState(r, s); err != nil {
		return nil, err
	}

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
