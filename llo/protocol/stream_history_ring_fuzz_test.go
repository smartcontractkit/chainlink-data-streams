package protocol

import (
	"math"
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// FuzzUnmarshalStreamHistoryHeader fuzzes the header decoder.
//
// The header is the most trusted thing in the chunked layout: read planning,
// eviction and the append rule are all decided from it without consulting a
// chunk, so a header that decoded while violating an invariant would put the
// round's reads and writes outside every bound the byte budget assumes. It is
// also replicated state read every round, so a panic here takes the node down.
func FuzzUnmarshalStreamHistoryHeader(f *testing.F) {
	seed := func(pb *LLOStreamHistoryHeaderProto) {
		b, err := deterministicMarshal.Marshal(pb)
		require.NoError(f, err)
		f.Add(b)
	}

	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte("not a protobuf"))
	seed(&LLOStreamHistoryHeaderProto{})
	seed(&LLOStreamHistoryHeaderProto{
		RequiredCount: 10,
		Counts:        []uint32{3},
		ChunkFirstObservationTimestampNanoseconds: []uint64{100},
		LastObservationTimestampNanoseconds:       300,
	})
	seed(&LLOStreamHistoryHeaderProto{
		RequiredCount: 150,
		FirstSequence: 7,
		Counts:        []uint32{MaxHistoryChunkRecords, MaxHistoryChunkRecords, 30},
		ChunkFirstObservationTimestampNanoseconds: []uint64{100, 200, 300},
		LastObservationTimestampNanoseconds:       400,
	})
	seed(&LLOStreamHistoryHeaderProto{
		RequiredCount: MaxHistoryRecordsPerPair,
		FirstSequence: math.MaxUint64 - 4,
		Counts:        []uint32{MaxHistoryChunkRecords, 1},
		ChunkFirstObservationTimestampNanoseconds: []uint64{1, 2},
		LastObservationTimestampNanoseconds:       2,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		header, err := UnmarshalStreamHistoryHeader(data)
		if err != nil {
			require.ErrorIs(t, err, ErrCorruptStreamHistory,
				"every decode failure must be reported as corruption so callers can uniformly reset and re-warm")
			require.Nil(t, header)
			return
		}
		require.NotNil(t, header)

		// Caps hold, so the reads and writes planned from this header stay
		// within what the byte budget was sized for.
		require.LessOrEqual(t, header.RequiredCount(), uint32(MaxHistoryRecordsPerPair))
		require.LessOrEqual(t, header.ChunkCount(), MaxHistoryChunkSlots)
		require.LessOrEqual(t, header.Len(), MaxHistoryRetainedRecords)

		counts := header.Counts()
		var total uint32
		for i, count := range counts {
			require.Positive(t, count)
			require.LessOrEqual(t, count, uint32(MaxHistoryChunkRecords))
			if i < len(counts)-1 {
				require.Equal(t, uint32(MaxHistoryChunkRecords), count, "only the newest chunk may be partial")
			}
			total += count
		}
		require.Equal(t, int(total), header.Len())

		if len(counts) > 0 {
			// Retention holds as few whole chunks as cover the requirement.
			require.Less(t, total-counts[0], header.RequiredCount())
			require.Less(t, uint64(total), uint64(header.RequiredCount())+MaxHistoryChunkRecords)

			// Sequences are contiguous and do not overflow.
			sequences := header.Sequences()
			require.Len(t, sequences, len(counts))
			for i, sequence := range sequences {
				require.Equal(t, header.FirstSequence()+uint64(i), sequence)
			}

			// The window's bounds are ordered.
			require.LessOrEqual(t, header.FirstObservationTimestampNanoseconds(), header.LastObservationTimestampNanoseconds())
		} else {
			require.Zero(t, header.FirstSequence())
			require.Zero(t, header.FirstObservationTimestampNanoseconds())
			require.Zero(t, header.LastObservationTimestampNanoseconds())
		}

		// Anything that decodes must re-encode identically, or a node that
		// restarted would write different bytes than one that did not.
		first, err := header.MarshalBinary()
		require.NoError(t, err)
		again, err := header.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, first, again)

		reparsed, err := UnmarshalStreamHistoryHeader(first)
		require.NoError(t, err)
		reencoded, err := reparsed.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, first, reencoded)
	})
}

// FuzzUnmarshalStreamHistoryChunk fuzzes the chunk decoder, which stands between
// arbitrary stored bytes and the records an expression evaluates.
func FuzzUnmarshalStreamHistoryChunk(f *testing.F) {
	value, err := marshalProtoStreamValue(ToDecimal(decimal.New(110000000000000000, -8)))
	require.NoError(f, err)
	quote, err := marshalProtoStreamValue(&Quote{
		Bid:       decimal.New(110000000000000000, -8),
		Benchmark: decimal.New(110000000000000001, -8),
		Ask:       decimal.New(110000000000000002, -8),
	})
	require.NoError(f, err)
	long, err := ToDecimal(decimal.NewFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(1000), nil), -2)).MarshalBinary()
	require.NoError(f, err)

	seed := func(pb *LLOStreamHistoryChunkProto) {
		b, err := deterministicMarshal.Marshal(pb)
		require.NoError(f, err)
		f.Add(b)
	}

	f.Add([]byte(nil))
	f.Add([]byte("not a protobuf"))
	seed(&LLOStreamHistoryChunkProto{})
	seed(&LLOStreamHistoryChunkProto{
		Sequence: 3,
		Records: []*LLOStreamHistoryRecord{
			{ObservedAtNanoseconds: 100, Value: value},
			{ObservedAtNanoseconds: 200, Value: quote},
		},
	})
	seed(&LLOStreamHistoryChunkProto{
		Sequence: math.MaxUint64,
		Records: []*LLOStreamHistoryRecord{
			{ObservedAtNanoseconds: 1, Value: &LLOStreamValue{Type: LLOStreamValue_Decimal, Value: long}},
		},
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		chunk, err := UnmarshalStreamHistoryChunk(data)
		if err != nil {
			require.ErrorIs(t, err, ErrCorruptStreamHistory)
			require.Nil(t, chunk)
			return
		}
		require.NotNil(t, chunk)

		records := chunk.Records()
		require.NotEmpty(t, records)
		require.LessOrEqual(t, len(records), MaxHistoryChunkRecords)
		for i, record := range records {
			require.NotNil(t, record.Value)
			if i > 0 {
				require.Greater(t, record.ObservedAtNanoseconds, records[i-1].ObservedAtNanoseconds)
			}
			size, err := historyRecordSize(record.ObservedAtNanoseconds, record.Value)
			require.NoError(t, err)
			require.LessOrEqual(t, size, MaxHistoryRecordBytes)
		}
		require.Equal(t, chunk.Sequence()%MaxHistoryChunkSlots, uint64(chunk.Slot()))

		first, err := chunk.MarshalBinary()
		require.NoError(t, err)
		again, err := chunk.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, first, again)

		reparsed, err := UnmarshalStreamHistoryChunk(first)
		require.NoError(t, err)
		reencoded, err := reparsed.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, first, reencoded)
	})
}

// FuzzRingWindowRounds fuzzes the mutation path end to end, driving a window
// through storage the way a round does.
//
// The invariants asserted after every round are the ones the plugin will rely
// on: the window never holds more than a chunk past its requirement, only the
// newest chunk is ever written, a slot is never written and deleted at once,
// and the stored slots are exactly the retained ones — the last of which is
// what stops the ring from leaking state it can no longer address.
func FuzzRingWindowRounds(f *testing.F) {
	f.Add(uint32(100), uint32(0), uint8(200), uint64(1_000_000_000), uint8(7))
	f.Add(uint32(1), uint32(1), uint8(10), uint64(1), uint8(1))
	f.Add(uint32(MaxHistoryChunkRecords), uint32(MaxHistoryChunkRecords*2), uint8(255), uint64(1), uint8(3))
	f.Add(uint32(MaxHistoryRecordsPerPair), uint32(0), uint8(255), uint64(1_000), uint8(11))
	f.Add(uint32(0), uint32(50), uint8(64), uint64(1_000), uint8(2))

	f.Fuzz(func(t *testing.T, required, laterRequired uint32, rounds uint8, interval uint64, switchAt uint8) {
		if required > MaxHistoryRecordsPerPair || laterRequired > MaxHistoryRecordsPerPair {
			return // rejected by construction; covered by the unit tests
		}
		if interval == 0 {
			interval = 1 // a non-advancing timestamp is covered separately
		}

		header := (*StreamHistoryHeader)(nil)
		slots := map[uint32][]byte{}
		var ts uint64

		for round := range int(rounds) {
			want := required
			if switchAt > 0 && round >= int(switchAt) {
				want = laterRequired
			}

			w := NewRingWindow(header)
			_, err := w.SetRequiredCount(want)
			require.NoError(t, err)

			for _, sequence := range w.AppendPlan() {
				b, ok := slots[HistoryChunkSlot(sequence)]
				require.True(t, ok, "the append plan named a slot that is not stored")
				chunk, err := UnmarshalStreamHistoryChunk(b)
				require.NoError(t, err)
				require.NoError(t, w.Provide(chunk))
			}

			ts += interval
			_, err = w.Append(ts, ToDecimal(decimal.NewFromInt(int64(round))))
			require.NoError(t, err)

			set := w.WriteSet()
			if set.Chunk != nil {
				require.Equal(t, set.Chunk.Sequence(), w.Header().FirstSequence()+uint64(w.Header().ChunkCount())-1,
					"only the newest chunk may be written")
				b, err := set.Chunk.MarshalBinary()
				require.NoError(t, err)
				slots[set.Chunk.Slot()] = b
				require.NotContains(t, set.DeletedSlots, set.Chunk.Slot(),
					"a slot must never be both written and deleted in one round")
			}
			for _, slot := range set.DeletedSlots {
				delete(slots, slot)
			}
			if set.Header != nil {
				b, err := set.Header.MarshalBinary()
				require.NoError(t, err)
				// Whatever a round writes must decode on the next one, on this
				// node or any other.
				header, err = UnmarshalStreamHistoryHeader(b)
				require.NoError(t, err)
			}

			require.Len(t, slots, header.ChunkCount(),
				"the stored slots must be exactly the retained chunks")
			require.Less(t, uint64(header.Len()), uint64(header.RequiredCount())+MaxHistoryChunkRecords)
			if header.RequiredCount() == 0 {
				require.Zero(t, header.Len())
			}
		}

		if header.RequiredCount() == 0 || header.Len() == 0 {
			return
		}

		// Whatever depth was reached must read back, oldest to newest, with the
		// last append at the end.
		n := uint32(header.Len())
		w := NewRingWindow(header)
		plan, err := w.ReadPlan(n)
		require.NoError(t, err)
		for _, sequence := range plan {
			chunk, err := UnmarshalStreamHistoryChunk(slots[HistoryChunkSlot(sequence)])
			require.NoError(t, err)
			require.NoError(t, w.Provide(chunk))
		}
		records, err := w.Newest(n)
		require.NoError(t, err)
		require.Len(t, records, int(n))
		require.Equal(t, ts, records[len(records)-1].ObservedAtNanoseconds)
		for i := 1; i < len(records); i++ {
			require.Greater(t, records[i].ObservedAtNanoseconds, records[i-1].ObservedAtNanoseconds)
		}
	})
}
