package llo

import (
	"errors"
	"fmt"
	"sort"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
)

// historyStore satisfies the read side of the shared expression engine.
var _ calculated.HistoryReader = (*historyStore)(nil)

// historyStore is the per-round working set of stream history windows. It is
// created fresh in each StateTransition and discarded at the end of it; nothing
// carries across rounds except what Flush persists.
//
// It is the only thing in the plugin that touches history keys, and it holds
// two guarantees the design depends on:
//
//   - each key is read at most once and written at most once per round, however
//     many channels or expressions reference the pair. Ten channels sharing
//     History(s101, 300) cause one set of reads and one set of writes.
//   - a round reads only what it needs. Windows are chunked (see
//     protocol.RingWindow), so appending touches the newest chunk, reading n
//     records touches the chunks covering them, and a pair that is still warming
//     up costs a single header read — the shortfall is decided from the header
//     without opening a chunk at all.
type historyStore struct {
	r    ocr3_1types.KeyValueStateReader
	lggr logger.Logger

	// windows memoizes the working copy of each pair touched this round,
	// including negative results: a pair present here has already been read.
	windows map[histKey]*protocol.RingWindow
	// required is the depth admitted for each pair this round. Kept separately
	// from the windows so that discarding a corrupt one does not lose it.
	required map[histKey]uint32
	// index is the persisted set of pairs that have a stored window, used to
	// find orphans without a range scan.
	index map[histKey]bool
	// corrupt records pairs whose stored window failed to decode. They are
	// re-warmed from empty; this is kept for telemetry and to force the bad
	// bytes to be overwritten.
	corrupt map[histKey]bool
	// oversized counts agreed values dropped for exceeding the per-record size
	// cap, each of which leaves a gap in a series. Telemetry only.
	oversized int
	// written records the bytes each pair's keys took this round, which is what
	// the per-round byte budget is actually spent on. Telemetry only.
	written map[histKey]int
	// chunkWrites counts chunk values written this round. Telemetry only.
	chunkWrites int

	// layoutReset is set when the stored layout version is not the current one.
	// The stored windows are abandoned rather than read, and Flush records the
	// current version even if there was nothing else to write.
	layoutReset bool
	// abandoned is how many indexed pairs the layout reset threw away. Non-zero
	// means the stored index is stale and must be rewritten even if this round
	// warms nothing.
	abandoned int
}

// newHistoryStore loads the history index and returns a store for this round.
//
// If the stored layout version is not the current one, the index is treated as
// empty and every window re-warms from scratch: v31 is under llo/dev, so a
// layout change is a reset and a re-warm, not a dual-read shim.
func newHistoryStore(r ocr3_1types.KeyValueStateReader, lggr logger.Logger) (*historyStore, error) {
	keys, err := readHistoryIndex(r)
	if err != nil {
		return nil, fmt.Errorf("read history index: %w", err)
	}
	version, err := readHistoryLayoutVersion(r)
	if err != nil {
		return nil, fmt.Errorf("read history layout version: %w", err)
	}

	s := &historyStore{
		r:           r,
		lggr:        lggr,
		windows:     make(map[histKey]*protocol.RingWindow, len(keys)),
		required:    map[histKey]uint32{},
		index:       make(map[histKey]bool, len(keys)),
		corrupt:     map[histKey]bool{},
		written:     map[histKey]int{},
		layoutReset: version != historyLayoutVersion,
	}

	if s.layoutReset {
		if len(keys) > 0 {
			lggr.Infow("Stream history layout changed; dropping stored windows and re-warming",
				"storedVersion", version, "version", historyLayoutVersion, "pairs", len(keys))
		}
		s.abandoned = len(keys)
		return s, nil
	}
	for _, k := range keys {
		s.index[k] = true
	}
	return s, nil
}

// window returns the working copy for a pair, reading its header on first use.
//
// A header that fails to decode is replaced with a reset window, which deletes
// the pair's whole slot space on flush. Corruption must not fail the round:
// libocr requires malformed stored entries to be handled gracefully, and the
// cost of discarding is a warmup, not a halt.
func (s *historyStore) window(k histKey) (*protocol.RingWindow, error) {
	if w, ok := s.windows[k]; ok {
		return w, nil
	}

	header, err := readHistoryHeader(s.r, k.streamID, k.aggregator)
	if err != nil {
		if !errors.Is(err, protocol.ErrCorruptStreamHistory) {
			// A genuine read failure, not bad data. Failing the round is right:
			// every oracle will retry, and continuing would silently drop
			// history that is actually there.
			return nil, err
		}
		return s.reset(k, err), nil
	}

	w := protocol.NewRingWindow(header)
	s.windows[k] = w
	return w, nil
}

// reset discards a pair's stored window and returns an empty one whose write set
// deletes every slot of the ring.
//
// Blind deletion of the whole slot space is why the layout uses a fixed ring:
// the header is the only thing that says which chunks exist, so when it cannot
// be trusted there is nothing else to consult. The admitted depth is reapplied
// so the pair starts warming again immediately.
func (s *historyStore) reset(k histKey, cause error) *protocol.RingWindow {
	s.lggr.Errorw("Discarding corrupt stream history; re-warming from empty",
		"err", cause, "streamID", k.streamID, "aggregator", k.aggregator)
	s.corrupt[k] = true

	w := protocol.ResetRingWindow()
	if n := s.required[k]; n > 0 {
		if _, err := w.SetRequiredCount(n); err != nil {
			// Only possible above the cap, which admission already rejects.
			s.lggr.Errorw("Failed to reapply history requirement after reset",
				"err", err, "streamID", k.streamID, "aggregator", k.aggregator, "requiredCount", n)
		}
	}
	s.windows[k] = w
	return w
}

// provide reads the named chunks into the window. A chunk that is missing or
// does not match the header means the window as a whole cannot be trusted, so it
// is discarded and re-warmed; the returned window is then the empty replacement.
func (s *historyStore) provide(k histKey, w *protocol.RingWindow, sequences []uint64) (*protocol.RingWindow, error) {
	for _, sequence := range sequences {
		if w.Loaded(sequence) {
			// Already in hand, and possibly appended to since: the stored bytes
			// are stale until this round's write set is flushed, so re-reading
			// them would look like a header/chunk mismatch. This is also what
			// makes the one-read-per-key-per-round guarantee hold when a pair is
			// both appended to and read in the same round.
			continue
		}
		slot := protocol.HistoryChunkSlot(sequence)
		chunk, err := readHistoryChunk(s.r, k.streamID, k.aggregator, slot)
		switch {
		case err != nil && !errors.Is(err, protocol.ErrCorruptStreamHistory):
			return nil, err
		case err != nil:
			return s.reset(k, err), nil
		case chunk == nil:
			return s.reset(k, fmt.Errorf("%w: chunk %d (slot %d) is missing",
				protocol.ErrCorruptStreamHistory, sequence, slot)), nil
		}
		if err := w.Provide(chunk); err != nil {
			return s.reset(k, err), nil
		}
	}
	return w, nil
}

// Load returns the working copy for a pair, reading no chunks. The window is
// empty, never nil, when nothing is stored.
//
// It is enough for anything that only asks how deep a window is; reading records
// out of it additionally needs the chunks covering them (see Series).
func (s *historyStore) Load(sid llotypes.StreamID, agg llotypes.Aggregator) (*protocol.RingWindow, error) {
	return s.window(histKey{streamID: sid, aggregator: agg})
}

// Series implements calculated.HistoryReader, projecting the newest count
// records onto one field.
//
// Chunk memoization is what satisfies the interface's one-read-per-pair
// requirement: however many expressions ask for this pair, at whatever depths
// and fields, each underlying key is read once. Depths that overlap share the
// chunks they have in common, and an unsatisfiable depth reads nothing.
func (s *historyStore) Series(sid llotypes.StreamID, agg llotypes.Aggregator, count uint32, field calculated.Field) (calculated.Series, error) {
	k := histKey{streamID: sid, aggregator: agg}
	w, err := s.window(k)
	if err != nil {
		return calculated.Series{}, err
	}

	sequences, err := w.ReadPlan(count)
	if err != nil {
		// Not deep enough yet: warming up, or a depth increase, or state that
		// was discarded as corrupt. Decided from the header alone, so this costs
		// no chunk reads. The caller must not evaluate.
		return calculated.Series{}, err
	}

	if w, err = s.provide(k, w, sequences); err != nil {
		return calculated.Series{}, err
	}
	records, err := w.Newest(count)
	if err != nil {
		if errors.Is(err, protocol.ErrCorruptStreamHistory) {
			s.reset(k, err)
		}
		return calculated.Series{}, err
	}
	return calculated.SeriesFromRecords(records, field)
}

// SetRequired sets a pair's window capacity for this round. A capacity of zero
// means no channel needs the pair any more, and Flush deletes it.
//
// Capacity changes never rewrite a chunk: growing touches only the header, and
// shrinking evicts whole chunks, which is deletes.
func (s *historyStore) SetRequired(sid llotypes.StreamID, agg llotypes.Aggregator, n uint32) error {
	k := histKey{streamID: sid, aggregator: agg}
	s.required[k] = n

	w, err := s.window(k)
	if err != nil {
		return err
	}
	if _, err := w.SetRequiredCount(n); err != nil {
		return fmt.Errorf("set required count for stream %d aggregator %d: %w", sid, agg, err)
	}
	return nil
}

// Append records this round's agreed value for a pair, reporting whether it was
// stored. See protocol.RingWindow.Append for the strictly-newer rule; a rejected
// append is normal and not an error.
func (s *historyStore) Append(sid llotypes.StreamID, agg llotypes.Aggregator, observedAtNanoseconds uint64, sv protocol.StreamValue) (bool, error) {
	k := histKey{streamID: sid, aggregator: agg}
	w, err := s.window(k)
	if err != nil {
		return false, err
	}
	// Appending needs the newest chunk when it still has room; a sealed or
	// absent one starts a fresh chunk and reads nothing.
	if w, err = s.provide(k, w, w.AppendPlan()); err != nil {
		return false, err
	}

	appended, err := w.Append(observedAtNanoseconds, sv)
	if err != nil {
		return false, fmt.Errorf("append history for stream %d aggregator %d: %w", sid, agg, err)
	}
	return appended, nil
}

// Flush persists modified windows, deletes pairs no live channel requires, and
// rewrites the index if the stored set changed.
//
// Pairs are visited in sorted order and each pair's writes precede its deletes,
// so every oracle issues the same operations in the same sequence. Per pair a
// round writes at most one header and one chunk — the newest, the only one that
// is ever rewritten — plus a delete for each chunk that fell out.
func (s *historyStore) Flush(w ocr3_1types.KeyValueStateReadWriter) error {
	// A layout reset abandons whatever the previous layout stored: if the stale
	// index listed anything, it must be rewritten from the windows this round
	// actually warmed.
	indexChanged := s.abandoned > 0

	for _, k := range s.sortedWindowKeys() {
		win := s.windows[k]
		if win.RequiredCount() == 0 {
			continue // handled by the orphan pass below
		}
		set := win.WriteSet()
		if set.Empty() {
			continue
		}

		if set.Header != nil {
			n, err := writeHistoryHeader(w, k.streamID, k.aggregator, set.Header)
			if err != nil {
				return err
			}
			s.written[k] += n
		}
		if set.Chunk != nil {
			n, err := writeHistoryChunk(w, k.streamID, k.aggregator, set.Chunk)
			if err != nil {
				return err
			}
			s.written[k] += n
			s.chunkWrites++
		}
		for _, slot := range set.DeletedSlots {
			if err := deleteHistoryChunk(w, k.streamID, k.aggregator, slot); err != nil {
				return fmt.Errorf("delete history chunk for stream %d aggregator %d slot %d: %w", k.streamID, k.aggregator, slot, err)
			}
		}

		if !s.index[k] {
			s.index[k] = true
			indexChanged = true
		}
	}

	// A stored pair with no capacity is dead state: either its channels were
	// removed, or their expressions no longer reference it.
	for _, k := range s.orphans() {
		if err := s.deleteWindow(w, k); err != nil {
			return err
		}
		delete(s.index, k)
		indexChanged = true
	}

	if indexChanged {
		if err := writeHistoryIndex(w, s.indexKeys()); err != nil {
			return fmt.Errorf("write history index: %w", err)
		}
	}

	// Record the layout only once this round has actually stored something in
	// it. A DON that has never used history writes no history keys at all, and
	// stamping a version onto an otherwise untouched state would be the only
	// exception to that.
	if s.layoutReset && (indexChanged || len(s.index) > 0) {
		if err := writeHistoryLayoutVersion(w); err != nil {
			return fmt.Errorf("write history layout version: %w", err)
		}
		s.layoutReset = false
	}
	return nil
}

// deleteWindow removes every key belonging to a pair.
//
// The chunks to delete come from the window's write set, which after a teardown
// or a reset lists exactly the slots it held. A pair that was never loaded this
// round has no such list, so its whole slot space is deleted — bounded, and the
// same recovery a corrupt header gets.
func (s *historyStore) deleteWindow(w ocr3_1types.KeyValueStateReadWriter, k histKey) error {
	if err := deleteHistoryHeader(w, k.streamID, k.aggregator); err != nil {
		return fmt.Errorf("delete history header for stream %d aggregator %d: %w", k.streamID, k.aggregator, err)
	}

	slots := allHistoryChunkSlots()
	if win, ok := s.windows[k]; ok {
		slots = win.WriteSet().DeletedSlots
	}
	for _, slot := range slots {
		if err := deleteHistoryChunk(w, k.streamID, k.aggregator, slot); err != nil {
			return fmt.Errorf("delete history chunk for stream %d aggregator %d slot %d: %w", k.streamID, k.aggregator, slot, err)
		}
	}
	return nil
}

func allHistoryChunkSlots() []uint32 {
	slots := make([]uint32, 0, protocol.MaxHistoryChunkSlots)
	for slot := range uint32(protocol.MaxHistoryChunkSlots) {
		slots = append(slots, slot)
	}
	return slots
}

// sortedWindowKeys returns the pairs touched this round, in sorted order.
func (s *historyStore) sortedWindowKeys() []histKey {
	keys := make([]histKey, 0, len(s.windows))
	for k := range s.windows {
		keys = append(keys, k)
	}
	sortHistKeys(keys)
	return keys
}

// indexKeys returns the stored pairs in sorted order.
func (s *historyStore) indexKeys() []histKey {
	keys := make([]histKey, 0, len(s.index))
	for k := range s.index {
		keys = append(keys, k)
	}
	sortHistKeys(keys)
	return keys
}

// orphans returns stored pairs that no live channel requires, in sorted order.
// A pair is an orphan when it has a stored window but was never given a
// non-zero capacity this round.
func (s *historyStore) orphans() []histKey {
	orphans := make([]histKey, 0)
	for k := range s.index {
		if w, ok := s.windows[k]; ok && w.RequiredCount() > 0 {
			continue
		}
		orphans = append(orphans, k)
	}
	sortHistKeys(orphans)
	return orphans
}

func sortHistKeys(keys []histKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].streamID != keys[j].streamID {
			return keys[i].streamID < keys[j].streamID
		}
		return keys[i].aggregator < keys[j].aggregator
	})
}
