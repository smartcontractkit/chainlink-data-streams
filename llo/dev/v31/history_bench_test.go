package llo

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Stream history benchmarks at the caps.
//
// The caps were set from measured sizes; these measure time, which nothing had
// yet. The number to compare against is the round interval (on the order of a
// second): history work happens inside StateTransition, so anything approaching
// that is a problem regardless of how many bytes it writes.
//
// The kvBytes metric is reported alongside ns/op because the per-round byte
// budget, not CPU, is what the design expects to bind first.

const (
	benchPairs  = protocol.MaxHistoryPairs
	benchDepth  = protocol.MaxHistoryRecordsPerPair
	benchAggreg = llotypes.AggregatorMedian
)

// benchQuote is the largest realistic record: one window serves _bid/_ask/
// _benchmark, so quote streams are the case worth measuring.
func benchQuote(i int) protocol.StreamValue {
	return &protocol.Quote{
		Bid:       decimal.New(int64(110000000000000000+i), -8),
		Benchmark: decimal.New(int64(110000000000000001+i), -8),
		Ask:       decimal.New(int64(110000000000000002+i), -8),
	}
}

// benchState returns a store pre-populated with benchPairs full windows.
func benchState(tb testing.TB, pairs, depth int) *memKV {
	tb.Helper()
	kv := newMemKV()

	keys := make([]histKey, 0, pairs)
	for p := range pairs {
		key := histKey{streamID: llotypes.StreamID(p + 1), aggregator: benchAggreg}
		keys = append(keys, key)

		// Built in one pass rather than round by round: the window is kept in
		// memory and each chunk is persisted as it seals, which is exactly what
		// a run of rounds would have written.
		w := protocol.NewRingWindow(nil)
		_, err := w.SetRequiredCount(uint32(depth))
		require.NoError(tb, err)

		// Half a chunk past the required depth, so the benchmark measures a
		// settled window with a half-full newest chunk rather than the boundary
		// case where the next append happens to start a fresh, nearly empty one.
		records := depth + protocol.MaxHistoryChunkRecords/2
		for i := 1; i <= records; i++ {
			appended, err := w.Append(uint64(i)*uint64(1_000_000_000), benchQuote(i))
			require.NoError(tb, err)
			require.True(tb, appended)

			set := w.WriteSet()
			if set.Chunk.Len() == protocol.MaxHistoryChunkRecords || i == records {
				_, err := writeHistoryChunk(kv, key.streamID, key.aggregator, set.Chunk)
				require.NoError(tb, err)
			}
			for _, slot := range set.DeletedSlots {
				require.NoError(tb, deleteHistoryChunk(kv, key.streamID, key.aggregator, slot))
			}
		}
		_, err = writeHistoryHeader(kv, key.streamID, key.aggregator, w.WriteSet().Header)
		require.NoError(tb, err)
	}
	require.NoError(tb, writeHistoryIndex(kv, keys))
	require.NoError(tb, writeHistoryLayoutVersion(kv))
	return kv
}

func benchRequirements(pairs, depth int) historyRequirements {
	depths := make(map[histKey]uint32, pairs)
	for p := range pairs {
		depths[histKey{streamID: llotypes.StreamID(p + 1), aggregator: benchAggreg}] = uint32(depth)
	}
	return historyRequirements{depths: depths}
}

// BenchmarkHistoryStore_LoadAll measures decoding every window once, which is
// what a round costs before it has done any work.
func BenchmarkHistoryStore_LoadAll(b *testing.B) {
	kv := benchState(b, benchPairs, benchDepth)
	lggr := logger.Test(b)

	b.ResetTimer()
	for range b.N {
		store, err := newHistoryStore(kv, lggr)
		if err != nil {
			b.Fatal(err)
		}
		for p := range benchPairs {
			if _, err := store.Load(llotypes.StreamID(p+1), benchAggreg); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkHistoryStore_Round measures a full round's history work: load the
// index, set requirements, append one value per pair, flush.
func BenchmarkHistoryStore_Round(b *testing.B) {
	for _, tc := range []struct{ pairs, depth int }{
		{1, benchDepth},
		{16, benchDepth},
		{benchPairs, 300},
		{benchPairs, benchDepth},
	} {
		b.Run(fmt.Sprintf("pairs=%d/depth=%d", tc.pairs, tc.depth), func(b *testing.B) {
			base := benchState(b, tc.pairs, tc.depth)
			requirements := benchRequirements(tc.pairs, tc.depth)
			lggr := logger.Test(b)

			var bytesWritten int
			b.ResetTimer()
			for n := range b.N {
				// A fresh writer per iteration so the measured writes are one
				// round's worth, not cumulative.
				kv := &countingWriter{memKV: base}

				store, err := newHistoryStore(kv, lggr)
				if err != nil {
					b.Fatal(err)
				}
				if err := requirements.apply(store); err != nil {
					b.Fatal(err)
				}
				ts := uint64(tc.depth+protocol.MaxHistoryChunkRecords/2+n+1) * uint64(1_000_000_000)
				for p := range tc.pairs {
					if _, err := store.Append(llotypes.StreamID(p+1), benchAggreg, ts, benchQuote(n)); err != nil {
						b.Fatal(err)
					}
				}
				if err := store.Flush(kv); err != nil {
					b.Fatal(err)
				}
				bytesWritten = kv.bytes
			}
			b.ReportMetric(float64(bytesWritten), "kvBytes/op")
			b.ReportMetric(float64(bytesWritten)/1024, "kvKiB/op")
		})
	}
}

// BenchmarkComputeHistoryRequirements measures deriving the required depths from
// the channel definitions, which happens once per round.
func BenchmarkComputeHistoryRequirements(b *testing.B) {
	for _, channels := range []int{1, 16, 128} {
		b.Run(fmt.Sprintf("channels=%d", channels), func(b *testing.B) {
			defs := llotypes.ChannelDefinitions{}
			for c := range channels {
				streamID := llotypes.StreamID(c + 1)
				defs[llotypes.ChannelID(c+1)] = llotypes.ChannelDefinition{
					ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
					Streams:      []llotypes.Stream{{StreamID: streamID, Aggregator: benchAggreg}},
					Opts: []byte(fmt.Sprintf(
						`{"abi":[{"type":"int256","expression":"Avg(History(s%d, 300))","expressionStreamID":%d}]}`,
						streamID, 900+c)),
				}
			}
			cache := protocol.NewOptsCache()
			cache.ResetTo(defs)
			lggr := logger.Test(b)

			// Sanity check outside the timed loop, and a note on what it shows:
			// at 128 channels each wanting a 300-deep window every pair is
			// admitted. Under the single-blob layout only 109 were — a pair cost
			// depth * MaxHistoryRecordBytes per round and the byte budget ran out
			// first. Chunking made that cost independent of depth, so the pair
			// cap is now the only thing that can deny a window.
			admitted := len(computeHistoryRequirements(defs, cache, lggr).depths)
			b.ReportMetric(float64(admitted), "admittedPairs")

			b.ResetTimer()
			for range b.N {
				computeHistoryRequirements(defs, cache, lggr)
			}
		})
	}
}

// countingWriter counts bytes written, so a round's byte cost can be reported
// against the per-round budget. Reads fall through to the shared base state.
type countingWriter struct {
	*memKV
	bytes int
}

func (w *countingWriter) Write(key, value []byte) error {
	w.bytes += len(key) + len(value)
	// Deliberately not persisted: the benchmark measures one round against a
	// fixed starting state, and mutating it would make iterations diverge.
	return nil
}

func (w *countingWriter) Delete([]byte) error { return nil }
