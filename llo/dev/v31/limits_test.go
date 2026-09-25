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

// TestLimits_DefinitionSetBudgetsAreAdmissionOnly pins the shape of the bounds
// the c/defs and precursor sizes depend on: they are enforced when a channel is
// admitted and NOT when it is already committed, because the committed path runs
// on every oracle every round and rejecting there halts the protocol. A
// grandfathered set therefore stays over budget, which is why these numbers
// bound what can be added rather than what can exist.
func TestLimits_DefinitionSetBudgetsAreAdmissionOnly(t *testing.T) {
	oversizedOpts := llotypes.ChannelDefinitions{
		1: {
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			Opts:         []byte(strings.Repeat("x", protocol.MaxChannelOptsBytes+1)),
		},
	}

	// Committed: accepted, so the round proceeds.
	require.NoError(t, protocol.VerifyChannelDefinitions(nil, oversizedOpts))

	// Being admitted: refused, so it never becomes committed in the first place.
	err := protocol.VerifyChannelDefinitionsForAdmission(nil, oversizedOpts, map[llotypes.ChannelID]struct{}{1: {}})
	require.ErrorContains(t, err, "opts that are too long")

	// The per-channel caps still do not bound total stream entries between
	// them; MaxTotalStreamEntries is the bound that does, and it is what the
	// definitions-record and precursor sizes are derived from.
	require.Greater(t,
		protocol.MaxStreamsPerChannel*protocol.MaxOutcomeChannelDefinitionsLength,
		protocol.MaxTotalStreamEntries)
}

// TestLimits_ChannelStateWorstCaseFitsPerKeyLimit builds the largest definition
// set the admission budgets permit and measures the record it marshals to. The
// budgets were sized from this number, so measuring it is what keeps them
// honest: c/defs is a single value under a single key, and libocr rejects a
// write above the per-key limit.
func TestLimits_ChannelStateWorstCaseFitsPerKeyLimit(t *testing.T) {
	const channels = protocol.MaxOutcomeChannelDefinitionsLength
	const streamsEach = protocol.MaxTotalStreamEntries / channels
	const optsEach = protocol.MaxTotalOptsBytes / channels

	defs := make(llotypes.ChannelDefinitions, channels)
	for c := range channels {
		streams := make([]llotypes.Stream, 0, streamsEach)
		for i := range streamsEach {
			streams = append(streams, llotypes.Stream{
				StreamID:   llotypes.StreamID(i + 1),
				Aggregator: llotypes.AggregatorMedian,
			})
		}
		defs[llotypes.ChannelID(c+1)] = llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			Streams:      streams,
			Opts:         []byte(strings.Repeat("x", optsEach)),
		}
	}
	// Every budget at its cap, so the record measured below is the worst case
	// admission permits rather than an arbitrary large set. Spreading the opts
	// budget evenly loses less than one byte per channel to integer division,
	// which is why this is a bound rather than an equality.
	require.Equal(t, protocol.MaxTotalStreamEntries, channels*streamsEach)
	require.LessOrEqual(t, channels*optsEach, protocol.MaxTotalOptsBytes)
	require.Greater(t, channels*optsEach, protocol.MaxTotalOptsBytes-channels)
	require.LessOrEqual(t, optsEach, protocol.MaxChannelOptsBytes)

	kv := newMemKV()
	require.NoError(t, writeChannelState(kv, 1, defs))
	record, err := kv.Read(keyChannelState)
	require.NoError(t, err)
	require.LessOrEqual(t, len(record), ocr3_1types.MaxMaxKeyValueValueBytes,
		"worst-case c/defs record (%d bytes) exceeds the per-key limit", len(record))
	t.Logf("worst-case c/defs record: %d bytes of the %d per-key limit",
		len(record), ocr3_1types.MaxMaxKeyValueValueBytes)
}

// Observation decode rejects an oversized coefficient (see
// protocol.UnmarshalObservedProtoStreamValue), which bounds everything written
// from an observation going forward.
func TestLimits_KnownUnboundedInputs(t *testing.T) {
	// A 1000-digit coefficient, well inside the permitted exponent range.
	huge := decimal.New(1, 0)
	for range 1000 {
		huge = huge.Mul(decimal.New(10, 0))
	}
	require.LessOrEqual(t, huge.Exponent(), int32(protocol.MaxDecimalExponent))
	require.Greater(t, huge.Coefficient().BitLen(), protocol.MaxDecimalCoefficientBits)

	encoded, err := protocol.ToDecimal(huge).MarshalBinary()
	require.NoError(t, err)
	require.Greater(t, len(encoded), protocol.MaxHistoryRecordBytes,
		"a single stream value already exceeds the per-history-record bound")

	pb := &protocol.LLOStreamValue{Type: protocol.LLOStreamValue_Decimal, Value: encoded}

	// Phase 1: an observation carrying it is rejected.
	_, err = protocol.UnmarshalObservedProtoStreamValue(pb)
	require.ErrorIs(t, err, protocol.ErrDecimalCoefficientOutOfRange)
}
