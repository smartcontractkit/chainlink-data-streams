package llo

import (
	"strconv"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
)

// countingKV counts accesses per key so tests can assert the one-read,
// one-write-per-pair-per-round guarantee that makes history affordable inside
// the OCR3.1 round budget.
type countingKV struct {
	*memKV
	reads   map[string]int
	writes  map[string]int
	deletes map[string]int
}

func newCountingKV() *countingKV {
	return &countingKV{
		memKV:   newMemKV(),
		reads:   map[string]int{},
		writes:  map[string]int{},
		deletes: map[string]int{},
	}
}

func (k *countingKV) Read(key []byte) ([]byte, error) {
	k.reads[string(key)]++
	return k.memKV.Read(key)
}

func (k *countingKV) Write(key, value []byte) error {
	k.writes[string(key)]++
	return k.memKV.Write(key, value)
}

func (k *countingKV) Delete(key []byte) error {
	k.deletes[string(key)]++
	return k.memKV.Delete(key)
}

var _ ocr3_1types.KeyValueStateReadWriter = &countingKV{}

const (
	testAggMedian = llotypes.AggregatorMedian
	testAggMode   = llotypes.AggregatorMode
)

func newTestHistoryStore(t *testing.T, kv ocr3_1types.KeyValueStateReader) *historyStore {
	t.Helper()
	s, err := newHistoryStore(kv, logger.Test(t))
	require.NoError(t, err)
	return s
}

func testDecimal(i int64) protocol.StreamValue {
	return protocol.ToDecimal(decimal.NewFromInt(i))
}

// readHistory reassembles a stored window from its header and chunks, or
// returns nil when the pair holds no header.
//
// Tests assert against what is actually in the key-value state rather than
// against a store's in-memory copy, so they need to walk the same keys the store
// does. Unlike the store it loads every retained chunk, which is what makes it
// a check on the stored form rather than on the read plan.
func readHistory(t *testing.T, r ocr3_1types.KeyValueStateReader, sid llotypes.StreamID, agg llotypes.Aggregator) *protocol.RingWindow {
	t.Helper()

	header, err := readHistoryHeader(r, sid, agg)
	require.NoError(t, err)
	if header == nil {
		return nil
	}

	w := protocol.NewRingWindow(header)
	for _, sequence := range header.Sequences() {
		chunk, err := readHistoryChunk(r, sid, agg, protocol.HistoryChunkSlot(sequence))
		require.NoError(t, err)
		require.NotNil(t, chunk, "header retains chunk %d but the slot is empty", sequence)
		require.NoError(t, w.Provide(chunk))
	}
	return w
}

// requireHistoryRetention asserts the retention rule: a warm window holds at
// least the required depth and overshoots it by less than one chunk, because
// chunks are evicted whole.
func requireHistoryRetention(t *testing.T, w *protocol.RingWindow) {
	t.Helper()

	require.NotNil(t, w)
	assert.GreaterOrEqual(t, w.Len(), int(w.RequiredCount()), "a window must cover the depth it was asked for")
	assert.Less(t, w.Len(), int(w.RequiredCount())+protocol.MaxHistoryChunkRecords,
		"retention keeps as few whole chunks as cover the requirement")
}

// readHistoryNewest returns the newest n records of a stored window.
func readHistoryNewest(t *testing.T, r ocr3_1types.KeyValueStateReader, sid llotypes.StreamID, agg llotypes.Aggregator, n uint32) []protocol.StreamHistoryRecord {
	t.Helper()

	records, err := readHistory(t, r, sid, agg).Newest(n)
	require.NoError(t, err)
	return records
}

// historyTimestamps extracts the observation timestamps of a run of records.
func historyTimestamps(records []protocol.StreamHistoryRecord) []uint64 {
	timestamps := make([]uint64, 0, len(records))
	for _, record := range records {
		timestamps = append(timestamps, record.ObservedAtNanoseconds)
	}
	return timestamps
}

// readHistoryRecords returns every record of a stored window, oldest first.
func readHistoryRecords(t *testing.T, r ocr3_1types.KeyValueStateReader, sid llotypes.StreamID, agg llotypes.Aggregator) []protocol.StreamHistoryRecord {
	t.Helper()

	w := readHistory(t, r, sid, agg)
	if w == nil {
		return nil
	}
	records, err := w.Newest(uint32(w.Len()))
	require.NoError(t, err)
	return records
}

// historyKeys returns every key a pair may occupy: the header and the whole
// ring slot space.
func historyKeys(sid llotypes.StreamID, agg llotypes.Aggregator) [][]byte {
	keys := [][]byte{historyHeaderKey(sid, agg)}
	for slot := range uint32(protocol.MaxHistoryChunkSlots) {
		keys = append(keys, historyChunkKey(sid, agg, slot))
	}
	return keys
}

// historyKeyWrites totals the writes across every key belonging to a pair.
func historyKeyWrites(kv *countingKV, sid llotypes.StreamID, agg llotypes.Aggregator) int {
	writes := 0
	for _, key := range historyKeys(sid, agg) {
		writes += kv.writes[string(key)]
	}
	return writes
}

// historyKeyReads totals the reads across every key belonging to a pair.
func historyKeyReads(kv *countingKV, sid llotypes.StreamID, agg llotypes.Aggregator) int {
	reads := 0
	for _, key := range historyKeys(sid, agg) {
		reads += kv.reads[string(key)]
	}
	return reads
}

// historyStoredBytes totals the bytes a pair occupies across all of its keys.
func historyStoredBytes(t *testing.T, kv *countingKV, sid llotypes.StreamID, agg llotypes.Aggregator) int {
	t.Helper()

	bytes := 0
	for _, key := range historyKeys(sid, agg) {
		b, err := kv.Read(key)
		require.NoError(t, err)
		bytes += len(b)
	}
	return bytes
}

func TestHistoryStore_EmptyState(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	s := newTestHistoryStore(t, kv)

	h, err := s.Load(1, testAggMedian)
	require.NoError(t, err)
	require.NotNil(t, h, "an unstored pair must load as an empty window, not nil")
	assert.Equal(t, 0, h.Len())
	assert.Zero(t, h.RequiredCount())

	// Nothing was required, so nothing is persisted.
	require.NoError(t, s.Flush(kv))
	assert.Empty(t, kv.writes)
}

func TestHistoryStore_AppendAndFlush(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	s := newTestHistoryStore(t, kv)

	require.NoError(t, s.SetRequired(1, testAggMedian, 3))
	appended, err := s.Append(1, testAggMedian, 1_000, testDecimal(10))
	require.NoError(t, err)
	assert.True(t, appended)
	require.NoError(t, s.Flush(kv))

	// The window and the index are both persisted.
	stored := readHistory(t, kv, 1, testAggMedian)
	require.NotNil(t, stored)
	assert.Equal(t, 1, stored.Len())
	assert.Equal(t, uint32(3), stored.RequiredCount())

	idx, err := readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Equal(t, []histKey{{streamID: 1, aggregator: testAggMedian}}, idx)
}

// TestHistoryStore_ReadOnceWriteOnce is the core cost guarantee: however many
// channels or expressions touch a pair in a round, each of its keys is read
// once and written once.
func TestHistoryStore_ReadOnceWriteOnce(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	s := newTestHistoryStore(t, kv)

	// Ten channels all declaring History(s1, 300), then reading it back.
	for range 10 {
		require.NoError(t, s.SetRequired(1, testAggMedian, 300))
		_, err := s.Load(1, testAggMedian)
		require.NoError(t, err)
	}
	_, err := s.Append(1, testAggMedian, 1_000, testDecimal(1))
	require.NoError(t, err)
	for range 10 {
		_, err := s.Load(1, testAggMedian)
		require.NoError(t, err)
	}
	require.NoError(t, s.Flush(kv))

	// Nothing is stored yet, so the round reads the header and no chunk: an
	// empty window has none to append into.
	assert.Equal(t, 1, historyKeyReads(kv, 1, testAggMedian), "each history key must be read at most once per round")
	assert.Equal(t, 1, kv.reads[string(historyHeaderKey(1, testAggMedian))])
	assert.Equal(t, 2, historyKeyWrites(kv, 1, testAggMedian), "a round writes the header and the newest chunk, nothing else")
	assert.Equal(t, 1, kv.writes[string(keyHistoryIndex)], "index must be written at most once per round")
}

// TestHistoryStore_ReadsOnlyWhatItNeeds is the property the chunked layout buys:
// the number of chunks a round reads follows the depth it actually asks for, not
// the depth of the window.
func TestHistoryStore_ReadsOnlyWhatItNeeds(t *testing.T) {
	t.Parallel()

	const depth = protocol.MaxHistoryChunkRecords * 4
	kv := newCountingKV()
	for round := 1; round <= depth; round++ {
		s := newTestHistoryStore(t, kv)
		require.NoError(t, s.SetRequired(1, testAggMedian, depth))
		_, err := s.Append(1, testAggMedian, uint64(round)*1_000, testDecimal(int64(round)))
		require.NoError(t, err)
		require.NoError(t, s.Flush(kv))
	}

	for _, tc := range []struct {
		count uint32
		reads int
	}{
		{count: 1, reads: 2}, // header + newest chunk
		{count: protocol.MaxHistoryChunkRecords, reads: 2},     //
		{count: protocol.MaxHistoryChunkRecords + 1, reads: 3}, //
		{count: depth, reads: 5},                               // header + all four chunks
		{count: depth + 1, reads: 1},                           // unsatisfiable: header only
	} {
		t.Run(strconv.Itoa(int(tc.count)), func(t *testing.T) {
			kv := &countingKV{memKV: kv.memKV, reads: map[string]int{}, writes: map[string]int{}, deletes: map[string]int{}}
			s := newTestHistoryStore(t, kv)

			_, err := s.Series(1, testAggMedian, tc.count, calculated.FieldValue)
			if tc.count > depth {
				require.ErrorIs(t, err, protocol.ErrInsufficientStreamHistory)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.reads, historyKeyReads(kv, 1, testAggMedian))
		})
	}
}

// TestHistoryStore_UnchangedWindowNotWritten keeps modifiedKeys low: a round
// that neither appends nor changes capacity must not touch the key.
func TestHistoryStore_UnchangedWindowNotWritten(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()

	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 5))
	_, err := s.Append(1, testAggMedian, 1_000, testDecimal(1))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	writesAfterFirstRound := historyKeyWrites(kv, 1, testAggMedian)
	indexWritesAfterFirstRound := kv.writes[string(keyHistoryIndex)]

	// Next round: same requirement, and a stale observation timestamp that
	// must not be re-appended.
	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 5))
	appended, err := s.Append(1, testAggMedian, 1_000, testDecimal(2))
	require.NoError(t, err)
	assert.False(t, appended, "a non-advancing timestamp must not be appended")
	require.NoError(t, s.Flush(kv))

	assert.Equal(t, writesAfterFirstRound, historyKeyWrites(kv, 1, testAggMedian), "unchanged window must not be rewritten")
	assert.Equal(t, indexWritesAfterFirstRound, kv.writes[string(keyHistoryIndex)], "unchanged index must not be rewritten")
}

func TestHistoryStore_MultiRoundAccumulation(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()

	for round := 1; round <= 6; round++ {
		s := newTestHistoryStore(t, kv)
		require.NoError(t, s.SetRequired(1, testAggMedian, 3))
		appended, err := s.Append(1, testAggMedian, uint64(round)*1_000, testDecimal(int64(round)))
		require.NoError(t, err)
		assert.True(t, appended)
		require.NoError(t, s.Flush(kv))
	}

	// Records accumulate across rounds and the newest are the ones an expression
	// reads. Retention works in whole chunks, so with a capacity far below the
	// chunk size nothing has been evicted yet — what is bounded is what is read.
	stored := readHistory(t, kv, 1, testAggMedian)
	requireHistoryRetention(t, stored)
	assert.Equal(t, uint64(6_000), stored.LastObservationTimestampNanoseconds())
	assert.Equal(t, []uint64{4_000, 5_000, 6_000},
		historyTimestamps(readHistoryNewest(t, kv, 1, testAggMedian, 3)))
}

func TestHistoryStore_RequirementChanges(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()

	// Warm five records at capacity 5.
	for round := 1; round <= 5; round++ {
		s := newTestHistoryStore(t, kv)
		require.NoError(t, s.SetRequired(1, testAggMedian, 5))
		_, err := s.Append(1, testAggMedian, uint64(round)*1_000, testDecimal(int64(round)))
		require.NoError(t, err)
		require.NoError(t, s.Flush(kv))
	}

	// Lowering the requirement trims immediately.
	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 2))
	require.NoError(t, s.Flush(kv))

	stored := readHistory(t, kv, 1, testAggMedian)
	assert.Equal(t, uint32(2), stored.RequiredCount())
	requireHistoryRetention(t, stored)
	assert.Equal(t, []uint64{4_000, 5_000}, historyTimestamps(readHistoryNewest(t, kv, 1, testAggMedian, 2)))

	// Raising it keeps what is there; the extra depth fills over later rounds.
	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 10))
	require.NoError(t, s.Flush(kv))

	stored = readHistory(t, kv, 1, testAggMedian)
	assert.Equal(t, uint32(10), stored.RequiredCount())
	_, err := stored.Newest(10)
	assert.ErrorIs(t, err, protocol.ErrInsufficientStreamHistory, "the extra depth is not yet available")
}

// warmHistory runs n rounds appending to a pair, leaving the store flushed.
func warmHistory(t *testing.T, kv ocr3_1types.KeyValueStateReadWriter, k histKey, required uint32, rounds int) {
	t.Helper()
	for round := 1; round <= rounds; round++ {
		s := newTestHistoryStore(t, kv)
		require.NoError(t, s.SetRequired(k.streamID, k.aggregator, required))
		_, err := s.Append(k.streamID, k.aggregator, uint64(round)*1_000, testDecimal(int64(round)))
		require.NoError(t, err)
		require.NoError(t, s.Flush(kv))
	}
}

// TestHistoryStore_ShrinkReclaimsState covers a config change that lowers the
// requested depth: the deeper window must be trimmed to the new depth in the
// round the requirement drops, not carried until it happens to evict.
func TestHistoryStore_ShrinkReclaimsState(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	key := histKey{streamID: 1, aggregator: testAggMedian}
	warmHistory(t, kv, key, 300, 300)

	deep := readHistory(t, kv, key.streamID, key.aggregator)
	requireHistoryRetention(t, deep)
	require.Equal(t, 300, deep.Len())
	deepBytes := historyStoredBytes(t, kv, key.streamID, key.aggregator)

	// A channel edit lowers the deepest requirement for this pair to 10.
	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(key.streamID, key.aggregator, 10))
	require.NoError(t, s.Flush(kv))

	shallow := readHistory(t, kv, key.streamID, key.aggregator)
	assert.Equal(t, uint32(10), shallow.RequiredCount())
	requireHistoryRetention(t, shallow)
	assert.Less(t, shallow.Len(), deep.Len(), "the window must be trimmed in the round the requirement drops")
	// The newest records are the ones kept.
	assert.Equal(t, uint64(300_000), shallow.LastObservationTimestampNanoseconds())
	assert.Equal(t, uint64(291_000), readHistoryNewest(t, kv, key.streamID, key.aggregator, 10)[0].ObservedAtNanoseconds)

	shallowBytes := historyStoredBytes(t, kv, key.streamID, key.aggregator)
	assert.Less(t, shallowBytes, deepBytes, "trimming must actually reclaim stored bytes")

	// The lower capacity persists: later rounds must not regrow past it.
	warmHistory(t, kv, key, 10, 5)
	stored := readHistory(t, kv, key.streamID, key.aggregator)
	assert.Equal(t, uint32(10), stored.RequiredCount())
	requireHistoryRetention(t, stored)
}

// TestHistoryStore_ShrinkBelowStoredButAboveNothing checks the shrink case where
// the stored window is already shorter than the new capacity: nothing is
// trimmed, but the lower capacity is still persisted.
func TestHistoryStore_ShrinkWithoutTrim(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	key := histKey{streamID: 1, aggregator: testAggMedian}
	warmHistory(t, kv, key, 300, 3)

	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(key.streamID, key.aggregator, 10))
	require.NoError(t, s.Flush(kv))

	stored := readHistory(t, kv, key.streamID, key.aggregator)
	assert.Equal(t, 3, stored.Len())
	assert.Equal(t, uint32(10), stored.RequiredCount())
}

// TestHistoryStore_UnreferencedStreamFullyRemoved covers a stream that no
// expression references any more: its window and its index entry must both go,
// while its siblings are left untouched.
func TestHistoryStore_UnreferencedStreamFullyRemoved(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	kept := histKey{streamID: 1, aggregator: testAggMedian}
	dropped := histKey{streamID: 2, aggregator: testAggMedian}
	alsoKept := histKey{streamID: 3, aggregator: testAggMode}

	s := newTestHistoryStore(t, kv)
	for _, k := range []histKey{kept, dropped, alsoKept} {
		require.NoError(t, s.SetRequired(k.streamID, k.aggregator, 5))
		_, err := s.Append(k.streamID, k.aggregator, 1_000, testDecimal(1))
		require.NoError(t, err)
	}
	require.NoError(t, s.Flush(kv))

	keptBytesBefore := historyStoredBytes(t, kv, kept.streamID, kept.aggregator)
	keptWritesBefore := historyKeyWrites(kv, kept.streamID, kept.aggregator)

	// Stream 2 is no longer referenced: it is simply absent from this round's
	// requirements, which is what channel removal looks like.
	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(kept.streamID, kept.aggregator, 5))
	require.NoError(t, s.SetRequired(alsoKept.streamID, alsoKept.aggregator, 5))
	require.NoError(t, s.Flush(kv))

	gone := readHistory(t, kv, dropped.streamID, dropped.aggregator)
	assert.Nil(t, gone, "unreferenced stream must have its window deleted")
	assert.Equal(t, 1, kv.deletes[string(historyHeaderKey(dropped.streamID, dropped.aggregator))],
		"the header is what makes a window findable, so it must be deleted exactly once")

	idx, err := readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Equal(t, []histKey{kept, alsoKept}, idx, "index must be pruned to the surviving pairs")

	// Siblings are untouched: no rewrite, no data change.
	assert.Equal(t, keptBytesBefore, historyStoredBytes(t, kv, kept.streamID, kept.aggregator))
	assert.Equal(t, keptWritesBefore, historyKeyWrites(kv, kept.streamID, kept.aggregator),
		"removing one pair must not rewrite the others")
}

// TestHistoryStore_RemovingAllStreamsEmptiesIndex checks the state fully drains
// when every calculated channel goes away.
func TestHistoryStore_RemovingAllStreamsEmptiesIndex(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	pairs := []histKey{
		{streamID: 1, aggregator: testAggMedian},
		{streamID: 2, aggregator: testAggMedian},
		{streamID: 2, aggregator: testAggMode},
	}

	s := newTestHistoryStore(t, kv)
	for _, k := range pairs {
		require.NoError(t, s.SetRequired(k.streamID, k.aggregator, 5))
		_, err := s.Append(k.streamID, k.aggregator, 1_000, testDecimal(1))
		require.NoError(t, err)
	}
	require.NoError(t, s.Flush(kv))

	// A round with no calculated channels at all.
	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.Flush(kv))

	for _, k := range pairs {
		stored := readHistory(t, kv, k.streamID, k.aggregator)
		assert.Nilf(t, stored, "window for stream %d aggregator %d must be deleted", k.streamID, k.aggregator)
	}
	idx, err := readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Empty(t, idx)

	// The index is rewritten once for the whole cleanup, not once per pair.
	assert.Equal(t, 2, kv.writes[string(keyHistoryIndex)])
}

// TestHistoryStore_IndexEntryWithoutWindow covers index/window divergence in the
// direction the store can repair: an indexed pair whose window is already gone
// must be pruned without failing the round.
func TestHistoryStore_IndexEntryWithoutWindow(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	stale := histKey{streamID: 9, aggregator: testAggMedian}
	require.NoError(t, writeHistoryIndex(kv, []histKey{stale}))

	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.Flush(kv))

	idx, err := readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Empty(t, idx, "stale index entry must be pruned")
}

func TestHistoryStore_AggregatorScoping(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	s := newTestHistoryStore(t, kv)

	// The same stream aggregated two ways must not share a series.
	require.NoError(t, s.SetRequired(1, testAggMedian, 5))
	require.NoError(t, s.SetRequired(1, testAggMode, 5))
	_, err := s.Append(1, testAggMedian, 1_000, testDecimal(10))
	require.NoError(t, err)
	_, err = s.Append(1, testAggMode, 1_000, testDecimal(20))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	medianValue, ok := readHistoryRecords(t, kv, 1, testAggMedian)[0].Value.(*protocol.Decimal)
	require.True(t, ok)
	modeValue, ok := readHistoryRecords(t, kv, 1, testAggMode)[0].Value.(*protocol.Decimal)
	require.True(t, ok)
	assert.Equal(t, "10", medianValue.Decimal().String())
	assert.Equal(t, "20", modeValue.Decimal().String())
}

func TestHistoryStore_OrphanCleanup(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()

	// Two pairs get history.
	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 5))
	require.NoError(t, s.SetRequired(2, testAggMedian, 5))
	_, err := s.Append(1, testAggMedian, 1_000, testDecimal(1))
	require.NoError(t, err)
	_, err = s.Append(2, testAggMedian, 1_000, testDecimal(2))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	idx, err := readHistoryIndex(kv)
	require.NoError(t, err)
	require.Len(t, idx, 2)

	// Next round only stream 1 is required: stream 2's channel was removed, so
	// its window must not be carried forward as dead state.
	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 5))
	_, err = s.Append(1, testAggMedian, 2_000, testDecimal(3))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	orphan := readHistory(t, kv, 2, testAggMedian)
	assert.Nil(t, orphan, "orphaned window must be deleted")

	idx, err = readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Equal(t, []histKey{{streamID: 1, aggregator: testAggMedian}}, idx)
}

// TestHistoryStore_ExplicitZeroRequirementDeletes covers the requirement
// dropping to zero for a pair that is still loaded this round.
func TestHistoryStore_ExplicitZeroRequirementDeletes(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()

	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 5))
	_, err := s.Append(1, testAggMedian, 1_000, testDecimal(1))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 0))
	require.NoError(t, s.Flush(kv))

	stored := readHistory(t, kv, 1, testAggMedian)
	assert.Nil(t, stored)

	idx, err := readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Empty(t, idx)
}

// TestHistoryStore_CorruptWindow checks the fail-soft path: a malformed stored
// window is discarded and re-warmed rather than halting the round.
func TestHistoryStore_CorruptWindow(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	require.NoError(t, kv.Write(historyHeaderKey(1, testAggMedian), []byte("garbage")))
	require.NoError(t, writeHistoryIndex(kv, []histKey{{streamID: 1, aggregator: testAggMedian}}))
	require.NoError(t, writeHistoryLayoutVersion(kv))

	s := newTestHistoryStore(t, kv)
	h, err := s.Load(1, testAggMedian)
	require.NoError(t, err, "corruption must not fail the round")
	assert.Equal(t, 0, h.Len())
	assert.True(t, s.corrupt[histKey{streamID: 1, aggregator: testAggMedian}])

	// The bad bytes are overwritten with a valid empty-then-warming window.
	require.NoError(t, s.SetRequired(1, testAggMedian, 3))
	_, err = s.Append(1, testAggMedian, 1_000, testDecimal(1))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	stored := readHistory(t, kv, 1, testAggMedian)
	require.NotNil(t, stored)
	assert.Equal(t, 1, stored.Len())
}

// TestHistoryStore_CorruptWindowNoLongerRequired makes sure a corrupt window
// belonging to a pair nothing needs is deleted rather than rewritten.
func TestHistoryStore_CorruptWindowNoLongerRequired(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	require.NoError(t, kv.Write(historyHeaderKey(1, testAggMedian), []byte("garbage")))
	require.NoError(t, writeHistoryIndex(kv, []histKey{{streamID: 1, aggregator: testAggMedian}}))
	require.NoError(t, writeHistoryLayoutVersion(kv))

	s := newTestHistoryStore(t, kv)
	_, err := s.Load(1, testAggMedian)
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	stored := readHistory(t, kv, 1, testAggMedian)
	assert.Nil(t, stored)
}

func TestHistoryStore_SetRequiredOverCap(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	s := newTestHistoryStore(t, kv)

	err := s.SetRequired(1, testAggMedian, protocol.MaxHistoryRecordsPerPair+1)
	require.ErrorContains(t, err, "exceeds MaxHistoryRecordsPerPair")
}

func TestHistoryStore_AppendNilValue(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(1, testAggMedian, 3))

	_, err := s.Append(1, testAggMedian, 1_000, nil)
	require.ErrorIs(t, err, protocol.ErrNilStreamValue)
}

// TestHistoryStore_FlushDeterministic checks that two oracles running the same
// round produce byte-identical state, including for pairs offered in different
// iteration orders.
func TestHistoryStore_FlushDeterministic(t *testing.T) {
	t.Parallel()

	pairs := []histKey{
		{streamID: 7, aggregator: testAggMode},
		{streamID: 1, aggregator: testAggMedian},
		{streamID: 7, aggregator: testAggMedian},
		{streamID: 3, aggregator: testAggMedian},
	}

	run := func(order []histKey) map[string][]byte {
		kv := newCountingKV()
		s := newTestHistoryStore(t, kv)
		for i, k := range order {
			require.NoError(t, s.SetRequired(k.streamID, k.aggregator, 4))
			_, err := s.Append(k.streamID, k.aggregator, 1_000, testDecimal(int64(i)))
			require.NoError(t, err)
		}
		require.NoError(t, s.Flush(kv))
		return kv.m
	}

	forward := run(pairs)
	reversed := make([]histKey, len(pairs))
	for i, k := range pairs {
		reversed[len(pairs)-1-i] = k
	}

	// Values differ (they are indexed by offer order), so compare the key set
	// and the index blob, which are what must agree.
	backward := run(reversed)
	require.Len(t, backward, len(forward))
	for k := range forward {
		require.Contains(t, backward, k)
	}
	assert.Equal(t, forward[string(keyHistoryIndex)], backward[string(keyHistoryIndex)])
}

func TestHistoryIndexCodec(t *testing.T) {
	t.Parallel()

	t.Run("round trips sorted", func(t *testing.T) {
		t.Parallel()
		in := []histKey{
			{streamID: 7, aggregator: testAggMode},
			{streamID: 1, aggregator: testAggMedian},
			{streamID: 7, aggregator: testAggMedian},
		}
		got := decodeHistoryIndex(encodeHistoryIndex(in))
		assert.Equal(t, []histKey{
			{streamID: 1, aggregator: testAggMedian},
			{streamID: 7, aggregator: testAggMedian},
			{streamID: 7, aggregator: testAggMode},
		}, got)
	})

	t.Run("encoding is order independent", func(t *testing.T) {
		t.Parallel()
		a := encodeHistoryIndex([]histKey{{streamID: 2}, {streamID: 1}})
		b := encodeHistoryIndex([]histKey{{streamID: 1}, {streamID: 2}})
		assert.Equal(t, a, b)
	})

	t.Run("does not mutate the input", func(t *testing.T) {
		t.Parallel()
		in := []histKey{{streamID: 2}, {streamID: 1}}
		encodeHistoryIndex(in)
		assert.Equal(t, []histKey{{streamID: 2}, {streamID: 1}}, in)
	})

	t.Run("empty and partial input", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, decodeHistoryIndex(nil))
		assert.Empty(t, decodeHistoryIndex([]byte{}))
		// A trailing partial entry is ignored rather than failing the round.
		assert.Empty(t, decodeHistoryIndex([]byte{0, 0, 0, 1}))
		assert.Len(t, decodeHistoryIndex([]byte{0, 0, 0, 1, 0, 0, 0, 2, 0xff}), 1)
	})
}

func TestHistoryKey(t *testing.T) {
	t.Parallel()

	// Keys are prefixed and big-endian so the persisted ordering is
	// deterministic.
	assert.Equal(t, []byte("hh/\x00\x00\x00\x01\x00\x00\x00\x02"), historyHeaderKey(1, 2))
	assert.Equal(t, []byte("hc/\x00\x00\x00\x01\x00\x00\x00\x02\x00\x00\x00\x05"), historyChunkKey(1, 2, 5))

	// History keys share a namespace with the channel and hot records, so they
	// must never collide with them, with each other, or across pairs.
	for _, other := range [][]byte{keyLifecycle, keyChannelState, keyChannelSeqNr, keyHotState, keyHistoryIndex, keyHistoryVersion} {
		assert.NotEqual(t, other, historyHeaderKey(1, 2))
		assert.NotEqual(t, other, historyChunkKey(1, 2, 0))
	}
	assert.NotEqual(t, historyHeaderKey(1, 2), historyHeaderKey(2, 1))
	assert.NotEqual(t, historyChunkKey(1, 2, 0), historyChunkKey(1, 2, 1))
	assert.NotEqual(t, historyChunkKey(1, 2, 0), historyChunkKey(2, 1, 0))
	for slot := range uint32(protocol.MaxHistoryChunkSlots) {
		assert.NotEqual(t, historyHeaderKey(1, 2), historyChunkKey(1, 2, slot))
	}
}

// TestHistoryStore_AppendAndReadInOneRound is a regression test: a pair that is
// both appended to and read in the same round must not re-read the chunk it just
// appended to.
//
// The stored bytes are stale until the flush, so re-reading them would fail the
// header/chunk consistency check and be taken for corruption — discarding a
// perfectly good window every round, silently, for as long as the channel ran.
func TestHistoryStore_AppendAndReadInOneRound(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	for round := 1; round <= 3; round++ {
		s := newTestHistoryStore(t, kv)
		require.NoError(t, s.SetRequired(1, testAggMedian, 3))
		_, err := s.Append(1, testAggMedian, uint64(round)*1_000, testDecimal(int64(round)))
		require.NoError(t, err)

		if round == 3 {
			series, err := s.Series(1, testAggMedian, 3, calculated.FieldValue)
			require.NoError(t, err, "the round that fills the window must be able to read it")
			assert.Equal(t, 3, series.Len())
			assert.Empty(t, s.corrupt, "reading what this round appended must not look like corruption")
		}
		require.NoError(t, s.Flush(kv))
	}

	assert.Equal(t, 3, readHistory(t, kv, 1, testAggMedian).Len())
}

// TestHistoryStore_ResetsOnLayoutChange covers a layout bump: the stored
// windows are abandoned rather than read, because v31 is under llo/dev and a
// layout change costs a warmup rather than a dual-read shim.
func TestHistoryStore_ResetsOnLayoutChange(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	key := histKey{streamID: 1, aggregator: testAggMedian}

	// State as a node running a different layout left it: an index naming a
	// pair, and a version that is not the current one.
	require.NoError(t, writeHistoryIndex(kv, []histKey{key}))
	require.NoError(t, kv.Write(keyHistoryVersion, []byte{historyLayoutVersion + 1}))

	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(key.streamID, key.aggregator, 5))
	_, err := s.Append(key.streamID, key.aggregator, 2_000, testDecimal(2))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	// The version is re-stamped and the pair starts over from empty.
	version, err := readHistoryLayoutVersion(kv)
	require.NoError(t, err)
	assert.Equal(t, historyLayoutVersion, version)

	records := readHistoryRecords(t, kv, key.streamID, key.aggregator)
	require.Len(t, records, 1, "the window re-warms from empty; nothing is carried over")
	assert.Equal(t, uint64(2_000), records[0].ObservedAtNanoseconds)

	// And the reset does not run again.
	writesBefore := kv.writes[string(keyHistoryVersion)]
	s = newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(key.streamID, key.aggregator, 5))
	require.NoError(t, s.Flush(kv))
	assert.Equal(t, writesBefore, kv.writes[string(keyHistoryVersion)], "the version must be written once, not every round")
}

// TestHistoryStore_LayoutChangeAbandonsStoredWindows covers the two things a
// layout reset must do to state that was actually written: a pair still required
// re-warms from empty rather than adopting bytes that merely happen to decode,
// and a pair no longer required has all of its keys deleted rather than being
// stranded by an index that no longer names it.
func TestHistoryStore_LayoutChangeAbandonsStoredWindows(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	kept := histKey{streamID: 1, aggregator: testAggMedian}
	dropped := histKey{streamID: 2, aggregator: testAggMedian}

	// Real stored state under the current layout: two pairs, each with a header,
	// a chunk and an index entry.
	seed := newTestHistoryStore(t, kv)
	for _, k := range []histKey{kept, dropped} {
		require.NoError(t, seed.SetRequired(k.streamID, k.aggregator, 3))
		_, err := seed.Append(k.streamID, k.aggregator, 1_000, testDecimal(7))
		require.NoError(t, err)
	}
	require.NoError(t, seed.Flush(kv))
	require.Len(t, readHistoryRecords(t, kv, kept.streamID, kept.aggregator), 1)
	require.Len(t, readHistoryRecords(t, kv, dropped.streamID, dropped.aggregator), 1)

	// A node comes up on a different layout.
	require.NoError(t, kv.Write(keyHistoryVersion, []byte{historyLayoutVersion + 1}))

	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.SetRequired(kept.streamID, kept.aggregator, 3))
	_, err := s.Append(kept.streamID, kept.aggregator, 2_000, testDecimal(9))
	require.NoError(t, err)
	require.NoError(t, s.Flush(kv))

	// The still-required pair starts over: the pre-reset record is gone, even
	// though it was stored in a form this layout can decode.
	records := readHistoryRecords(t, kv, kept.streamID, kept.aggregator)
	require.Len(t, records, 1, "the abandoned record must not be carried over")
	assert.Equal(t, uint64(2_000), records[0].ObservedAtNanoseconds)

	// The pair nobody requires is reclaimed, keys and all.
	assert.Nil(t, readHistory(t, kv, dropped.streamID, dropped.aggregator), "the header must be deleted")
	for _, key := range historyKeys(dropped.streamID, dropped.aggregator) {
		b, err := kv.Read(key)
		require.NoError(t, err)
		assert.Empty(t, b, "key %x must be deleted", key)
	}

	// And the rewritten index names only what survived.
	index, err := readHistoryIndex(kv)
	require.NoError(t, err)
	assert.Equal(t, []histKey{kept}, index)
}

// TestHistoryStore_UnusedStateStaysUntouched checks the layout reset does not
// stamp a version onto a DON that has never stored any history.
func TestHistoryStore_UnusedStateStaysUntouched(t *testing.T) {
	t.Parallel()

	kv := newCountingKV()
	s := newTestHistoryStore(t, kv)
	require.NoError(t, s.Flush(kv))
	assert.Empty(t, kv.writes)
}

// TestHistoryStore_CorruptChunk covers the other half of the fail-soft path: the
// header decodes but a chunk it names does not.
func TestHistoryStore_CorruptChunk(t *testing.T) {
	t.Parallel()

	for name, damage := range map[string]func(t *testing.T, kv *countingKV){
		"garbage": func(t *testing.T, kv *countingKV) {
			require.NoError(t, kv.Write(historyChunkKey(1, testAggMedian, 0), []byte("garbage")))
		},
		"missing": func(t *testing.T, kv *countingKV) {
			require.NoError(t, kv.Delete(historyChunkKey(1, testAggMedian, 0)))
		},
		"stale lap": func(t *testing.T, kv *countingKV) {
			// A chunk from an earlier lap: well formed, but its sequence is not
			// one the header retains.
			stale := protocol.NewRingWindow(nil)
			_, err := stale.SetRequiredCount(5)
			require.NoError(t, err)
			_, err = stale.Append(999, testDecimal(99))
			require.NoError(t, err)
			b, err := stale.WriteSet().Chunk.MarshalBinary()
			require.NoError(t, err)
			require.NoError(t, kv.Write(historyChunkKey(1, testAggMedian, 0), b))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Deep enough to span several chunks, so the reset has more than
			// the damaged slot to clear.
			const depth = protocol.MaxHistoryChunkRecords * 2

			kv := newCountingKV()
			warmHistory(t, kv, histKey{streamID: 1, aggregator: testAggMedian}, depth, depth)
			require.Equal(t, 2, readHistory(t, kv, 1, testAggMedian).Header().ChunkCount())
			damage(t, kv)

			// The round must survive, discard the window and start warming again.
			s := newTestHistoryStore(t, kv)
			require.NoError(t, s.SetRequired(1, testAggMedian, depth))
			_, err := s.Series(1, testAggMedian, depth, calculated.FieldValue)
			require.Error(t, err, "a channel must not evaluate over a window that could not be trusted")
			assert.True(t, s.corrupt[histKey{streamID: 1, aggregator: testAggMedian}])

			appended, err := s.Append(1, testAggMedian, 10_000_000, testDecimal(7))
			require.NoError(t, err)
			assert.True(t, appended)
			require.NoError(t, s.Flush(kv))

			records := readHistoryRecords(t, kv, 1, testAggMedian)
			require.Len(t, records, 1, "the window must re-warm from empty")
			assert.Equal(t, uint64(10_000_000), records[0].ObservedAtNanoseconds)

			// Nothing of the discarded window is left addressable.
			for slot := range uint32(protocol.MaxHistoryChunkSlots) {
				if slot == protocol.HistoryChunkSlot(0) {
					continue // the re-warmed window's first chunk
				}
				b, err := kv.Read(historyChunkKey(1, testAggMedian, slot))
				require.NoError(t, err)
				assert.Empty(t, b, "slot %d must have been cleared by the reset", slot)
			}
		})
	}
}

// TestHistoryStore_LapsTheRing runs enough rounds to reuse every slot several
// times, which is where a stale chunk or a slot collision would show up.
func TestHistoryStore_LapsTheRing(t *testing.T) {
	t.Parallel()

	const (
		required = protocol.MaxHistoryChunkRecords * 2
		rounds   = protocol.MaxHistoryChunkRecords * protocol.MaxHistoryChunkSlots * 3
	)
	kv := newCountingKV()
	key := histKey{streamID: 1, aggregator: testAggMedian}
	warmHistory(t, kv, key, required, rounds)

	w := readHistory(t, kv, key.streamID, key.aggregator)
	requireHistoryRetention(t, w)
	require.Greater(t, w.Header().FirstSequence(), uint64(2*protocol.MaxHistoryChunkSlots), "the ring must have wrapped")

	records := readHistoryNewest(t, kv, key.streamID, key.aggregator, required)
	require.Len(t, records, required)
	for i, record := range records {
		assert.Equal(t, uint64(rounds-required+1+i)*1_000, record.ObservedAtNanoseconds)
	}

	// Stored keys stay bounded by the ring however long it runs.
	slots := 0
	for slot := range uint32(protocol.MaxHistoryChunkSlots) {
		b, err := kv.Read(historyChunkKey(key.streamID, key.aggregator, slot))
		require.NoError(t, err)
		if len(b) > 0 {
			slots++
		}
	}
	assert.Equal(t, w.Header().ChunkCount(), slots, "the stored slots must be exactly the retained chunks")
}

// TestHistoryStore_DeterministicAcrossALap is the property a divergence would
// halt the DON over, exercised across ring wraparound and a capacity change.
func TestHistoryStore_DeterministicAcrossALap(t *testing.T) {
	t.Parallel()

	run := func() map[string][]byte {
		kv := newCountingKV()
		key := histKey{streamID: 1, aggregator: testAggMedian}
		for round := 1; round <= protocol.MaxHistoryChunkRecords*8; round++ {
			required := uint32(protocol.MaxHistoryChunkRecords * 3)
			if round > protocol.MaxHistoryChunkRecords*5 {
				required = protocol.MaxHistoryChunkRecords / 2
			}
			s := newTestHistoryStore(t, kv)
			require.NoError(t, s.SetRequired(key.streamID, key.aggregator, required))
			_, err := s.Append(key.streamID, key.aggregator, uint64(round)*1_000, testDecimal(int64(round)))
			require.NoError(t, err)
			require.NoError(t, s.Flush(kv))
		}
		return kv.m
	}

	assert.Equal(t, run(), run(), "two oracles running identical rounds must end with byte-identical state")
}
