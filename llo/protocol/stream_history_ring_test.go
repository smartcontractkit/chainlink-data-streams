package protocol

import (
	"fmt"
	"math"
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ringStore is a stand-in for the plugin's KeyValueState: one header value and
// MaxHistoryChunkSlots slot values per pair, addressed exactly the way the real
// store will address them.
//
// It exists so the tests drive the layout the way a round does — decode header,
// read only the planned slots, mutate, apply the write set, throw the working
// copy away — rather than reaching into an in-memory window that never went
// through storage. Slot reads are counted, because "how many reads does a round
// cost" is the property this layout exists to control.
type ringStore struct {
	header    []byte
	slots     map[uint32][]byte
	slotReads int
}

func newRingStore() *ringStore {
	return &ringStore{slots: map[uint32][]byte{}}
}

// window decodes the stored header and returns a working copy for a round.
func (s *ringStore) window(t *testing.T) *RingWindow {
	t.Helper()
	if len(s.header) == 0 {
		return NewRingWindow(nil)
	}
	header, err := UnmarshalStreamHistoryHeader(s.header)
	require.NoError(t, err)
	return NewRingWindow(header)
}

// provide reads the named sequences from their slots and hands them over.
func (s *ringStore) provide(t *testing.T, w *RingWindow, sequences []uint64) {
	t.Helper()
	for _, sequence := range sequences {
		b, ok := s.slots[HistoryChunkSlot(sequence)]
		require.True(t, ok, "planned sequence %d is not stored", sequence)
		s.slotReads++
		chunk, err := UnmarshalStreamHistoryChunk(b)
		require.NoError(t, err)
		require.NoError(t, w.Provide(chunk))
	}
}

// apply persists a write set: writes first, then deletes.
func (s *ringStore) apply(t *testing.T, set RingWriteSet) {
	t.Helper()
	if set.Header != nil {
		b, err := set.Header.MarshalBinary()
		require.NoError(t, err)
		s.header = b
	}
	if set.Chunk != nil {
		b, err := set.Chunk.MarshalBinary()
		require.NoError(t, err)
		s.slots[set.Chunk.Slot()] = b
	}
	for _, slot := range set.DeletedSlots {
		delete(s.slots, slot)
	}
}

// round runs one full round against the store: apply the requirement, load what
// appending needs, append, persist.
func (s *ringStore) round(t *testing.T, required uint32, observedAtNanoseconds uint64, value StreamValue) RingWriteSet {
	t.Helper()
	w := s.window(t)
	_, err := w.SetRequiredCount(required)
	require.NoError(t, err)
	s.provide(t, w, w.AppendPlan())
	appended, err := w.Append(observedAtNanoseconds, value)
	require.NoError(t, err)
	require.True(t, appended)
	set := w.WriteSet()
	s.apply(t, set)
	return set
}

// newest reads the newest n records back through storage.
func (s *ringStore) newest(t *testing.T, n uint32) ([]StreamHistoryRecord, error) {
	t.Helper()
	w := s.window(t)
	sequences, err := w.ReadPlan(n)
	if err != nil {
		return nil, err
	}
	s.provide(t, w, sequences)
	return w.Newest(n)
}

func testValue(i int) StreamValue { return ToDecimal(decimal.NewFromInt(int64(i))) }

func testTs(i int) uint64 { return uint64(i) * 1_000_000_000 }

// warm appends n records, one per round, at second cadence.
func warmRing(t *testing.T, s *ringStore, required uint32, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		s.round(t, required, testTs(i), testValue(i))
	}
}

func TestHistoryChunkLimits(t *testing.T) {
	// The chunk size must divide the maximum depth, so a window at full depth is
	// a whole number of chunks and the "only the newest chunk is partial"
	// invariant is reachable at the boundary.
	require.Zero(t, MaxHistoryRecordsPerPair%MaxHistoryChunkRecords)

	// The ring must hold the deepest window plus the chunk a round creates
	// before eviction runs, or a new chunk would land on a live slot.
	maxRetained := (MaxHistoryRetainedRecords + MaxHistoryChunkRecords - 1) / MaxHistoryChunkRecords
	require.Equal(t, MaxHistoryChunkSlots, maxRetained+1,
		"the ring must have exactly one slot spare above the deepest retention")
}

func TestRingWindowWarmup(t *testing.T) {
	const required = 100
	s := newRingStore()

	// An empty window plans no reads and reports insufficiency from the header.
	w := s.window(t)
	require.Empty(t, w.AppendPlan())
	_, err := w.ReadPlan(required)
	require.ErrorIs(t, err, ErrInsufficientStreamHistory)

	for i := 1; i < required; i++ {
		s.round(t, required, testTs(i), testValue(i))

		// Warming up costs a single header read: the shortfall is decided from
		// the header, so no chunk is touched.
		before := s.slotReads
		_, err := s.newest(t, required)
		require.ErrorIs(t, err, ErrInsufficientStreamHistory)
		require.Equal(t, before, s.slotReads, "an unsatisfied read must not touch a chunk")
	}

	s.round(t, required, testTs(required), testValue(required))
	records, err := s.newest(t, required)
	require.NoError(t, err)
	require.Len(t, records, required)
	require.Equal(t, testTs(1), records[0].ObservedAtNanoseconds)
	require.Equal(t, testTs(required), records[required-1].ObservedAtNanoseconds)
}

func TestRingWindowSealsAndWritesOnlyTheNewestChunk(t *testing.T) {
	s := newRingStore()
	const required = 200

	// Filling the first chunk rewrites the same slot every round.
	for i := 1; i <= MaxHistoryChunkRecords; i++ {
		set := s.round(t, required, testTs(i), testValue(i))
		require.NotNil(t, set.Chunk)
		require.Equal(t, uint64(0), set.Chunk.Sequence())
		require.Empty(t, set.DeletedSlots)
	}
	sealed := append([]byte(nil), s.slots[0]...)

	// The next record starts a new chunk, and the sealed one is never rewritten
	// again — which is the whole point of the layout.
	set := s.round(t, required, testTs(MaxHistoryChunkRecords+1), testValue(MaxHistoryChunkRecords+1))
	require.NotNil(t, set.Chunk)
	require.Equal(t, uint64(1), set.Chunk.Sequence())
	require.Equal(t, 1, set.Chunk.Len())
	require.Equal(t, sealed, s.slots[0])

	for i := MaxHistoryChunkRecords + 2; i <= 3*MaxHistoryChunkRecords; i++ {
		s.round(t, required, testTs(i), testValue(i))
		require.Equal(t, sealed, s.slots[0], "a sealed chunk must never be rewritten")
	}
}

func TestRingWindowAppendPlan(t *testing.T) {
	s := newRingStore()
	const required = 200

	require.Empty(t, s.window(t).AppendPlan(), "an empty window starts a chunk and needs nothing loaded")

	warmRing(t, s, required, 1)
	require.Equal(t, []uint64{0}, s.window(t).AppendPlan(), "a partial newest chunk must be loaded to append into")

	for i := 2; i <= MaxHistoryChunkRecords; i++ {
		s.round(t, required, testTs(i), testValue(i))
	}
	require.Empty(t, s.window(t).AppendPlan(), "a sealed newest chunk needs nothing loaded")

	s.round(t, required, testTs(MaxHistoryChunkRecords+1), testValue(MaxHistoryChunkRecords+1))
	require.Equal(t, []uint64{1}, s.window(t).AppendPlan())
}

func TestRingWindowAppendRequiresTheTailChunk(t *testing.T) {
	s := newRingStore()
	warmRing(t, s, 200, 3)

	w := s.window(t)
	_, err := w.Append(testTs(4), testValue(4))
	require.ErrorIs(t, err, ErrHistoryChunkNotLoaded)
	require.True(t, w.WriteSet().Empty(), "a failed append must not produce a write")
}

func TestRingWindowAppendRules(t *testing.T) {
	t.Run("zero capacity stores nothing", func(t *testing.T) {
		w := NewRingWindow(nil)
		appended, err := w.Append(testTs(1), testValue(1))
		require.NoError(t, err)
		require.False(t, appended)
		require.True(t, w.WriteSet().Empty())
	})

	t.Run("nil value", func(t *testing.T) {
		w := NewRingWindow(nil)
		_, err := w.SetRequiredCount(10)
		require.NoError(t, err)
		_, err = w.Append(testTs(1), nil)
		require.ErrorIs(t, err, ErrNilStreamValue)
	})

	t.Run("not strictly newer", func(t *testing.T) {
		s := newRingStore()
		warmRing(t, s, 10, 3)

		for _, ts := range []uint64{testTs(3), testTs(2), 0} {
			w := s.window(t)
			s.provide(t, w, w.AppendPlan())
			appended, err := w.Append(ts, testValue(99))
			require.NoError(t, err, "a non-advancing timestamp is normal, not an error")
			require.False(t, appended)
			require.Nil(t, w.WriteSet().Chunk)
		}

		records, err := s.newest(t, 3)
		require.NoError(t, err)
		require.Len(t, records, 3)
	})

	t.Run("oversized record", func(t *testing.T) {
		w := NewRingWindow(nil)
		_, err := w.SetRequiredCount(10)
		require.NoError(t, err)
		huge := ToDecimal(decimal.NewFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(1000), nil), -2))
		_, err = w.Append(testTs(1), huge)
		require.ErrorIs(t, err, ErrHistoryRecordTooLarge)
		require.Zero(t, w.Len(), "a rejected record leaves a gap, it does not half-append")
	})
}

func TestRingWindowEviction(t *testing.T) {
	const required = 100
	s := newRingStore()

	deleted := 0
	for i := 1; i <= 500; i++ {
		set := s.round(t, required, testTs(i), testValue(i))
		deleted += len(set.DeletedSlots)

		header, err := UnmarshalStreamHistoryHeader(s.header)
		require.NoError(t, err)

		// Retention keeps as few whole chunks as cover the required depth, so
		// the window overshoots by less than one chunk and never more.
		if i >= required {
			require.GreaterOrEqual(t, header.Len(), required)
		}
		require.Less(t, header.Len(), required+MaxHistoryChunkRecords)
		require.Len(t, s.slots, header.ChunkCount(), "every retained chunk is stored and nothing else is")
	}
	require.Positive(t, deleted, "eviction must actually delete slots")

	records, err := s.newest(t, required)
	require.NoError(t, err)
	require.Len(t, records, required)
	require.Equal(t, testTs(500), records[required-1].ObservedAtNanoseconds, "the newest record must survive")
	require.Equal(t, testTs(500-required+1), records[0].ObservedAtNanoseconds)
}

func TestRingWindowLapsTheRing(t *testing.T) {
	// Long enough to reuse every slot several times over, which is what makes
	// stale-slot handling reachable at all.
	const (
		required = 128
		rounds   = 3000
	)
	s := newRingStore()

	for i := 1; i <= rounds; i++ {
		s.round(t, required, testTs(i), testValue(i))
		require.LessOrEqual(t, len(s.slots), MaxHistoryChunkSlots)
	}
	require.Greater(t, s.window(t).Header().FirstSequence(), uint64(2*MaxHistoryChunkSlots),
		"the ring must have wrapped several times")

	records, err := s.newest(t, required)
	require.NoError(t, err)
	require.Len(t, records, required)
	for i, record := range records {
		want := rounds - required + 1 + i
		require.Equal(t, testTs(want), record.ObservedAtNanoseconds)
	}
}

func TestRingWindowRejectsAStaleChunk(t *testing.T) {
	s := newRingStore()
	const required = 64
	warmRing(t, s, required, 3*MaxHistoryChunkRecords)

	// A chunk from an earlier lap of the ring: it decodes, but its sequence is
	// no longer retained. Reads address slots, so this is exactly what a missed
	// delete or a resurrected value would look like.
	header := s.window(t).Header()
	stale := &StreamHistoryChunk{
		sequence: header.FirstSequence() - 1,
		records:  []StreamHistoryRecord{{ObservedAtNanoseconds: testTs(1), Value: testValue(1)}},
	}
	b, err := stale.MarshalBinary()
	require.NoError(t, err)
	decoded, err := UnmarshalStreamHistoryChunk(b)
	require.NoError(t, err, "a stale chunk is well formed in isolation")

	err = s.window(t).Provide(decoded)
	require.ErrorIs(t, err, ErrCorruptStreamHistory)
	require.Contains(t, err.Error(), "not retained")
}

func TestRingWindowReadPlanIsMinimal(t *testing.T) {
	const required = MaxHistoryChunkRecords * 4
	s := newRingStore()
	warmRing(t, s, required, required)

	w := s.window(t)
	require.Equal(t, 4, w.Header().ChunkCount())

	for _, tc := range []struct {
		n     uint32
		reads int
	}{
		{n: 1, reads: 1},
		{n: MaxHistoryChunkRecords, reads: 1},
		{n: MaxHistoryChunkRecords + 1, reads: 2},
		{n: required, reads: 4},
	} {
		t.Run(fmt.Sprintf("newest %d", tc.n), func(t *testing.T) {
			plan, err := s.window(t).ReadPlan(tc.n)
			require.NoError(t, err)
			require.Len(t, plan, tc.reads)

			records, err := s.newest(t, tc.n)
			require.NoError(t, err)
			require.Len(t, records, int(tc.n))
			require.Equal(t, testTs(required), records[len(records)-1].ObservedAtNanoseconds)
		})
	}

	plan, err := w.ReadPlan(0)
	require.NoError(t, err)
	require.Empty(t, plan)
}

func TestRingWindowNewestNeedsThePlannedChunks(t *testing.T) {
	const required = MaxHistoryChunkRecords * 2
	s := newRingStore()
	warmRing(t, s, required, required)

	w := s.window(t)
	s.provide(t, w, []uint64{1}) // the newest chunk only
	_, err := w.Newest(required)
	require.ErrorIs(t, err, ErrHistoryChunkNotLoaded)
}

func TestRingWindowNewestRejectsABrokenSeam(t *testing.T) {
	const required = MaxHistoryChunkRecords * 2
	s := newRingStore()
	warmRing(t, s, required, required)

	// Timestamps are strictly increasing within a chunk and across the header's
	// per-chunk start timestamps; the seam between two chunks is the remaining
	// gap, so it is checked when the records are concatenated.
	w := s.window(t)
	s.provide(t, w, []uint64{0, 1})
	older := w.chunks[0]
	older.records[len(older.records)-1].ObservedAtNanoseconds = w.chunks[1].records[0].ObservedAtNanoseconds

	_, err := w.Newest(required)
	require.ErrorIs(t, err, ErrCorruptStreamHistory)
	require.Contains(t, err.Error(), "not strictly after")
}

func TestRingWindowSetRequiredCount(t *testing.T) {
	t.Run("increase writes only the header", func(t *testing.T) {
		s := newRingStore()
		warmRing(t, s, 100, 100)

		w := s.window(t)
		changed, err := w.SetRequiredCount(300)
		require.NoError(t, err)
		require.True(t, changed)

		set := w.WriteSet()
		require.NotNil(t, set.Header)
		require.Nil(t, set.Chunk, "growing capacity must not rewrite a chunk")
		require.Empty(t, set.DeletedSlots)

		// The extra depth fills over subsequent rounds; until then, unsatisfied.
		s.apply(t, set)
		_, err = s.newest(t, 300)
		require.ErrorIs(t, err, ErrInsufficientStreamHistory)
	})

	t.Run("decrease deletes only", func(t *testing.T) {
		s := newRingStore()
		warmRing(t, s, 300, 300)
		before := len(s.slots)

		w := s.window(t)
		_, err := w.SetRequiredCount(10)
		require.NoError(t, err)
		set := w.WriteSet()
		require.NotNil(t, set.Header)
		require.Nil(t, set.Chunk, "shrinking capacity must not rewrite a chunk")
		require.NotEmpty(t, set.DeletedSlots)
		s.apply(t, set)

		require.Less(t, len(s.slots), before)
		require.Len(t, s.slots, s.window(t).Header().ChunkCount())

		records, err := s.newest(t, 10)
		require.NoError(t, err)
		require.Equal(t, testTs(300), records[9].ObservedAtNanoseconds)
	})

	t.Run("unchanged", func(t *testing.T) {
		w := NewRingWindow(nil)
		changed, err := w.SetRequiredCount(0)
		require.NoError(t, err)
		require.False(t, changed)
		require.True(t, w.WriteSet().Empty())
	})

	t.Run("over the cap", func(t *testing.T) {
		w := NewRingWindow(nil)
		_, err := w.SetRequiredCount(MaxHistoryRecordsPerPair + 1)
		require.Error(t, err)
	})

	t.Run("teardown evicts everything", func(t *testing.T) {
		s := newRingStore()
		warmRing(t, s, 200, 200)

		w := s.window(t)
		_, err := w.SetRequiredCount(0)
		require.NoError(t, err)
		require.Zero(t, w.Len())
		require.Zero(t, w.Header().ChunkCount())

		set := w.WriteSet()
		require.Len(t, set.DeletedSlots, 4)
		s.apply(t, set)
		require.Empty(t, s.slots)

		// An emptied window is canonically zero-valued, so two oracles that
		// arrive here by different routes write identical bytes.
		empty, err := NewRingWindow(nil).Header().MarshalBinary()
		require.NoError(t, err)
		torn, err := set.Header.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, empty, torn)
	})
}

func TestResetRingWindow(t *testing.T) {
	w := ResetRingWindow()
	require.Zero(t, w.Len())
	require.Zero(t, w.RequiredCount())

	set := w.WriteSet()
	require.NotNil(t, set.Header)
	require.Nil(t, set.Chunk)
	require.Len(t, set.DeletedSlots, MaxHistoryChunkSlots,
		"recovery deletes the whole slot space, because a bad header cannot say which slots are live")
	for i, slot := range set.DeletedSlots {
		require.Equal(t, uint32(i), slot)
	}

	// A reset window re-warms from empty.
	_, err := w.SetRequiredCount(10)
	require.NoError(t, err)
	appended, err := w.Append(testTs(1), testValue(1))
	require.NoError(t, err)
	require.True(t, appended)

	set = w.WriteSet()
	require.NotNil(t, set.Chunk)
	require.Equal(t, uint64(0), set.Chunk.Sequence())
	require.NotContains(t, set.DeletedSlots, set.Chunk.Slot(),
		"a slot must never be both written and deleted in one round")
	require.Len(t, set.DeletedSlots, MaxHistoryChunkSlots-1)
}

func TestRingWindowWriteSetIsDeterministic(t *testing.T) {
	// Two oracles running the same rounds must produce identical bytes, key for
	// key: the layout is replicated state, so any divergence halts the DON.
	run := func() *ringStore {
		s := newRingStore()
		for i := 1; i <= 400; i++ {
			required := uint32(150)
			if i > 250 {
				required = 80
			}
			s.round(t, required, testTs(i), testValue(i))
		}
		return s
	}

	a, b := run(), run()
	require.Equal(t, a.header, b.header)
	require.Equal(t, a.slots, b.slots)

	// And repeated marshaling of the same window is byte-identical.
	header, err := UnmarshalStreamHistoryHeader(a.header)
	require.NoError(t, err)
	first, err := header.MarshalBinary()
	require.NoError(t, err)
	again, err := header.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, a.header, first)
}

func TestRingWindowRoundTripsEveryValueType(t *testing.T) {
	values := []StreamValue{
		ToDecimal(decimal.New(110000000000000000, -8)),
		&Quote{
			Bid:       decimal.New(110000000000000000, -8),
			Benchmark: decimal.New(110000000000000001, -8),
			Ask:       decimal.New(110000000000000002, -8),
		},
		&TimestampedStreamValue{
			ObservedAtNanoseconds: 999,
			StreamValue:           ToDecimal(decimal.NewFromInt(7)),
		},
	}

	s := newRingStore()
	for i, value := range values {
		s.round(t, uint32(len(values)), testTs(i+1), value)
	}

	records, err := s.newest(t, uint32(len(values)))
	require.NoError(t, err)
	require.Len(t, records, len(values))
	for i, record := range records {
		require.Equal(t, values[i].Type(), record.Value.Type())
		want, err := values[i].MarshalBinary()
		require.NoError(t, err)
		got, err := record.Value.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestUnmarshalStreamHistoryHeaderCorruption(t *testing.T) {
	// A valid header to mutate: two sealed chunks and a partial one.
	valid := func() *LLOStreamHistoryHeaderProto {
		return &LLOStreamHistoryHeaderProto{
			RequiredCount: 150,
			FirstSequence: 7,
			Counts:        []uint32{MaxHistoryChunkRecords, MaxHistoryChunkRecords, 30},
			ChunkFirstObservationTimestampNanoseconds: []uint64{100, 200, 300},
			LastObservationTimestampNanoseconds:       400,
		}
	}
	b, err := deterministicMarshal.Marshal(valid())
	require.NoError(t, err)
	header, err := UnmarshalStreamHistoryHeader(b)
	require.NoError(t, err)
	require.Equal(t, 158, header.Len())
	require.Equal(t, []uint64{7, 8, 9}, header.Sequences())

	for name, mutate := range map[string]func(*LLOStreamHistoryHeaderProto){
		"required over the cap": func(h *LLOStreamHistoryHeaderProto) {
			h.RequiredCount = MaxHistoryRecordsPerPair + 1
		},
		"counts and timestamps disagree in length": func(h *LLOStreamHistoryHeaderProto) {
			h.Counts = h.Counts[:2]
		},
		"too many chunks": func(h *LLOStreamHistoryHeaderProto) {
			h.RequiredCount = MaxHistoryRecordsPerPair
			h.Counts = make([]uint32, MaxHistoryChunkSlots+1)
			h.ChunkFirstObservationTimestampNanoseconds = make([]uint64, MaxHistoryChunkSlots+1)
			for i := range h.Counts {
				h.Counts[i] = MaxHistoryChunkRecords
				h.ChunkFirstObservationTimestampNanoseconds[i] = uint64(i + 1)
			}
			h.LastObservationTimestampNanoseconds = uint64(len(h.Counts))
		},
		"empty with a sequence": func(h *LLOStreamHistoryHeaderProto) {
			h.Counts = nil
			h.ChunkFirstObservationTimestampNanoseconds = nil
			h.LastObservationTimestampNanoseconds = 0
		},
		"empty with a last timestamp": func(h *LLOStreamHistoryHeaderProto) {
			h.Counts = nil
			h.ChunkFirstObservationTimestampNanoseconds = nil
			h.FirstSequence = 0
		},
		"empty chunk": func(h *LLOStreamHistoryHeaderProto) { h.Counts[1] = 0 },
		"chunk over the chunk size": func(h *LLOStreamHistoryHeaderProto) {
			h.Counts[2] = MaxHistoryChunkRecords + 1
		},
		"partial sealed chunk": func(h *LLOStreamHistoryHeaderProto) { h.Counts[0] = 1 },
		"chunk starts out of order": func(h *LLOStreamHistoryHeaderProto) {
			h.ChunkFirstObservationTimestampNanoseconds[1] = 100
		},
		"chunk starts after the window ends": func(h *LLOStreamHistoryHeaderProto) {
			h.LastObservationTimestampNanoseconds = 250
		},
		"single-record newest chunk disagreeing with the end": func(h *LLOStreamHistoryHeaderProto) {
			h.Counts[2] = 1
		},
		"redundant oldest chunk": func(h *LLOStreamHistoryHeaderProto) { h.RequiredCount = 90 },
		"retained more than a chunk past the requirement": func(h *LLOStreamHistoryHeaderProto) {
			h.Counts = []uint32{MaxHistoryChunkRecords, MaxHistoryChunkRecords}
			h.ChunkFirstObservationTimestampNanoseconds = []uint64{100, 200}
			h.RequiredCount = MaxHistoryChunkRecords
		},
		"sequence overflow": func(h *LLOStreamHistoryHeaderProto) { h.FirstSequence = math.MaxUint64 },
	} {
		t.Run(name, func(t *testing.T) {
			pb := valid()
			mutate(pb)
			b, err := deterministicMarshal.Marshal(pb)
			require.NoError(t, err)

			header, err := UnmarshalStreamHistoryHeader(b)
			require.ErrorIs(t, err, ErrCorruptStreamHistory)
			require.Nil(t, header)
		})
	}

	t.Run("garbage", func(t *testing.T) {
		_, err := UnmarshalStreamHistoryHeader([]byte("not a protobuf"))
		require.ErrorIs(t, err, ErrCorruptStreamHistory)
	})

	t.Run("nil proto", func(t *testing.T) {
		_, err := StreamHistoryHeaderFromProto(nil)
		require.ErrorIs(t, err, ErrCorruptStreamHistory)
	})

	t.Run("empty is a valid empty window", func(t *testing.T) {
		header, err := UnmarshalStreamHistoryHeader(nil)
		require.NoError(t, err)
		require.Zero(t, header.Len())
		require.Zero(t, header.FirstObservationTimestampNanoseconds())
		require.Zero(t, header.LastObservationTimestampNanoseconds())
	})
}

func TestUnmarshalStreamHistoryChunkCorruption(t *testing.T) {
	value, err := marshalProtoStreamValue(testValue(1))
	require.NoError(t, err)
	valid := func() *LLOStreamHistoryChunkProto {
		return &LLOStreamHistoryChunkProto{
			Sequence: 3,
			Records: []*LLOStreamHistoryRecord{
				{ObservedAtNanoseconds: 100, Value: value},
				{ObservedAtNanoseconds: 200, Value: value},
			},
		}
	}
	b, err := deterministicMarshal.Marshal(valid())
	require.NoError(t, err)
	chunk, err := UnmarshalStreamHistoryChunk(b)
	require.NoError(t, err)
	require.Equal(t, 2, chunk.Len())
	require.Equal(t, uint64(3), chunk.Sequence())
	require.Equal(t, uint32(3), chunk.Slot())

	long, err := ToDecimal(decimal.NewFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(1000), nil), -2)).MarshalBinary()
	require.NoError(t, err)
	huge, err := ToDecimal(decimal.New(1, MaxDecimalExponent+1)).MarshalBinary()
	require.NoError(t, err)

	for name, mutate := range map[string]func(*LLOStreamHistoryChunkProto){
		"no records": func(c *LLOStreamHistoryChunkProto) { c.Records = nil },
		"too many records": func(c *LLOStreamHistoryChunkProto) {
			c.Records = make([]*LLOStreamHistoryRecord, MaxHistoryChunkRecords+1)
			for i := range c.Records {
				c.Records[i] = &LLOStreamHistoryRecord{ObservedAtNanoseconds: uint64(i + 1), Value: value}
			}
		},
		"nil record": func(c *LLOStreamHistoryChunkProto) { c.Records[1] = nil },
		"non-advancing timestamp": func(c *LLOStreamHistoryChunkProto) {
			c.Records[1].ObservedAtNanoseconds = 100
		},
		"regressing timestamp": func(c *LLOStreamHistoryChunkProto) {
			c.Records[1].ObservedAtNanoseconds = 50
		},
		"record over the size cap": func(c *LLOStreamHistoryChunkProto) {
			c.Records[1].Value = &LLOStreamValue{Type: LLOStreamValue_Decimal, Value: long}
		},
		"decimal exponent out of range": func(c *LLOStreamHistoryChunkProto) {
			c.Records[1].Value = &LLOStreamValue{Type: LLOStreamValue_Decimal, Value: huge}
		},
		"undecodable value": func(c *LLOStreamHistoryChunkProto) {
			c.Records[1].Value = &LLOStreamValue{Type: LLOStreamValue_Decimal, Value: []byte("nope")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pb := valid()
			mutate(pb)
			b, err := deterministicMarshal.Marshal(pb)
			require.NoError(t, err)

			chunk, err := UnmarshalStreamHistoryChunk(b)
			require.ErrorIs(t, err, ErrCorruptStreamHistory)
			require.Nil(t, chunk)
		})
	}

	t.Run("garbage", func(t *testing.T) {
		_, err := UnmarshalStreamHistoryChunk([]byte("not a protobuf"))
		require.ErrorIs(t, err, ErrCorruptStreamHistory)
	})

	t.Run("nil proto", func(t *testing.T) {
		_, err := StreamHistoryChunkFromProto(nil)
		require.ErrorIs(t, err, ErrCorruptStreamHistory)
	})
}

func TestRingWindowProvideRejectsAMismatch(t *testing.T) {
	s := newRingStore()
	const required = MaxHistoryChunkRecords * 2
	warmRing(t, s, required, required)

	load := func(t *testing.T, sequence uint64) *StreamHistoryChunk {
		t.Helper()
		chunk, err := UnmarshalStreamHistoryChunk(s.slots[HistoryChunkSlot(sequence)])
		require.NoError(t, err)
		return chunk
	}

	for name, tc := range map[string]struct {
		chunk func(*testing.T) *StreamHistoryChunk
		want  string
	}{
		"nil": {
			chunk: func(*testing.T) *StreamHistoryChunk { return nil },
			want:  "nil chunk",
		},
		"unretained sequence": {
			chunk: func(t *testing.T) *StreamHistoryChunk {
				c := load(t, 0)
				c.sequence = 99
				return c
			},
			want: "not retained",
		},
		"wrong record count": {
			chunk: func(t *testing.T) *StreamHistoryChunk {
				c := load(t, 0)
				c.records = c.records[:len(c.records)-1]
				return c
			},
			want: "header says",
		},
		"wrong start timestamp": {
			chunk: func(t *testing.T) *StreamHistoryChunk {
				c := load(t, 0)
				c.records[0].ObservedAtNanoseconds = 1
				return c
			},
			want: "starts at",
		},
		"newest chunk ends elsewhere": {
			chunk: func(t *testing.T) *StreamHistoryChunk {
				c := load(t, 1)
				c.records[len(c.records)-1].ObservedAtNanoseconds = math.MaxUint64
				return c
			},
			want: "ends at",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := s.window(t).Provide(tc.chunk(t))
			require.ErrorIs(t, err, ErrCorruptStreamHistory)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRingWindowAtMaximumDepth(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a full-depth window")
	}
	const required = MaxHistoryRecordsPerPair
	s := newRingStore()
	warmRing(t, s, required, required+MaxHistoryChunkRecords)

	header, err := UnmarshalStreamHistoryHeader(s.header)
	require.NoError(t, err)
	require.LessOrEqual(t, header.ChunkCount(), MaxHistoryChunkSlots-1,
		"a settled window must leave a spare slot for the next chunk")
	require.Less(t, header.Len(), MaxHistoryRetainedRecords+1)

	before := s.slotReads
	records, err := s.newest(t, required)
	require.NoError(t, err)
	require.Len(t, records, required)

	// The read cost of the deepest possible window, which is the number this
	// layout exists to keep small.
	assert.LessOrEqual(t, s.slotReads-before, MaxHistoryRecordsPerPair/MaxHistoryChunkRecords+1)
}

func Test_RingWindow_AppendOncePerRound(t *testing.T) {
	// The write set carries only the newest chunk, so a second append that
	// sealed one would lose its last record while the header already counted it.
	// Rejecting the second append keeps the stored window and its header in
	// agreement.
	w := NewRingWindow(nil)
	_, err := w.SetRequiredCount(MaxHistoryChunkRecords * 2)
	require.NoError(t, err)

	appended, err := w.Append(1, ToDecimal(decimal.NewFromInt(1)))
	require.NoError(t, err)
	require.True(t, appended)

	appended, err = w.Append(2, ToDecimal(decimal.NewFromInt(2)))
	require.ErrorIs(t, err, ErrHistoryAlreadyAppended)
	require.False(t, appended)
	require.Equal(t, uint32(1), w.Header().total(), "a rejected append must not be counted")

	// A rejected append stores nothing, so it does not spend the round's one
	// append either.
	w = NewRingWindow(nil)
	_, err = w.SetRequiredCount(4)
	require.NoError(t, err)
	appended, err = w.Append(10, ToDecimal(decimal.NewFromInt(1)))
	require.NoError(t, err)
	require.True(t, appended)

	stored := w.WriteSet().Chunk
	require.NotNil(t, stored)
	w = NewRingWindow(w.Header())
	require.NoError(t, w.Provide(stored))
	appended, err = w.Append(10, ToDecimal(decimal.NewFromInt(2)))
	require.NoError(t, err, "a value that is not strictly newer is rejected, not an error")
	require.False(t, appended)
	appended, err = w.Append(11, ToDecimal(decimal.NewFromInt(3)))
	require.NoError(t, err)
	require.True(t, appended)
}
