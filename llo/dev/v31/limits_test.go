package llo

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
)

// The declared limits in factory.go are only honest if the plugin's own bounds
// keep what it produces underneath them. These tests hold that arithmetic, so
// that raising one of the protocol constants fails the build rather than the
// round: libocr rejects an oversized write or message, and a rejected write
// fails the round for every oracle at once.

func TestLimits_BlobPayloadGuardMatchesDeclared(t *testing.T) {
	// The write-side guard and the declared limit must be the same number, or
	// the pump can broadcast a blob every peer's libocr rejects.
	require.Equal(t, ocr3_1types.MaxMaxBlobPayloadBytes, MaxBlobPayloadBytes)

	// The anti-bomb bound on the read side is deliberately looser; it bounds a
	// different quantity (decompressed bytes) and must not be mistaken for the
	// broadcast limit.
	require.Greater(t, maxDecompressedBlobPayloadBytes, MaxBlobPayloadBytes)
}

func TestLimits_BlobPayloadRejectsOversizedFraming(t *testing.T) {
	// Incompressible bytes above the declared limit: passes the raw check
	// (below maxDecompressedBlobPayloadBytes) but cannot be framed within the
	// declared payload limit.
	raw := make([]byte, MaxBlobPayloadBytes+1)
	// Deterministic pseudo-random bytes, so zstd cannot shrink the payload
	// below the limit and the framing check is the one that fires. A periodic
	// pattern would compress away and test nothing.
	lcg := uint64(1)
	for i := range raw {
		lcg = lcg*6364136223846793005 + 1442695040888963407
		raw[i] = byte(lcg >> 33)
	}
	require.Less(t, len(raw), maxDecompressedBlobPayloadBytes)

	_, err := encodeBlobPayload(raw)
	require.ErrorContains(t, err, "framed blob payload too large")
}

func TestLimits_HistoryFitsWriteBudgets(t *testing.T) {
	// One pair's window header plus newest chunk is a single value under one
	// key, so it must fit the per-key limit.
	require.LessOrEqual(t, protocol.MaxHistoryPairRoundBytes, ocr3_1types.MaxMaxKeyValueValueBytes)

	// Every pair rewrites its newest chunk and header each round, and that has
	// to leave room in the per-round write set for c/defs and r/agg.
	require.Less(t,
		protocol.MaxHistoryPairs*protocol.MaxHistoryPairRoundBytes,
		ocr3_1types.MaxMaxKeyValueModifiedKeysPlusValuesBytes,
		"history alone must not consume the per-round write budget")

	// The admission budget history is held to must itself fit the per-round
	// write set.
	require.Less(t, protocol.MaxHistoryTotalBytes, ocr3_1types.MaxMaxKeyValueModifiedKeysPlusValuesBytes)

	// Each pair occupies a bounded, statically known number of keys.
	require.Less(t,
		protocol.MaxHistoryPairs*protocol.MaxHistoryChunkSlots,
		ocr3_1types.MaxMaxKeyValueModifiedKeys)
}

// TestLimits_HotStateWorstCaseFitsPerKeyLimit builds the largest r/agg record
// the persisted-aggregate cap permits and measures it. The cap exists precisely
// to make this record bounded (see protocol.MaxPersistedAggregates); without a
// measurement the cap is only a count, and the byte limit libocr enforces is
// what actually fails the round.
func TestLimits_HotStateWorstCaseFitsPerKeyLimit(t *testing.T) {
	// An 18-digit price, the largest value a real feed produces. Note this is
	// the honest worst case, not the adversarial one: a decimal's coefficient
	// length is not yet bounded, so a byzantine value can still exceed this.
	// Bounding the coefficient is a separate, consensus-affecting change.
	price := decimal.RequireFromString("123456789012345.678")

	carry := make(map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue, protocol.MaxObservationStreamValuesLength)
	aggregators := []llotypes.Aggregator{llotypes.AggregatorMedian, llotypes.AggregatorMode}
	for i := range protocol.MaxObservationStreamValuesLength {
		sid := llotypes.StreamID(i)
		carry[sid] = make(map[llotypes.Aggregator]*protocol.TimestampedStreamValue, len(aggregators))
		for _, agg := range aggregators {
			carry[sid][agg] = &protocol.TimestampedStreamValue{
				ObservedAtNanoseconds: 1_700_000_000_000_000_000,
				StreamValue:           protocol.ToDecimal(price),
			}
		}
	}
	// Exactly the cap: every observed stream aggregated two ways.
	require.Equal(t, protocol.MaxPersistedAggregates, len(carry)*len(aggregators))

	validAfter := make(map[llotypes.ChannelID]uint64, protocol.MaxOutcomeChannelDefinitionsLength)
	reportable := make(map[llotypes.ChannelID]bool, protocol.MaxOutcomeChannelDefinitionsLength)
	for i := range protocol.MaxOutcomeChannelDefinitionsLength {
		validAfter[llotypes.ChannelID(i)] = 1_700_000_000_000_000_000
		reportable[llotypes.ChannelID(i)] = true
	}

	kv := newMemKV()
	require.NoError(t, writeHotState(kv, 1_700_000_000_000_000_000, validAfter, reportable, carry, logger.Test(t)))

	record, err := kv.Read(keyHotState)
	require.NoError(t, err)
	require.LessOrEqual(t, len(record), ocr3_1types.MaxMaxKeyValueValueBytes,
		"worst-case r/agg record (%d bytes) exceeds the per-key limit", len(record))
}

// TestLimits_HotStateTruncatesAboveCap covers the cap itself: above it, pairs
// are dropped in (streamID, aggregator) order so every oracle writes the same
// record, rather than every oracle writing an oversized one libocr rejects.
func TestLimits_HotStateTruncatesAboveCap(t *testing.T) {
	const over = 3
	tsv := func(ns uint64) *protocol.TimestampedStreamValue {
		return &protocol.TimestampedStreamValue{ObservedAtNanoseconds: ns, StreamValue: protocol.ToDecimal(decimal.NewFromInt(1))}
	}

	carry := make(map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue, protocol.MaxPersistedAggregates+over)
	for i := range protocol.MaxPersistedAggregates + over {
		carry[llotypes.StreamID(i)] = map[llotypes.Aggregator]*protocol.TimestampedStreamValue{
			llotypes.AggregatorMedian: tsv(uint64(i) + 1),
		}
	}

	kv := newMemKV()
	require.NoError(t, writeHotState(kv, 1, nil, nil, carry, logger.Test(t)))

	state := &kvState{
		channelDefinitions:    llotypes.ChannelDefinitions{},
		validAfterNanoseconds: map[llotypes.ChannelID]uint64{},
		reportedLastRound:     map[llotypes.ChannelID]bool{},
		carryForward:          map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{},
	}
	require.NoError(t, readHotState(kv, state))

	require.Len(t, state.carryForward, protocol.MaxPersistedAggregates)
	// The kept pairs are the lowest ones: truncation is after the sort, so the
	// choice is a function of the pair identities alone and is identical on
	// every oracle.
	_, keptLowest := state.carryForward[llotypes.StreamID(0)]
	require.True(t, keptLowest)
	for i := protocol.MaxPersistedAggregates; i < protocol.MaxPersistedAggregates+over; i++ {
		_, dropped := state.carryForward[llotypes.StreamID(i)]
		require.False(t, dropped, "pair %d should have been dropped", i)
	}
}

// TestLimits_KnownUnboundedInputs documents the bounds that do NOT yet exist,
// so the gap is visible next to the arithmetic that depends on it rather than
// only in a review note. Each of these is a real input to the precursor and the
// c/defs record, and each is bounded only by the checks listed here.
func TestLimits_KnownUnboundedInputs(t *testing.T) {
	// Channel opts are raw bytes with no length check on any path.
	require.NoError(t, protocol.VerifyChannelDefinitions(nil, llotypes.ChannelDefinitions{
		1: {
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			Opts:         []byte(strings.Repeat("x", 1<<20)),
		},
	}), "opts length is not yet bounded; see the factory limits derivation")

	// Total stream entries across the set are bounded only per channel
	// (MaxStreamsPerChannel) and by unique stream IDs
	// (MaxObservationStreamValuesLength), so the same stream repeated across
	// channels multiplies the entry count without tripping either.
	require.Greater(t,
		protocol.MaxStreamsPerChannel*protocol.MaxOutcomeChannelDefinitionsLength,
		protocol.MaxObservationStreamValuesLength,
		"total stream entries are not yet bounded; see the factory limits derivation")
}
