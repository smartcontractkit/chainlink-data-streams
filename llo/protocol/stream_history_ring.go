package protocol

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"google.golang.org/protobuf/proto"
)

// ErrHistoryChunkNotLoaded is returned when an operation needs a chunk the
// caller has not provided yet. It is a programming error, not corruption: the
// caller is expected to read exactly the chunks AppendPlan and ReadPlan name,
// and to hand them to Provide before mutating or reading the window.
var ErrHistoryChunkNotLoaded = errors.New("stream history chunk not loaded")

// ErrHistoryAlreadyAppended is returned by RingWindow.Append when a pair is
// appended to twice in one round. See Append for why that cannot be allowed.
var ErrHistoryAlreadyAppended = errors.New("stream history already appended this round")

// StreamHistoryChunk is one slot of a chunked history ring: a run of
// consecutive records for one (streamID, aggregator) pair, oldest first.
//
// Only the newest chunk of a window is ever rewritten. Once a chunk holds
// MaxHistoryChunkRecords records it is sealed: immutable for as long as it is
// retained, and never part of a write set again. That is what makes per-round
// write cost a function of the chunk size rather than of the window depth.
//
// The records slice is unexported so a caller cannot assemble a chunk that
// violates the invariants the decoder enforces.
type StreamHistoryChunk struct {
	sequence uint64
	records  []StreamHistoryRecord
}

// Sequence is the chunk's absolute index in the window, monotonically
// increasing over the window's life. The key holds only the slot
// (sequence mod MaxHistoryChunkSlots), so this is what tells a live chunk apart
// from a stale one left behind by an earlier lap of the ring.
func (c *StreamHistoryChunk) Sequence() uint64 {
	if c == nil {
		return 0
	}
	return c.sequence
}

// Records returns the chunk's records, oldest first. The slice aliases internal
// state and must be treated as read-only; it is not copied because this is on
// the per-round hot path.
func (c *StreamHistoryChunk) Records() []StreamHistoryRecord {
	if c == nil {
		return nil
	}
	return c.records
}

// Len is the number of records in the chunk.
func (c *StreamHistoryChunk) Len() int {
	if c == nil {
		return 0
	}
	return len(c.records)
}

// Slot is the ring slot this chunk occupies.
func (c *StreamHistoryChunk) Slot() uint32 {
	return HistoryChunkSlot(c.Sequence())
}

// ToProto converts the chunk to its wire form.
func (c *StreamHistoryChunk) ToProto() (*LLOStreamHistoryChunkProto, error) {
	pb := &LLOStreamHistoryChunkProto{
		Sequence: c.sequence,
		Records:  make([]*LLOStreamHistoryRecord, 0, len(c.records)),
	}
	for i, rec := range c.records {
		value, err := marshalProtoStreamValue(rec.Value)
		if err != nil {
			return nil, fmt.Errorf("marshal chunk %d record %d: %w", c.sequence, i, err)
		}
		pb.Records = append(pb.Records, &LLOStreamHistoryRecord{
			ObservedAtNanoseconds: rec.ObservedAtNanoseconds,
			Value:                 value,
		})
	}
	return pb, nil
}

// MarshalBinary serializes the chunk deterministically for storage.
func (c *StreamHistoryChunk) MarshalBinary() ([]byte, error) {
	pb, err := c.ToProto()
	if err != nil {
		return nil, err
	}
	return deterministicHistoryMarshal.Marshal(pb)
}

// StreamHistoryChunkFromProto validates and converts a decoded chunk.
//
// Everything checkable without the header is checked here; the header-relative
// checks (is this sequence still retained, does it hold the number of records
// the header claims) happen in RingWindow.Provide.
func StreamHistoryChunkFromProto(pb *LLOStreamHistoryChunkProto) (*StreamHistoryChunk, error) {
	if pb == nil {
		return nil, fmt.Errorf("%w: nil chunk proto", ErrCorruptStreamHistory)
	}
	if len(pb.Records) == 0 {
		return nil, fmt.Errorf("%w: chunk %d holds no records", ErrCorruptStreamHistory, pb.Sequence)
	}
	if len(pb.Records) > MaxHistoryChunkRecords {
		return nil, fmt.Errorf("%w: chunk %d holds %d records, exceeding MaxHistoryChunkRecords %d",
			ErrCorruptStreamHistory, pb.Sequence, len(pb.Records), MaxHistoryChunkRecords)
	}

	c := &StreamHistoryChunk{
		sequence: pb.Sequence,
		records:  make([]StreamHistoryRecord, 0, len(pb.Records)),
	}
	var prev uint64
	for i, rec := range pb.Records {
		if rec == nil {
			return nil, fmt.Errorf("%w: chunk %d record %d is nil", ErrCorruptStreamHistory, pb.Sequence, i)
		}
		if i > 0 && rec.ObservedAtNanoseconds <= prev {
			return nil, fmt.Errorf("%w: chunk %d record %d timestamp %d is not strictly after %d",
				ErrCorruptStreamHistory, pb.Sequence, i, rec.ObservedAtNanoseconds, prev)
		}
		// The per-record cap is enforced on decode as well as on append, so the
		// bound holds for state this process did not write: a chunk restored
		// after a restart, or one corrupted in storage, cannot exceed what the
		// byte budget was sized for.
		if size := proto.Size(rec); size > MaxHistoryRecordBytes {
			return nil, fmt.Errorf("%w: chunk %d record %d is %d bytes, exceeding the maximum of %d",
				ErrCorruptStreamHistory, pb.Sequence, i, size, MaxHistoryRecordBytes)
		}
		sv, err := UnmarshalProtoStreamValue(rec.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %d record %d: %s", ErrCorruptStreamHistory, pb.Sequence, i, err)
		}
		prev = rec.ObservedAtNanoseconds
		c.records = append(c.records, StreamHistoryRecord{
			ObservedAtNanoseconds: rec.ObservedAtNanoseconds,
			Value:                 sv,
		})
	}
	return c, nil
}

// UnmarshalStreamHistoryChunk decodes and validates a stored chunk.
func UnmarshalStreamHistoryChunk(data []byte) (*StreamHistoryChunk, error) {
	pb := &LLOStreamHistoryChunkProto{}
	if err := proto.Unmarshal(data, pb); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCorruptStreamHistory, err)
	}
	return StreamHistoryChunkFromProto(pb)
}

// StreamHistoryHeader is the index of a chunked history window: which chunks
// are retained, how full each one is, and when each one starts.
//
// It is deliberately sufficient on its own. Every decision a round makes — is
// there enough depth, which chunks must be read, may this value be appended,
// which chunk falls out — is taken from the header without reading a chunk, so
// a pair that is still warming up costs exactly one read for the whole round.
//
// Invariants, enforced on decode and by every mutator:
//
//   - len(counts) == len(chunkFirst) <= MaxHistoryChunkSlots.
//   - counts[i] == MaxHistoryChunkRecords for every i but the last, which is in
//     [1, MaxHistoryChunkRecords].
//   - chunkFirst is strictly increasing, and chunkFirst[last] <= last.
//   - total() < requiredCount + MaxHistoryChunkRecords, and dropping the oldest
//     chunk would leave fewer than requiredCount records — retention keeps as
//     few whole chunks as cover the required depth.
//   - an empty window has firstSequence == 0 and last == 0.
type StreamHistoryHeader struct {
	requiredCount uint32
	firstSequence uint64
	counts        []uint32
	chunkFirst    []uint64
	last          uint64
}

// HistoryChunkSlot maps an absolute chunk sequence onto its ring slot.
func HistoryChunkSlot(sequence uint64) uint32 {
	return uint32(sequence % MaxHistoryChunkSlots)
}

// RequiredCount is the window capacity: the deepest history any live channel
// requires for this pair. Zero means the pair is torn down.
func (h *StreamHistoryHeader) RequiredCount() uint32 {
	if h == nil {
		return 0
	}
	return h.requiredCount
}

// FirstSequence is the absolute sequence of the oldest retained chunk.
func (h *StreamHistoryHeader) FirstSequence() uint64 {
	if h == nil {
		return 0
	}
	return h.firstSequence
}

// ChunkCount is the number of retained chunks.
func (h *StreamHistoryHeader) ChunkCount() int {
	if h == nil {
		return 0
	}
	return len(h.counts)
}

// Counts returns a copy of the per-chunk record counts, oldest first.
func (h *StreamHistoryHeader) Counts() []uint32 {
	if h == nil {
		return nil
	}
	return append([]uint32(nil), h.counts...)
}

// Sequences returns the absolute sequences of the retained chunks, oldest
// first.
func (h *StreamHistoryHeader) Sequences() []uint64 {
	if h == nil {
		return nil
	}
	seqs := make([]uint64, 0, len(h.counts))
	for i := range h.counts {
		seqs = append(seqs, h.firstSequence+uint64(i))
	}
	return seqs
}

// Len is the number of records the window holds across all retained chunks. It
// may exceed RequiredCount by up to one chunk, because retention works in whole
// chunks; readers ask for an exact depth and never see the overshoot.
func (h *StreamHistoryHeader) Len() int {
	return int(h.total())
}

func (h *StreamHistoryHeader) total() uint32 {
	if h == nil {
		return 0
	}
	var total uint32
	for _, c := range h.counts {
		total += c
	}
	return total
}

// FirstObservationTimestampNanoseconds is the timestamp of the oldest retained
// record, or zero when the window is empty.
func (h *StreamHistoryHeader) FirstObservationTimestampNanoseconds() uint64 {
	if h == nil || len(h.chunkFirst) == 0 {
		return 0
	}
	return h.chunkFirst[0]
}

// LastObservationTimestampNanoseconds is the timestamp of the newest record, or
// zero when the window is empty. The strictly-newer append rule is decided
// against this alone, so appending costs no chunk read beyond the tail.
func (h *StreamHistoryHeader) LastObservationTimestampNanoseconds() uint64 {
	if h == nil {
		return 0
	}
	return h.last
}

// index returns the position of a sequence among the retained chunks.
func (h *StreamHistoryHeader) index(sequence uint64) (int, bool) {
	if sequence < h.firstSequence {
		return 0, false
	}
	i := sequence - h.firstSequence
	if i >= uint64(len(h.counts)) {
		return 0, false
	}
	return int(i), true
}

// ToProto converts the header to its wire form.
func (h *StreamHistoryHeader) ToProto() *LLOStreamHistoryHeaderProto {
	pb := &LLOStreamHistoryHeaderProto{
		RequiredCount:                       h.requiredCount,
		FirstSequence:                       h.firstSequence,
		LastObservationTimestampNanoseconds: h.last,
	}
	if len(h.counts) > 0 {
		pb.Counts = append([]uint32(nil), h.counts...)
		pb.ChunkFirstObservationTimestampNanoseconds = append([]uint64(nil), h.chunkFirst...)
	}
	return pb
}

// MarshalBinary serializes the header deterministically for storage.
func (h *StreamHistoryHeader) MarshalBinary() ([]byte, error) {
	return deterministicHistoryMarshal.Marshal(h.ToProto())
}

// StreamHistoryHeaderFromProto validates and converts a decoded header.
//
// Every check guards against corrupt or byzantine stored state, and every
// failure is ErrCorruptStreamHistory so callers can uniformly discard the whole
// window and re-warm. A header that decodes is trustworthy enough to plan reads
// and evictions from without cross-checking it against the chunks.
func StreamHistoryHeaderFromProto(pb *LLOStreamHistoryHeaderProto) (*StreamHistoryHeader, error) {
	if pb == nil {
		return nil, fmt.Errorf("%w: nil header proto", ErrCorruptStreamHistory)
	}
	if pb.RequiredCount > MaxHistoryRecordsPerPair {
		return nil, fmt.Errorf("%w: requiredCount %d exceeds MaxHistoryRecordsPerPair %d",
			ErrCorruptStreamHistory, pb.RequiredCount, MaxHistoryRecordsPerPair)
	}
	if len(pb.Counts) != len(pb.ChunkFirstObservationTimestampNanoseconds) {
		return nil, fmt.Errorf("%w: %d counts but %d chunk timestamps",
			ErrCorruptStreamHistory, len(pb.Counts), len(pb.ChunkFirstObservationTimestampNanoseconds))
	}
	if len(pb.Counts) > MaxHistoryChunkSlots {
		return nil, fmt.Errorf("%w: %d retained chunks exceeds MaxHistoryChunkSlots %d",
			ErrCorruptStreamHistory, len(pb.Counts), MaxHistoryChunkSlots)
	}

	h := &StreamHistoryHeader{
		requiredCount: pb.RequiredCount,
		firstSequence: pb.FirstSequence,
		last:          pb.LastObservationTimestampNanoseconds,
	}

	if len(pb.Counts) == 0 {
		if pb.FirstSequence != 0 || pb.LastObservationTimestampNanoseconds != 0 {
			return nil, fmt.Errorf("%w: empty window with firstSequence %d and last timestamp %d",
				ErrCorruptStreamHistory, pb.FirstSequence, pb.LastObservationTimestampNanoseconds)
		}
		return h, nil
	}

	if pb.FirstSequence > math.MaxUint64-uint64(len(pb.Counts)) {
		return nil, fmt.Errorf("%w: firstSequence %d overflows with %d chunks",
			ErrCorruptStreamHistory, pb.FirstSequence, len(pb.Counts))
	}

	var total uint32
	for i, count := range pb.Counts {
		last := i == len(pb.Counts)-1
		switch {
		case count == 0:
			return nil, fmt.Errorf("%w: chunk %d holds no records", ErrCorruptStreamHistory, i)
		case count > MaxHistoryChunkRecords:
			return nil, fmt.Errorf("%w: chunk %d holds %d records, exceeding MaxHistoryChunkRecords %d",
				ErrCorruptStreamHistory, i, count, MaxHistoryChunkRecords)
		case !last && count != MaxHistoryChunkRecords:
			// Only the newest chunk may be partial: a sealed chunk is never
			// rewritten, so a short one in the middle means the series has a
			// hole the counts do not describe.
			return nil, fmt.Errorf("%w: sealed chunk %d holds %d records, expected %d",
				ErrCorruptStreamHistory, i, count, MaxHistoryChunkRecords)
		}
		total += count

		if i > 0 && pb.ChunkFirstObservationTimestampNanoseconds[i] <= pb.ChunkFirstObservationTimestampNanoseconds[i-1] {
			return nil, fmt.Errorf("%w: chunk %d starts at %d, which is not strictly after %d",
				ErrCorruptStreamHistory, i, pb.ChunkFirstObservationTimestampNanoseconds[i], pb.ChunkFirstObservationTimestampNanoseconds[i-1])
		}
	}

	newestFirst := pb.ChunkFirstObservationTimestampNanoseconds[len(pb.Counts)-1]
	newestCount := pb.Counts[len(pb.Counts)-1]
	switch {
	case newestCount == 1 && newestFirst != pb.LastObservationTimestampNanoseconds:
		return nil, fmt.Errorf("%w: single-record newest chunk starts at %d but the window ends at %d",
			ErrCorruptStreamHistory, newestFirst, pb.LastObservationTimestampNanoseconds)
	case newestFirst > pb.LastObservationTimestampNanoseconds:
		return nil, fmt.Errorf("%w: newest chunk starts at %d, after the window end %d",
			ErrCorruptStreamHistory, newestFirst, pb.LastObservationTimestampNanoseconds)
	}

	// Retention keeps as few whole chunks as cover the required depth. Both
	// bounds matter: the first is what makes the window usable, the second is
	// what stops a byzantine or corrupt header from making a round read and
	// hold far more than the byte budget was sized for.
	if total-pb.Counts[0] >= pb.RequiredCount {
		return nil, fmt.Errorf("%w: %d records retained for a required depth of %d; the oldest chunk (%d records) is redundant",
			ErrCorruptStreamHistory, total, pb.RequiredCount, pb.Counts[0])
	}
	if uint64(total) >= uint64(pb.RequiredCount)+MaxHistoryChunkRecords {
		return nil, fmt.Errorf("%w: %d records retained exceeds the required depth %d by a whole chunk",
			ErrCorruptStreamHistory, total, pb.RequiredCount)
	}

	h.counts = append([]uint32(nil), pb.Counts...)
	h.chunkFirst = append([]uint64(nil), pb.ChunkFirstObservationTimestampNanoseconds...)
	return h, nil
}

// UnmarshalStreamHistoryHeader decodes and validates a stored header.
func UnmarshalStreamHistoryHeader(data []byte) (*StreamHistoryHeader, error) {
	pb := &LLOStreamHistoryHeaderProto{}
	if err := proto.Unmarshal(data, pb); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCorruptStreamHistory, err)
	}
	return StreamHistoryHeaderFromProto(pb)
}

// RingWriteSet is what one round's mutations mean for storage. It is produced
// by RingWindow.WriteSet and executed by the plugin, which owns the KV types.
//
// Writes must be applied before deletes. A slot is never both written and
// deleted in the same round — WriteSet drops the tail slot from DeletedSlots if
// it somehow appears — but ordering the two makes that independent of the
// caller getting the argument order right.
type RingWriteSet struct {
	// Header is the header to write, or nil when it did not change.
	Header *StreamHistoryHeader
	// Chunk is the newest chunk, the only one a round ever rewrites, or nil
	// when nothing was appended.
	Chunk *StreamHistoryChunk
	// DeletedSlots are ring slots to delete, ascending: chunks evicted this
	// round, or every slot when the window was reset.
	DeletedSlots []uint32
}

// Empty reports whether the round changed nothing about this window.
func (s RingWriteSet) Empty() bool {
	return s.Header == nil && s.Chunk == nil && len(s.DeletedSlots) == 0
}

// RingWindow is the per-round working copy of one pair's chunked history.
//
// Chunks are supplied by the caller rather than read by this type, which is
// what keeps the layout logic free of KV types and independently testable. The
// protocol is:
//
//	w := NewRingWindow(header)          // header read from storage, nil if none
//	for _, seq := range w.AppendPlan()  // and/or w.ReadPlan(n)
//	    w.Provide(chunk)                // chunks read from storage
//	w.Append(...) / w.Newest(n)         // mutate and read
//	w.WriteSet()                        // what to persist
//
// AppendPlan and ReadPlan name chunks by absolute sequence; HistoryChunkSlot
// turns those into the keys to read.
type RingWindow struct {
	header  StreamHistoryHeader
	chunks  map[uint64]*StreamHistoryChunk
	deleted []uint32

	headerDirty bool
	dirtyChunk  *StreamHistoryChunk
	// stored reports whether this round already appended a record, which it is
	// allowed to do at most once. See Append.
	stored bool
}

// NewRingWindow returns a working copy over an existing header. A nil header
// means nothing is stored yet.
func NewRingWindow(header *StreamHistoryHeader) *RingWindow {
	w := &RingWindow{chunks: map[uint64]*StreamHistoryChunk{}}
	if header != nil {
		w.header = *header
		w.header.counts = append([]uint32(nil), header.counts...)
		w.header.chunkFirst = append([]uint64(nil), header.chunkFirst...)
	}
	return w
}

// ResetRingWindow returns an empty working copy whose write set deletes every
// slot of the ring.
//
// This is the recovery path for a window that failed to decode. The header is
// the only thing that says which chunks exist, so when it is unusable the only
// safe move is to delete the whole slot space — which is bounded, and is the
// reason the layout uses a fixed ring rather than an unbounded sequence. The
// cost of recovery is a warmup, never a halted round.
func ResetRingWindow() *RingWindow {
	w := NewRingWindow(nil)
	w.headerDirty = true
	w.deleted = make([]uint32, 0, MaxHistoryChunkSlots)
	for slot := range uint32(MaxHistoryChunkSlots) {
		w.deleted = append(w.deleted, slot)
	}
	return w
}

// Header returns the window's current header.
func (w *RingWindow) Header() *StreamHistoryHeader {
	if w == nil {
		return nil
	}
	return &w.header
}

// RequiredCount is the window capacity.
func (w *RingWindow) RequiredCount() uint32 { return w.Header().RequiredCount() }

// Len is the number of records retained, which may overshoot RequiredCount by
// less than one chunk.
func (w *RingWindow) Len() int { return w.Header().Len() }

// FirstObservationTimestampNanoseconds is the timestamp of the oldest retained
// record, or zero when empty.
func (w *RingWindow) FirstObservationTimestampNanoseconds() uint64 {
	return w.Header().FirstObservationTimestampNanoseconds()
}

// LastObservationTimestampNanoseconds is the timestamp of the newest record, or
// zero when empty.
func (w *RingWindow) LastObservationTimestampNanoseconds() uint64 {
	return w.Header().LastObservationTimestampNanoseconds()
}

// AppendPlan returns the sequences that must be provided before Append can
// succeed: the newest chunk, when there is one with room left. Appending into a
// sealed or absent chunk starts a new one and needs nothing loaded.
func (w *RingWindow) AppendPlan() []uint64 {
	if w.header.requiredCount == 0 {
		return nil
	}
	last := len(w.header.counts) - 1
	if last < 0 || w.header.counts[last] >= MaxHistoryChunkRecords {
		return nil
	}
	return []uint64{w.header.firstSequence + uint64(last)}
}

// ReadPlan returns the sequences covering the newest n records, oldest first.
//
// It returns ErrInsufficientStreamHistory when fewer than n records are
// retained — decided from the header, so a warming-up pair costs no chunk reads
// at all. A short window is never silently substituted.
func (w *RingWindow) ReadPlan(n uint32) ([]uint64, error) {
	if n == 0 {
		return nil, nil
	}
	total := w.header.total()
	if total < n {
		return nil, fmt.Errorf("%w: have %d, need %d", ErrInsufficientStreamHistory, total, n)
	}

	var acc uint32
	first := len(w.header.counts) - 1
	for ; first >= 0; first-- {
		acc += w.header.counts[first]
		if acc >= n {
			break
		}
	}

	seqs := make([]uint64, 0, len(w.header.counts)-first)
	for i := first; i < len(w.header.counts); i++ {
		seqs = append(seqs, w.header.firstSequence+uint64(i))
	}
	return seqs, nil
}

// Loaded reports whether a chunk has already been provided.
//
// Callers must consult this before re-reading a planned sequence: a chunk the
// window has already been given may have been appended to since, so the stored
// bytes are stale until the write set is flushed and providing them again would
// look like corruption.
func (w *RingWindow) Loaded(sequence uint64) bool {
	_, ok := w.chunks[sequence]
	return ok
}

// Provide hands a decoded chunk to the window, checking it against the header.
//
// This is where a stale chunk is caught: reads are by ring slot, so a slot left
// behind by an earlier lap decodes fine but carries a sequence the header no
// longer retains. Treating that as corruption — rather than as data — is what
// makes slot reuse safe.
func (w *RingWindow) Provide(chunk *StreamHistoryChunk) error {
	if chunk == nil {
		return fmt.Errorf("%w: nil chunk", ErrCorruptStreamHistory)
	}
	i, ok := w.header.index(chunk.sequence)
	if !ok {
		return fmt.Errorf("%w: chunk sequence %d is not retained (%d chunks from %d)",
			ErrCorruptStreamHistory, chunk.sequence, len(w.header.counts), w.header.firstSequence)
	}
	if len(chunk.records) != int(w.header.counts[i]) {
		return fmt.Errorf("%w: chunk %d holds %d records, header says %d",
			ErrCorruptStreamHistory, chunk.sequence, len(chunk.records), w.header.counts[i])
	}
	if chunk.records[0].ObservedAtNanoseconds != w.header.chunkFirst[i] {
		return fmt.Errorf("%w: chunk %d starts at %d, header says %d",
			ErrCorruptStreamHistory, chunk.sequence, chunk.records[0].ObservedAtNanoseconds, w.header.chunkFirst[i])
	}
	if newest := chunk.records[len(chunk.records)-1].ObservedAtNanoseconds; i == len(w.header.counts)-1 && newest != w.header.last {
		return fmt.Errorf("%w: newest chunk %d ends at %d, header says %d",
			ErrCorruptStreamHistory, chunk.sequence, newest, w.header.last)
	}
	w.chunks[chunk.sequence] = chunk
	return nil
}

// Newest returns the n most recent records, oldest first, from the chunks
// ReadPlan named. It returns ErrInsufficientStreamHistory if fewer than n are
// retained, and ErrHistoryChunkNotLoaded if a planned chunk was not provided.
func (w *RingWindow) Newest(n uint32) ([]StreamHistoryRecord, error) {
	seqs, err := w.ReadPlan(n)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}

	records := make([]StreamHistoryRecord, 0, n+MaxHistoryChunkRecords)
	for _, seq := range seqs {
		chunk := w.chunks[seq]
		if chunk == nil {
			return nil, fmt.Errorf("%w: sequence %d", ErrHistoryChunkNotLoaded, seq)
		}
		// Monotonicity within a chunk is checked on decode and the per-chunk
		// start timestamps are checked against each other in the header; this
		// is the remaining seam, between one chunk's end and the next one's
		// start.
		if len(records) > 0 {
			prev := records[len(records)-1].ObservedAtNanoseconds
			if chunk.records[0].ObservedAtNanoseconds <= prev {
				return nil, fmt.Errorf("%w: chunk %d starts at %d, which is not strictly after %d",
					ErrCorruptStreamHistory, seq, chunk.records[0].ObservedAtNanoseconds, prev)
			}
		}
		records = append(records, chunk.records...)
	}
	return records[uint32(len(records))-n:], nil
}

// SetRequiredCount updates the capacity, evicting chunks the new depth no
// longer needs. It reports whether anything changed, so callers can avoid
// writing an unmodified header.
//
// Growing the capacity does not synthesize records and touches no chunk: the
// extra depth fills over subsequent rounds, and expressions needing it stay
// unsatisfied meanwhile. Shrinking is deletes only — a sealed chunk is never
// rewritten. Setting zero tears the window down, evicting everything.
func (w *RingWindow) SetRequiredCount(requiredCount uint32) (changed bool, err error) {
	if requiredCount > MaxHistoryRecordsPerPair {
		return false, fmt.Errorf("requiredCount %d exceeds MaxHistoryRecordsPerPair %d", requiredCount, MaxHistoryRecordsPerPair)
	}
	if w.header.requiredCount == requiredCount {
		return false, nil
	}
	w.header.requiredCount = requiredCount
	w.headerDirty = true
	w.evict()
	return true, nil
}

// Append adds a value to the newest end of the window, sealing the newest chunk
// and starting another when it fills, and evicting whole chunks from the oldest
// end once they are no longer needed. It reports whether the record was stored.
//
// A record is stored only if its timestamp is strictly newer than the current
// newest. That single rule keeps the series monotonic and prevents the two ways
// a value could otherwise be double counted: a carry-forward timestamped
// aggregate re-appended every round until it refreshes, and a non-advancing or
// regressing consensus observation timestamp. Neither is an error — both are
// normal — so a rejected append returns (false, nil).
//
// A pair with zero capacity stores nothing. A value serializing to more than
// MaxHistoryRecordBytes is rejected with ErrHistoryRecordTooLarge, leaving an
// honest gap rather than a window larger than the byte budget assumed.
//
// At most one record may be stored per round, and a second attempt returns
// ErrHistoryAlreadyAppended. Two rules depend on it. The write set carries the
// newest chunk only, so a second append that sealed a chunk and started another
// would leave the sealed one's final record unpersisted while the header already
// counted it -- next round the window reads short and is discarded as corrupt.
// And the round's write budget is sized on one header and one chunk per pair.
// The aggregation path appends once per pair per round, so this only rejects
// misuse, but it is checked rather than assumed because Append is exported.
func (w *RingWindow) Append(observedAtNanoseconds uint64, value StreamValue) (appended bool, err error) {
	if value == nil {
		return false, ErrNilStreamValue
	}
	if w.stored {
		return false, ErrHistoryAlreadyAppended
	}
	if w.header.requiredCount == 0 {
		return false, nil
	}
	if len(w.header.counts) > 0 && observedAtNanoseconds <= w.header.last {
		return false, nil
	}
	size, err := historyRecordSize(observedAtNanoseconds, value)
	if err != nil {
		return false, err
	}
	if size > MaxHistoryRecordBytes {
		return false, fmt.Errorf("%w: %d bytes exceeds the maximum of %d", ErrHistoryRecordTooLarge, size, MaxHistoryRecordBytes)
	}

	last := len(w.header.counts) - 1
	var tail *StreamHistoryChunk
	if last >= 0 && w.header.counts[last] < MaxHistoryChunkRecords {
		sequence := w.header.firstSequence + uint64(last)
		if tail = w.chunks[sequence]; tail == nil {
			return false, fmt.Errorf("%w: sequence %d", ErrHistoryChunkNotLoaded, sequence)
		}
		w.header.counts[last]++
	} else {
		// The ring is sized so this cannot collide: retention leaves at most
		// MaxHistoryChunkSlots-1 chunks, so the new sequence lands on the one
		// slot none of them occupy, and eviction below frees the oldest again.
		if len(w.header.counts) >= MaxHistoryChunkSlots {
			return false, fmt.Errorf("%w: %d chunks retained, no free ring slot", ErrCorruptStreamHistory, len(w.header.counts))
		}
		sequence := w.header.firstSequence + uint64(len(w.header.counts))
		tail = &StreamHistoryChunk{sequence: sequence}
		w.chunks[sequence] = tail
		w.header.counts = append(w.header.counts, 1)
		w.header.chunkFirst = append(w.header.chunkFirst, observedAtNanoseconds)
	}

	tail.records = append(tail.records, StreamHistoryRecord{
		ObservedAtNanoseconds: observedAtNanoseconds,
		Value:                 value,
	})
	w.header.last = observedAtNanoseconds
	w.headerDirty = true
	w.dirtyChunk = tail
	w.stored = true
	w.evict()
	return true, nil
}

// evict drops whole chunks from the oldest end for as long as the window would
// still cover the required depth without them.
//
// Eviction is deletes only, never a rewrite, and it needs no chunk loaded: the
// header carries each chunk's record count and start timestamp precisely so
// that dropping one does not mean reading its successor to learn where the
// window now begins.
func (w *RingWindow) evict() {
	for len(w.header.counts) > 0 {
		if w.header.total()-w.header.counts[0] < w.header.requiredCount {
			return
		}
		sequence := w.header.firstSequence
		delete(w.chunks, sequence)
		w.deleted = append(w.deleted, HistoryChunkSlot(sequence))
		w.header.counts = w.header.counts[1:]
		w.header.chunkFirst = w.header.chunkFirst[1:]
		w.header.firstSequence++
		w.headerDirty = true
	}
	// Only reachable at teardown (requiredCount zero); an empty window is
	// canonically zero-valued so two oracles that arrive here by different
	// routes write identical bytes.
	w.header.firstSequence = 0
	w.header.last = 0
	w.header.counts = nil
	w.header.chunkFirst = nil
}

// WriteSet returns what this round's mutations mean for storage: at most one
// header, at most one chunk — the newest, the only one a round ever rewrites —
// and the slots of any chunks evicted.
func (w *RingWindow) WriteSet() RingWriteSet {
	set := RingWriteSet{Chunk: w.dirtyChunk}
	if w.headerDirty {
		set.Header = &w.header
	}
	if len(w.deleted) == 0 {
		return set
	}

	// A slot is never both written and deleted, but say so structurally rather
	// than relying on the ring arithmetic staying correct forever.
	var written uint32
	hasWritten := false
	if w.dirtyChunk != nil {
		written, hasWritten = w.dirtyChunk.Slot(), true
	}
	slots := make([]uint32, 0, len(w.deleted))
	seen := make(map[uint32]bool, len(w.deleted))
	for _, slot := range w.deleted {
		if seen[slot] || (hasWritten && slot == written) {
			continue
		}
		seen[slot] = true
		slots = append(slots, slot)
	}
	slices.Sort(slots)
	set.DeletedSlots = slots
	return set
}
