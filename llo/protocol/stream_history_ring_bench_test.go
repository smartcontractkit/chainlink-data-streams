package protocol

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
)

// Benchmarks for the chunked ring layout, measured against the single-blob
// layout it replaces.
//
// The number that matters is kvBytes/op: bytes a round writes for one pair.
// That is what is charged against the OCR3.1 per-round budget, and under the
// blob layout it is proportional to the window depth — which is what made the
// byte budget, rather than the pair count, the binding constraint. The point of
// the ring is that this figure becomes a function of the chunk size instead.
//
// A quote of three 18-digit decimals is the case worth measuring: one window
// serves _bid/_ask/_benchmark, and it is the largest realistic record.

func benchRingQuote(i int) StreamValue {
	return &Quote{
		Bid:       decimal.New(int64(110000000000000000+i), -8),
		Benchmark: decimal.New(int64(110000000000000001+i), -8),
		Ask:       decimal.New(int64(110000000000000002+i), -8),
	}
}

// benchRingStore is the storage the benchmarks write through, counting bytes the
// way the round budget does: key plus value for every write.
type benchRingStore struct {
	header []byte
	slots  map[uint32][]byte
	bytes  int
}

func (s *benchRingStore) apply(b *testing.B, set RingWriteSet) {
	b.Helper()
	if set.Header != nil {
		v, err := set.Header.MarshalBinary()
		if err != nil {
			b.Fatal(err)
		}
		s.header = v
		s.bytes += len(v) + 10 // hh/<streamID><aggregator>
	}
	if set.Chunk != nil {
		v, err := set.Chunk.MarshalBinary()
		if err != nil {
			b.Fatal(err)
		}
		s.slots[set.Chunk.Slot()] = v
		s.bytes += len(v) + 14 // hc/<streamID><aggregator><slot>
	}
	for _, slot := range set.DeletedSlots {
		delete(s.slots, slot)
		s.bytes += 14
	}
}

// benchRingRound runs one round: decode the header, read what appending needs,
// append, persist. Exactly the work the plugin will do per pair.
func benchRingRound(b *testing.B, s *benchRingStore, required uint32, ts uint64, value StreamValue) {
	b.Helper()
	var header *StreamHistoryHeader
	if len(s.header) > 0 {
		var err error
		if header, err = UnmarshalStreamHistoryHeader(s.header); err != nil {
			b.Fatal(err)
		}
	}
	w := NewRingWindow(header)
	if _, err := w.SetRequiredCount(required); err != nil {
		b.Fatal(err)
	}
	for _, sequence := range w.AppendPlan() {
		chunk, err := UnmarshalStreamHistoryChunk(s.slots[HistoryChunkSlot(sequence)])
		if err != nil {
			b.Fatal(err)
		}
		if err := w.Provide(chunk); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := w.Append(ts, value); err != nil {
		b.Fatal(err)
	}
	s.apply(b, w.WriteSet())
}

func warmBenchRing(b *testing.B, required uint32, rounds int) *benchRingStore {
	b.Helper()
	s := &benchRingStore{slots: map[uint32][]byte{}}
	for i := 1; i <= rounds; i++ {
		benchRingRound(b, s, required, uint64(i)*1_000_000_000, benchRingQuote(i))
	}
	return s
}

// BenchmarkRingAppendRound measures one round's append for one pair at a
// settled window: the cost the plugin pays every round, per pair.
func BenchmarkRingAppendRound(b *testing.B) {
	for _, depth := range []uint32{100, 300, MaxHistoryRecordsPerPair} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			warm := warmBenchRing(b, depth, int(depth)+MaxHistoryChunkRecords)
			ts := uint64(int(depth)+MaxHistoryChunkRecords) * 1_000_000_000

			b.ResetTimer()
			s := &benchRingStore{header: warm.header, slots: warm.slots}
			for i := range b.N {
				ts += 1_000_000_000
				benchRingRound(b, s, depth, ts, benchRingQuote(i))
			}
			b.StopTimer()

			b.ReportMetric(float64(s.bytes)/float64(b.N), "kvBytes/op")
		})
	}
}

// BenchmarkRingNewest measures the read side: decoding a full window back into
// records, which is the cost the chunking does not remove.
func BenchmarkRingNewest(b *testing.B) {
	for _, depth := range []uint32{100, 300, MaxHistoryRecordsPerPair} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			s := warmBenchRing(b, depth, int(depth)+MaxHistoryChunkRecords)

			b.ResetTimer()
			reads := 0
			for range b.N {
				header, err := UnmarshalStreamHistoryHeader(s.header)
				if err != nil {
					b.Fatal(err)
				}
				w := NewRingWindow(header)
				plan, err := w.ReadPlan(depth)
				if err != nil {
					b.Fatal(err)
				}
				for _, sequence := range plan {
					chunk, err := UnmarshalStreamHistoryChunk(s.slots[HistoryChunkSlot(sequence)])
					if err != nil {
						b.Fatal(err)
					}
					if err := w.Provide(chunk); err != nil {
						b.Fatal(err)
					}
					reads++
				}
				if _, err := w.Newest(depth); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(reads)/float64(b.N), "chunkReads/op")
		})
	}
}
