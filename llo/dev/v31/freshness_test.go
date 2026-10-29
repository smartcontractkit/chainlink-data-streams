package llo

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

func Test_Freshness_Constants(t *testing.T) {
	require.Greater(t, MaxStreamValueAgeRounds, SnapshotReuseRounds, "the last reuse round must still be fresh")
	require.Greater(t, agreedTimestampWindow, MaxStreamValueAgeRounds, "every fresh cycle start must resolve in the window")
}

func Test_appendAgreedTimestamp(t *testing.T) {
	var window []uint64
	for ts := uint64(1); ts <= agreedTimestampWindow+2; ts++ {
		prev := append([]uint64(nil), window...)
		window = appendAgreedTimestamp(window, ts)
		require.Equal(t, prev, prev[:len(prev):len(prev)], "the previous window is not modified")
		require.LessOrEqual(t, len(window), agreedTimestampWindow)
		require.Equal(t, ts, window[len(window)-1], "the newest entry is last")
		for i := 1; i < len(window); i++ {
			require.Equal(t, window[i-1]+1, window[i], "entries are consecutive")
		}
	}
	require.Len(t, window, agreedTimestampWindow)
}

func Test_agreedTimestampAt(t *testing.T) {
	// Window written by round 10: rounds 8, 9, 10.
	window := []uint64{800, 900, 1000}
	for at, want := range map[uint64]uint64{10: 1000, 9: 900, 8: 800} {
		got, ok := agreedTimestampAt(window, 10, at)
		require.True(t, ok, "round %d", at)
		require.Equal(t, want, got, "round %d", at)
	}
	_, ok := agreedTimestampAt(window, 10, 7)
	require.False(t, ok, "before the window")
	_, ok = agreedTimestampAt(window, 10, 11)
	require.False(t, ok, "after the window")
	_, ok = agreedTimestampAt(nil, 10, 10)
	require.False(t, ok, "empty window")
}

func Test_roundIntervalEstimate(t *testing.T) {
	sec := uint64(time.Second)
	t.Run("median of the intervals", func(t *testing.T) {
		window := []uint64{0, sec, 2 * sec, 2*sec + sec/2, 4 * sec}
		require.Equal(t, time.Second, roundIntervalEstimate(window, time.Second))
	})
	t.Run("zero intervals are skipped", func(t *testing.T) {
		window := []uint64{sec, sec, sec, 3 * sec}
		require.Equal(t, 2*time.Second, roundIntervalEstimate(window, 0))
	})
	t.Run("floored at the configured interval", func(t *testing.T) {
		window := []uint64{0, sec / 4, sec / 2}
		require.Equal(t, time.Second, roundIntervalEstimate(window, time.Second))
	})
	t.Run("not capped by the configured interval", func(t *testing.T) {
		// DeltaRound is a lower bound on the round interval, not its cadence.
		window := []uint64{0, 5 * sec, 10 * sec}
		require.Equal(t, 5*time.Second, roundIntervalEstimate(window, 250*time.Millisecond))
	})
	t.Run("a stall does not move the median", func(t *testing.T) {
		window := []uint64{0, sec, 2 * sec, 3 * sec, 63 * sec}
		require.Equal(t, time.Second, roundIntervalEstimate(window, 0))
	})
	t.Run("unknown while fewer than two timestamps advance", func(t *testing.T) {
		require.Equal(t, time.Duration(0), roundIntervalEstimate(nil, 0))
		require.Equal(t, time.Duration(0), roundIntervalEstimate([]uint64{sec}, 0))
		require.Equal(t, time.Duration(0), roundIntervalEstimate([]uint64{sec, sec, sec}, 0))
		require.Equal(t, time.Duration(0), roundIntervalEstimate(nil, time.Second), "the floor applies to a measurement only")
	})
}

func Test_selectFreshContributions(t *testing.T) {
	val := func(i int64) protocol.StreamValue { return protocol.ToDecimal(decimal.NewFromInt(i)) }
	contribution := func(i int64, cycleStart uint64) streamContribution {
		return streamContribution{value: val(i), cycleStartSeqNr: cycleStart}
	}
	values := func(in []protocol.StreamValue) []int64 {
		out := make([]int64, 0, len(in))
		for _, sv := range in {
			out = append(out, sv.(*protocol.Decimal).Decimal().IntPart())
		}
		return out
	}

	const seqNr = uint64(20)
	const interval = time.Second
	sec := uint64(time.Second)
	// Window written by round 20, one second apart: rounds 13 to 20.
	window := make([]uint64, 0, agreedTimestampWindow)
	for i := range agreedTimestampWindow {
		window = append(window, uint64(100+i)*sec)
	}
	roundTs := window[len(window)-1]
	require.Len(t, window, agreedTimestampWindow)

	t.Run("all fresh", func(t *testing.T) {
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(3, 20), contribution(1, 19), contribution(2, 20-MaxStreamValueAgeRounds)}}
		out, stats := selectFreshContributions(in, seqNr, roundTs, window, interval, 3)
		require.Equal(t, []int64{3, 1, 2}, values(out[1]), "input order is kept")
		require.False(t, stats.any())
	})

	t.Run("stale by rounds is left out when enough are fresh", func(t *testing.T) {
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 20), contribution(9, 20-MaxStreamValueAgeRounds-1), contribution(2, 20), contribution(3, 19)}}
		out, stats := selectFreshContributions(in, seqNr, roundTs, window, interval, 3)
		require.Equal(t, []int64{1, 2, 3}, values(out[1]))
		require.Equal(t, 1, stats.streamsFiltered)
		require.Equal(t, 0, stats.streamsFallenBack)
		require.Equal(t, 1, stats.contributionsStale)
		require.Equal(t, 1, stats.staleByRounds)
	})

	t.Run("stale by time", func(t *testing.T) {
		// The cycle's round is in the window and recent in rounds, but this
		// round's agreed timestamp jumped far ahead of it: a stall. The window
		// ends with this round's timestamp.
		stalled := roundTs + uint64(MaxStreamValueAgeRounds)*sec + 1
		stalledWindow := append(append([]uint64(nil), window[:len(window)-1]...), stalled)
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 20), contribution(2, 20), contribution(3, 20), contribution(9, 19)}}
		out, stats := selectFreshContributions(in, seqNr, stalled, stalledWindow, interval, 3)
		require.Equal(t, []int64{1, 2, 3}, values(out[1]), "this round's own cycle has age zero and stays")
		require.Equal(t, 1, stats.staleByTime)
	})

	t.Run("no time bound while the interval is unknown", func(t *testing.T) {
		stalled := roundTs + uint64(MaxStreamValueAgeRounds)*sec + 1
		stalledWindow := append(append([]uint64(nil), window[:len(window)-1]...), stalled)
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 20), contribution(2, 20), contribution(3, 20), contribution(9, 19)}}
		_, stats := selectFreshContributions(in, seqNr, stalled, stalledWindow, 0, 3)
		require.False(t, stats.any())
	})

	t.Run("exactly at the bound is fresh", func(t *testing.T) {
		atBound := roundTs + uint64(MaxStreamValueAgeRounds)*sec
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 20), contribution(2, 20), contribution(3, 20), contribution(9, 20)}}
		_, stats := selectFreshContributions(in, seqNr, atBound, window, interval, 3)
		require.False(t, stats.any())
	})

	t.Run("before the window", func(t *testing.T) {
		short := window[len(window)-2:] // rounds 19 and 20 only
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 20), contribution(2, 20), contribution(3, 20), contribution(9, 18)}}
		out, stats := selectFreshContributions(in, seqNr, roundTs, short, interval, 3)
		require.Equal(t, []int64{1, 2, 3}, values(out[1]))
		require.Equal(t, 1, stats.staleBeforeWindow)
	})

	t.Run("future cycle start is stale", func(t *testing.T) {
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 20), contribution(2, 20), contribution(3, 20), contribution(9, 21)}}
		out, stats := selectFreshContributions(in, seqNr, roundTs, window, interval, 3)
		require.Equal(t, []int64{1, 2, 3}, values(out[1]))
		require.Equal(t, 1, stats.staleByRounds)
	})

	t.Run("falls back to contributions stale by rounds below min contributions", func(t *testing.T) {
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 20), contribution(9, 1), contribution(2, 20)}}
		out, stats := selectFreshContributions(in, seqNr, roundTs, window, interval, 3)
		require.Equal(t, []int64{1, 9, 2}, values(out[1]), "every contribution, in input order")
		require.Equal(t, 0, stats.streamsFiltered)
		require.Equal(t, 1, stats.streamsFallenBack)
		require.Equal(t, 1, stats.contributionsStale)
	})

	t.Run("stale by time is never re-admitted", func(t *testing.T) {
		// A stall: every contribution comes from before the timestamp jump.
		stalled := roundTs + uint64(MaxStreamValueAgeRounds)*sec + 1
		stalledWindow := append(append([]uint64(nil), window[:len(window)-1]...), stalled)
		in := map[llotypes.StreamID][]streamContribution{1: {contribution(1, 19), contribution(2, 19), contribution(3, 19), contribution(4, 18)}}
		out, stats := selectFreshContributions(in, seqNr, stalled, stalledWindow, interval, 3)
		require.Empty(t, out[1], "no fresh contribution and nothing to fall back to")
		require.Equal(t, 1, stats.streamsFiltered)
		require.Equal(t, 0, stats.streamsFallenBack)
		require.Equal(t, 4, stats.staleByTime)
	})

	t.Run("per stream", func(t *testing.T) {
		in := map[llotypes.StreamID][]streamContribution{
			1: {contribution(1, 20), contribution(2, 20), contribution(3, 20), contribution(9, 1)},
			2: {contribution(1, 20), contribution(9, 1), contribution(8, 1)},
			3: {contribution(1, 20), contribution(2, 20)},
		}
		out, stats := selectFreshContributions(in, seqNr, roundTs, window, interval, 3)
		require.Equal(t, []int64{1, 2, 3}, values(out[1]))
		require.Equal(t, []int64{1, 9, 8}, values(out[2]))
		require.Equal(t, []int64{1, 2}, values(out[3]))
		require.Equal(t, 1, stats.streamsFiltered)
		require.Equal(t, 1, stats.streamsFallenBack)
		require.Equal(t, 3, stats.contributionsStale)
	})
}

// Test_fetchBlobsAndDecodeObservation_CycleStartSeqNr covers the deterministic blob checks:
// a payload without a cycle start, handles from different cycles, and a cycle
// start ahead of the round.
func Test_fetchBlobsAndDecodeObservation_CycleStartSeqNr(t *testing.T) {
	ctx := t.Context()
	bc := newFakeBroadcaster()
	sv := protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(1))}
	handleFor := func(cycleStart uint64) []byte {
		payload, err := marshalStreamValues(sv, cycleStart)
		require.NoError(t, err)
		handle, err := bc.BroadcastBlob(ctx, payload, ocr3_1types.BlobExpirationHintSequenceNumber{SeqNr: 100})
		require.NoError(t, err)
		handleBytes, err := handle.MarshalBinary()
		require.NoError(t, err)
		return handleBytes
	}

	t.Run("round trip", func(t *testing.T) {
		enc, err := encodeObservation(Observation{ObservationTimestampNanoseconds: 1}, [][]byte{handleFor(7)})
		require.NoError(t, err)
		memo := newBlobPayloadCache().round(9)
		decoded, err := fetchBlobsAndDecodeObservation(ctx, enc, bc, memo)
		require.NoError(t, err)
		require.EqualValues(t, 7, decoded.CycleStartSeqNr)
		// A memo hit carries the same cycle start.
		decoded, err = fetchBlobsAndDecodeObservation(ctx, enc, bc, memo)
		require.NoError(t, err)
		require.EqualValues(t, 7, decoded.CycleStartSeqNr)
	})

	t.Run("no stream values no cycle start", func(t *testing.T) {
		enc, err := encodeObservation(Observation{ObservationTimestampNanoseconds: 1}, nil)
		require.NoError(t, err)
		decoded, err := fetchBlobsAndDecodeObservation(ctx, enc, bc, nil)
		require.NoError(t, err)
		require.Zero(t, decoded.CycleStartSeqNr)
	})

	t.Run("zero cycle start is malformed", func(t *testing.T) {
		enc, err := encodeObservation(Observation{ObservationTimestampNanoseconds: 1}, [][]byte{handleFor(0)})
		require.NoError(t, err)
		_, err = fetchBlobsAndDecodeObservation(ctx, enc, bc, nil)
		require.ErrorContains(t, err, "carries no cycle start seqNr")
	})

	t.Run("handles from different cycles", func(t *testing.T) {
		enc, err := encodeObservation(Observation{ObservationTimestampNanoseconds: 1}, [][]byte{handleFor(7), handleFor(8)})
		require.NoError(t, err)
		_, err = fetchBlobsAndDecodeObservation(ctx, enc, bc, nil)
		require.ErrorContains(t, err, "blobs from different cycles")
	})

	t.Run("cycle start ahead of the round", func(t *testing.T) {
		p := testPlugin(t)
		enc, err := encodeObservation(Observation{ObservationTimestampNanoseconds: 1, SupportedReportFormats: testSupportedReportFormats}, [][]byte{handleFor(7)})
		require.NoError(t, err)
		_, err = p.fetchBlobsDecodeAndValidateObservation(ctx, 6, ao(0, enc), bc)
		require.ErrorContains(t, err, "gathered for a future round")
		_, err = p.fetchBlobsDecodeAndValidateObservation(ctx, 7, ao(0, enc), bc)
		require.NoError(t, err)
	})
}

// Test_StateTransition_Freshness drives the rule through whole rounds: a
// lagging oracle's values are left out while enough fresh ones remain, every
// contribution is aggregated when they do not, and a stall rejects the values
// gathered before it for one round.
func Test_StateTransition_Freshness(t *testing.T) {
	ctx := t.Context()
	p := testPlugin(t)
	p.MinRoundInterval = time.Second
	kv := newMemKV()

	const sec = uint64(time.Second)
	median := func(t *testing.T, precBytes ocr3_1types.ReportsPlusPrecursor) int64 {
		t.Helper()
		prec, err := decodePrecursor(precBytes)
		require.NoError(t, err)
		agg, ok := prec.StreamAggregates[100][llotypes.AggregatorMedian]
		require.True(t, ok, "stream 100 must aggregate")
		return agg.(*protocol.Decimal).Decimal().IntPart()
	}
	obs := func(i int, ts uint64, value int64, cycleStart uint64) ocrtypes.AttributedObservation {
		return ao(i, mustEncodeObs(t, Observation{
			ObservationTimestampNanoseconds: ts,
			StreamValues:                    protocol.StreamValues{100: protocol.ToDecimal(decimal.NewFromInt(value))},
			CycleStartSeqNr:                 cycleStart,
		}))
	}

	// Rounds 1 to 3: bootstrap and install the channel, one second apart.
	_, err := p.StateTransition(ctx, 1, ocrtypes.AttributedQuery{}, emptyRound(), kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 2, ocrtypes.AttributedQuery{}, addChannelRound(t, 2*sec, 1, jsonChannel()), kv, testBlobs)
	require.NoError(t, err)
	_, err = p.StateTransition(ctx, 3, ocrtypes.AttributedQuery{}, addChannelRound(t, 3*sec, 1, jsonChannel()), kv, testBlobs)
	require.NoError(t, err)

	// Round 4: three fresh contributions and one from a cycle that started
	// before the window. Without the rule the median of {10, 12, 14, 1000} is
	// 14; with it, the median of the fresh three is 12.
	prec, err := p.StateTransition(ctx, 4, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{
		obs(0, 4*sec, 10, 4), obs(1, 4*sec, 12, 3), obs(2, 4*sec, 14, 4), obs(3, 4*sec, 1000, 1),
	}, kv, testBlobs)
	require.NoError(t, err)
	require.EqualValues(t, 12, median(t, prec))

	// Round 5: only two fresh contributions, below the floor of three, so
	// every contribution is aggregated: the median of {10, 14, 1000} is 14.
	prec, err = p.StateTransition(ctx, 5, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{
		obs(0, 5*sec, 10, 5), obs(1, 5*sec, 14, 5), obs(3, 5*sec, 1000, 1),
	}, kv, testBlobs)
	require.NoError(t, err)
	require.EqualValues(t, 14, median(t, prec))

	// Round 6 after a stall: the agreed timestamp jumps a minute, and every
	// contribution comes from a cycle started in round 5, before the stall.
	// All are stale by time and none is re-admitted: the stream has no
	// aggregate this round.
	prec, err = p.StateTransition(ctx, 6, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{
		obs(0, 65*sec, 10, 5), obs(1, 65*sec, 12, 5), obs(2, 65*sec, 1000, 5), obs(3, 65*sec, 1000, 5),
	}, kv, testBlobs)
	require.NoError(t, err)
	prec6, err := decodePrecursor(prec)
	require.NoError(t, err)
	_, aggregated := prec6.StreamAggregates[100][llotypes.AggregatorMedian]
	require.False(t, aggregated, "pre-stall values must not be reported under the post-stall timestamp")

	// Round 7: cycles started after the stall are fresh again, and the one
	// still carrying round 5 is left out.
	prec, err = p.StateTransition(ctx, 7, ocrtypes.AttributedQuery{}, []ocrtypes.AttributedObservation{
		obs(0, 66*sec, 10, 7), obs(1, 66*sec, 12, 6), obs(2, 66*sec, 14, 7), obs(3, 66*sec, 1000, 5),
	}, kv, testBlobs)
	require.NoError(t, err)
	require.EqualValues(t, 12, median(t, prec))

	// The window persisted with the round.
	state, err := loadKVState(kv, p.ChannelCache, p.Logger)
	require.NoError(t, err)
	require.Equal(t, []uint64{2 * sec, 3 * sec, 4 * sec, 5 * sec, 65 * sec, 66 * sec}, state.recentAgreedTimestamps)
}
