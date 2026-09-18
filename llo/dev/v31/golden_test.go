package llo

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Golden tests freeze the wire format of the two records the plugin cannot
// change unilaterally: the precursor StateTransition hands to Reports, and the
// KeyValueState records every oracle replicates. A format change that is not
// accompanied by a deliberate golden update diverges oracles rather than
// failing loudly, so the bytes are asserted directly.
//
// Regenerate with: GOLDEN_UPDATE=1 go test ./llo/dev/v31/ -run Golden

const goldenDir = "testdata/golden"

// goldenBytes compares got against the committed golden file, or rewrites the
// file when GOLDEN_UPDATE is set.
func goldenBytes(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	if os.Getenv("GOLDEN_UPDATE") != "" {
		require.NoError(t, os.WriteFile(path, got, 0o600))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "golden file not found; run with GOLDEN_UPDATE=1 to generate")
	require.Equal(t, hex.EncodeToString(want), hex.EncodeToString(got), "encoding of %s changed; every oracle must agree on these bytes", name)
}

// goldenPrecursor is the fully populated precursor the golden cases encode. It
// exercises every field, both stream value types, and out-of-order map keys so
// that the sorting the encoder does is part of what is frozen.
func goldenPrecursor() precursor {
	return precursor{
		LifeCycleStage:                  llotypes.LifeCycleStage("production"),
		ObservationTimestampNanoseconds: 1_700_000_000_000_000_000,
		ChannelStateSeqNr:               42,
		ChannelDefinitions: llotypes.ChannelDefinitions{
			3: {
				ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
				Streams:      []llotypes.Stream{{StreamID: 300, Aggregator: llotypes.AggregatorQuote}},
				Opts:         []byte(`{"baseUSDFee":"0.1"}`),
			},
			1: {
				ReportFormat: llotypes.ReportFormatJSON,
				Streams:      []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}, {StreamID: 200, Aggregator: llotypes.AggregatorMode}},
				Opts:         []byte(`{"foo":"bar"}`),
			},
		},
		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{3: 30, 1: 10},
		StreamAggregates: protocol.StreamAggregates{
			300: {llotypes.AggregatorQuote: &protocol.Quote{
				Bid:       decimal.NewFromInt(1010),
				Benchmark: decimal.NewFromInt(1011),
				Ask:       decimal.NewFromInt(1012),
			}},
			100: {
				llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(123)),
				llotypes.AggregatorMode:   protocol.ToDecimal(decimal.NewFromInt(124)),
			},
		},
		SupportByFormat: map[llotypes.ReportFormat]int{
			llotypes.ReportFormatEVMPremiumLegacy: 3,
			llotypes.ReportFormatJSON:             4,
		},
	}
}

func Test_Golden_Precursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    precursor
	}{
		{"precursor_empty.bin", precursor{}},
		{"precursor_full.bin", goldenPrecursor()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := encodePrecursor(tc.p)
			require.NoError(t, err)
			goldenBytes(t, tc.name, b)

			// The golden bytes must also decode back to the same projection, so
			// an accidental encode/decode asymmetry cannot hide behind a
			// regenerated file.
			want, err := os.ReadFile(filepath.Join(goldenDir, tc.name))
			require.NoError(t, err)
			got, err := decodePrecursor(want)
			require.NoError(t, err)
			require.Equal(t, normalizePrecursor(tc.p), got)
		})
	}
}

// normalizePrecursor fills in the empty maps decodePrecursor always returns, so
// that a nil-map input compares equal to its decoded form.
func normalizePrecursor(p precursor) precursor {
	if p.ChannelDefinitions == nil {
		p.ChannelDefinitions = llotypes.ChannelDefinitions{}
	}
	if p.ValidAfterNanoseconds == nil {
		p.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{}
	}
	if p.StreamAggregates == nil {
		p.StreamAggregates = protocol.StreamAggregates{}
	}
	return p
}

func Test_Golden_KVRecords(t *testing.T) {
	p := goldenPrecursor()
	kv := newMemKV()

	require.NoError(t, writeLifecycle(kv, p.LifeCycleStage))
	require.NoError(t, writeChannelState(kv, p.ChannelStateSeqNr, p.ChannelDefinitions))
	require.NoError(t, writeHotState(kv,
		p.ObservationTimestampNanoseconds,
		p.ValidAfterNanoseconds,
		map[llotypes.ChannelID]bool{3: true, 1: false, 2: true},
		map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{
			300: {llotypes.AggregatorQuote: {ObservedAtNanoseconds: 3, StreamValue: &protocol.Quote{
				Bid:       decimal.NewFromInt(1010),
				Benchmark: decimal.NewFromInt(1011),
				Ask:       decimal.NewFromInt(1012),
			}}},
			100: {llotypes.AggregatorMedian: {ObservedAtNanoseconds: 1, StreamValue: protocol.ToDecimal(decimal.NewFromInt(123))}},
		},
		logger.Test(t),
	))
	require.NoError(t, writeHistoryLayoutVersion(kv))
	require.NoError(t, writeHistoryIndex(kv, []histKey{
		{streamID: 100, aggregator: llotypes.AggregatorMedian},
		{streamID: 300, aggregator: llotypes.AggregatorQuote},
	}))

	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{"kv_lifecycle.bin", keyLifecycle},
		{"kv_channel_state.bin", keyChannelState},
		{"kv_channel_seqnr.bin", keyChannelSeqNr},
		{"kv_hot_state.bin", keyHotState},
		{"kv_history_version.bin", keyHistoryVersion},
		{"kv_history_index.bin", keyHistoryIndex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, ok := kv.m[string(tc.key)]
			require.True(t, ok, "record %s was not written", tc.key)
			goldenBytes(t, tc.name, b)
		})
	}

	// The records must load back into the same projection the writers were
	// handed.
	s, err := loadKVState(kv, nil)
	require.NoError(t, err)
	require.Equal(t, p.LifeCycleStage, s.lifeCycleStage)
	require.Equal(t, p.ChannelStateSeqNr, s.channelStateSeqNr)
	require.Equal(t, p.ChannelDefinitions, s.channelDefinitions)
	require.Equal(t, p.ObservationTimestampNanoseconds, s.observationTimestampNs)
	require.Equal(t, p.ValidAfterNanoseconds, s.validAfterNanoseconds)
	require.Equal(t, map[llotypes.ChannelID]bool{3: true, 2: true}, s.reportedLastRound)
	require.Len(t, s.carryForward, 2)

	version, err := readHistoryLayoutVersion(kv)
	require.NoError(t, err)
	require.Equal(t, historyLayoutVersion, version)
	index, err := readHistoryIndex(kv)
	require.NoError(t, err)
	require.Equal(t, []histKey{
		{streamID: 100, aggregator: llotypes.AggregatorMedian},
		{streamID: 300, aggregator: llotypes.AggregatorQuote},
	}, index)
}

func Test_Golden_KVHistoryRecords(t *testing.T) {
	const (
		sid = llotypes.StreamID(100)
		agg = llotypes.AggregatorMedian
	)

	kv := newMemKV()

	// One append per round, each round re-reading the stored window: that is the
	// only way to grow a chunk past a single record, and it exercises the stored
	// form rather than an in-memory shortcut.
	var set protocol.RingWriteSet
	for i := 1; i <= 3; i++ {
		w := readHistory(t, kv, sid, agg)
		if w == nil {
			w = protocol.NewRingWindow(nil)
		}
		_, err := w.SetRequiredCount(4)
		require.NoError(t, err)
		appended, err := w.Append(uint64(i)*1_000, protocol.ToDecimal(decimal.NewFromInt(int64(i))))
		require.NoError(t, err)
		require.True(t, appended)

		set = w.WriteSet()
		require.NotNil(t, set.Header)
		require.NotNil(t, set.Chunk)
		_, err = writeHistoryHeader(kv, sid, agg, set.Header)
		require.NoError(t, err)
		_, err = writeHistoryChunk(kv, sid, agg, set.Chunk)
		require.NoError(t, err)
	}

	goldenBytes(t, "kv_history_header.bin", kv.m[string(historyHeaderKey(sid, agg))])
	goldenBytes(t, "kv_history_chunk.bin", kv.m[string(historyChunkKey(sid, agg, set.Chunk.Slot()))])

	header, err := readHistoryHeader(kv, sid, agg)
	require.NoError(t, err)
	require.Equal(t, set.Header.Sequences(), header.Sequences())
	require.Equal(t, set.Header.Counts(), header.Counts())
	chunk, err := readHistoryChunk(kv, sid, agg, set.Chunk.Slot())
	require.NoError(t, err)
	require.Len(t, chunk.Records(), 3)
}

// Test_Golden_KVKeys freezes the key layout. Keys are part of the replicated
// schema too: renaming one silently orphans the stored value on every node.
func Test_Golden_KVKeys(t *testing.T) {
	for _, tc := range []struct {
		want string
		key  string
	}{
		{"c/lifecycle", string(keyLifecycle)},
		{"c/defs", string(keyChannelState)},
		{"c/seqnr", string(keyChannelSeqNr)},
		{"r/agg", string(keyHotState)},
		{"hidx", string(keyHistoryIndex)},
		{"hv", string(keyHistoryVersion)},
	} {
		require.Equal(t, tc.want, tc.key)
	}
	require.Equal(t, "68682f0000006400000001", hex.EncodeToString(historyHeaderKey(100, llotypes.AggregatorMedian)))
	require.Equal(t, "68632f000000640000000100000007", hex.EncodeToString(historyChunkKey(100, llotypes.AggregatorMedian, 7)))
}
