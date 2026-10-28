package llo

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"google.golang.org/protobuf/proto"
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
	// The adversarial worst case: the longest coefficient observation decode
	// admits (MaxDecimalCoefficientBits), negative, at the most negative
	// exponent, with maximal IDs and timestamps so every varint is at its
	// widest. An aggregate is one of the observed values, so it is held to the
	// same bounds.
	coefficient := new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits)
	coefficient.Sub(coefficient, big.NewInt(1)).Neg(coefficient)
	price := decimal.NewFromBigInt(coefficient, -protocol.MaxDecimalExponent)
	const maxTimestamp = uint64(math.MaxUint64)

	carry := make(map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue, protocol.MaxObservationStreamValuesLength)
	aggregators := []llotypes.Aggregator{llotypes.AggregatorMedian, llotypes.AggregatorMode}
	for i := range protocol.MaxObservationStreamValuesLength {
		sid := llotypes.StreamID(math.MaxUint32 - i)
		carry[sid] = make(map[llotypes.Aggregator]*protocol.TimestampedStreamValue, len(aggregators))
		for _, agg := range aggregators {
			carry[sid][agg] = &protocol.TimestampedStreamValue{
				ObservedAtNanoseconds: maxTimestamp,
				StreamValue:           protocol.ToDecimal(price),
			}
		}
	}
	// Exactly the cap: every observed stream aggregated two ways.
	require.Equal(t, protocol.MaxPersistedAggregates, len(carry)*len(aggregators))

	validAfter := make(map[llotypes.ChannelID]uint64, protocol.MaxOutcomeChannelDefinitionsLength)
	reportable := make(map[llotypes.ChannelID]bool, protocol.MaxOutcomeChannelDefinitionsLength)
	for i := range protocol.MaxOutcomeChannelDefinitionsLength {
		cid := llotypes.ChannelID(math.MaxUint32 - i)
		validAfter[cid] = maxTimestamp
		reportable[cid] = true
	}

	// A full window of maximal agreed timestamps.
	window := make([]uint64, agreedTimestampWindow)
	for i := range window {
		window[i] = maxTimestamp
	}

	kv := newMemKV()
	require.NoError(t, writeHotState(kv, maxTimestamp, validAfter, reportable, carry, window, logger.Test(t)))

	record, err := kv.Read(keyHotState)
	require.NoError(t, err)
	require.LessOrEqual(t, len(record), ocr3_1types.MaxMaxKeyValueValueBytes,
		"worst-case r/agg record (%d bytes) exceeds the per-key limit", len(record))
	t.Logf("worst-case r/agg record: %d bytes of %d", len(record), ocr3_1types.MaxMaxKeyValueValueBytes)
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
	require.NoError(t, writeHotState(kv, 1, nil, nil, carry, nil, logger.Test(t)))

	state := &kvState{
		channelDefinitions:    llotypes.ChannelDefinitions{},
		validAfterNanoseconds: map[llotypes.ChannelID]uint64{},
		reportedLastRound:     map[llotypes.ChannelID]bool{},
		carryForward:          map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{},
	}
	require.NoError(t, readHotState(kv, state, logger.Test(t)))

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

// TestLimits_ChannelVotesRespectDefinitionSetBudgets covers updates that each
// pass admission on their own but whose union does not: two oracles with
// different desired sets each get an addition past the vote threshold in the
// same round. Admission cannot see the union, so the state transition must
// keep it within budget.
func TestLimits_ChannelVotesRespectDefinitionSetBudgets(t *testing.T) {
	const f = 1
	channel := func(streamID llotypes.StreamID) llotypes.ChannelDefinition {
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: streamID, Aggregator: llotypes.AggregatorMedian}},
			Opts:         []byte(strings.Repeat("x", protocol.MaxChannelOptsBytes)),
		}
	}
	// One channel short of MaxTotalOptsBytes, so either addition fits alone.
	committed := llotypes.ChannelDefinitions{}
	for c := range protocol.MaxTotalOptsBytes/protocol.MaxChannelOptsBytes - 1 {
		committed[llotypes.ChannelID(c+1)] = channel(1)
	}
	updates := map[[32]byte]protocol.ChannelDefinitionWithID{
		{1}: {ChannelDefinition: channel(2), ChannelID: 1000},
		{2}: {ChannelDefinition: channel(3), ChannelID: 1001},
	}
	votes := map[[32]byte]int{{1}: f + 1, {2}: f + 1}

	for _, id := range []llotypes.ChannelID{1000, 1001} {
		alone := cloneChannelDefinitions(committed)
		alone[id] = channel(2)
		require.NoError(t, protocol.NewChannelDefinitionsBudget(nil, alone, nil).Err())
	}

	pending := cloneChannelDefinitions(committed)
	applyChannelVotes(nil, nil, pending, nil, updates, votes, f)
	require.NoError(t, protocol.NewChannelDefinitionsBudget(nil, pending, nil).Err())
	// Applied in channel ID order, so the lower ID wins on every oracle.
	require.Contains(t, pending, llotypes.ChannelID(1000))
	require.NotContains(t, pending, llotypes.ChannelID(1001))

	// Once the committed set is over budget, a tombstone that trims it is
	// still applied.
	over := cloneChannelDefinitions(pending)
	over[1001] = channel(3)
	tombstone := llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, Tombstone: true}
	applyChannelVotes(nil, nil, over, nil,
		map[[32]byte]protocol.ChannelDefinitionWithID{{3}: {ChannelDefinition: tombstone, ChannelID: 1001}},
		map[[32]byte]int{{3}: f + 1}, f)
	require.True(t, over[1001].Tombstone)
	require.NoError(t, protocol.NewChannelDefinitionsBudget(nil, over, nil).Err())
}

// TestLimits_ChannelVotesRespectExpressionBudgets covers the budgets measured
// from opts: two updates that each fit can still exceed the calculated stream
// budget together, or declare the same calculated stream.
func TestLimits_ChannelVotesRespectExpressionBudgets(t *testing.T) {
	const f = 1
	expression := func(observed llotypes.StreamID, calculatedIDs ...llotypes.StreamID) llotypes.ChannelDefinition {
		abi := make([]string, 0, len(calculatedIDs))
		for _, id := range calculatedIDs {
			abi = append(abi, fmt.Sprintf(`{"expressionStreamID":%d}`, id))
		}
		return llotypes.ChannelDefinition{
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams:      []llotypes.Stream{{StreamID: observed, Aggregator: llotypes.AggregatorMedian}},
			Opts:         llotypes.ChannelOpts(fmt.Sprintf(`{"abi":[%s]}`, strings.Join(abi, ","))),
		}
	}
	votes := map[[32]byte]int{{1}: f + 1, {2}: f + 1}

	t.Run("calculated streams", func(t *testing.T) {
		// One calculated stream short of MaxTotalCalculatedStreams.
		ids := make([]llotypes.StreamID, protocol.MaxTotalCalculatedStreams-1)
		for i := range ids {
			ids[i] = llotypes.StreamID(1_000_000 + i)
		}
		committed := llotypes.ChannelDefinitions{1: expression(1, ids...)}
		updates := map[[32]byte]protocol.ChannelDefinitionWithID{
			{1}: {ChannelDefinition: expression(2, 2_000_000), ChannelID: 1000},
			{2}: {ChannelDefinition: expression(3, 2_000_001), ChannelID: 1001},
		}
		for _, u := range updates {
			alone := cloneChannelDefinitions(committed)
			alone[u.ChannelID] = u.ChannelDefinition
			require.NoError(t, protocol.NewChannelDefinitionsBudget(nil, alone, nil).Err())
		}

		pending := cloneChannelDefinitions(committed)
		applyChannelVotes(nil, nil, pending, nil, updates, votes, f)
		require.NoError(t, protocol.NewChannelDefinitionsBudget(nil, pending, nil).Err())
		require.Contains(t, pending, llotypes.ChannelID(1000))
		require.NotContains(t, pending, llotypes.ChannelID(1001))
	})

	t.Run("calculated stream declared twice", func(t *testing.T) {
		committed := llotypes.ChannelDefinitions{1: expression(1, 100)}
		updates := map[[32]byte]protocol.ChannelDefinitionWithID{
			{1}: {ChannelDefinition: expression(2, 200), ChannelID: 1000},
			{2}: {ChannelDefinition: expression(3, 200), ChannelID: 1001},
		}

		pending := cloneChannelDefinitions(committed)
		applyChannelVotes(nil, nil, pending, nil, updates, votes, f)
		require.Contains(t, pending, llotypes.ChannelID(1000))
		require.NotContains(t, pending, llotypes.ChannelID(1001))
	})
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

// worstCaseDefinitions builds channels definitions of streamsEach streams and
// optsEach opts bytes, with every ID in the 5-byte varint range so each entry
// encodes at its largest. The channels share one set of stream IDs, which
// keeps the set within the aggregated-pair budget however many channels it has.
func worstCaseDefinitions(channels, streamsEach, optsEach int) llotypes.ChannelDefinitions {
	streams := make([]llotypes.Stream, 0, streamsEach)
	for i := range streamsEach {
		streams = append(streams, llotypes.Stream{
			StreamID:   llotypes.StreamID(math.MaxUint32 - i),
			Aggregator: llotypes.AggregatorMedian,
		})
	}
	defs := make(llotypes.ChannelDefinitions, channels)
	for c := range channels {
		defs[llotypes.ChannelID(math.MaxUint32-c)] = llotypes.ChannelDefinition{
			ReportFormat:           llotypes.ReportFormatJSON,
			Streams:                streams,
			Opts:                   []byte(strings.Repeat("x", optsEach)),
			Source:                 math.MaxUint32,
			DisableNilStreamValues: true,
		}
	}
	return defs
}

// TestLimits_ObservationWorstCaseFitsDeclared builds the largest observation
// this oracle can produce and measures it. The channel definition votes are
// the only part that admission alone does not keep under the libocr limit, so
// the votes are capped by bytes, and this test measures the cap together with
// every other field at its maximum.
func TestLimits_ObservationWorstCaseFitsDeclared(t *testing.T) {
	const channels = protocol.MaxObservationUpdateChannelDefinitionsLength
	defs := worstCaseDefinitions(channels, protocol.MaxStreamsPerChannel, protocol.MaxChannelOptsBytes)
	admitting := make(map[llotypes.ChannelID]struct{}, channels)
	for id := range defs {
		admitting[id] = struct{}{}
	}
	require.NoError(t, protocol.VerifyChannelDefinitionsForAdmission(nil, defs, admitting))

	// One admissible definition must always fit, or the first pending change
	// could never be voted on.
	for _, cd := range defs {
		require.Less(t, proto.Size(protocol.ChannelDefinitionToProto(cd)), maxObservationUpdateChannelDefinitionsBytes)
		break
	}
	updates := channelUpdateVotes(nil, defs)
	require.NotEmpty(t, updates)

	// The largest retirement report a predecessor can attest: a validAfter for
	// every channel, signed by every oracle.
	validAfter := make(map[llotypes.ChannelID]uint64, protocol.MaxRetirementReportChannels)
	for i := range protocol.MaxRetirementReportChannels {
		validAfter[llotypes.ChannelID(math.MaxUint32-i)] = math.MaxUint64
	}
	retirementReport, err := protocol.StandardRetirementReportCodec{}.Encode(protocol.RetirementReport{
		ProtocolVersion:       math.MaxUint32,
		ValidAfterNanoseconds: validAfter,
	})
	require.NoError(t, err)
	attested := &protocol.AttestedRetirementReport{RetirementReport: retirementReport, SeqNr: math.MaxUint64}
	signers := make([][]byte, protocol.MaxObservationPredecessorSignersLength)
	for i := range signers {
		signers[i] = make([]byte, protocol.MaxPredecessorSignerBytes)
		attested.Sigs = append(attested.Sigs, &protocol.AttributedOnchainSignature{
			Signature: make([]byte, protocol.MaxPredecessorSignerBytes),
			Signer:    math.MaxUint32,
		})
	}
	attestedBytes, err := proto.Marshal(attested)
	require.NoError(t, err)
	require.LessOrEqual(t, len(attestedBytes), protocol.MaxAttestedRetirementReportBytes,
		"worst-case attested retirement report (%d bytes) exceeds the declared limit", len(attestedBytes))

	removals := make(map[llotypes.ChannelID]struct{}, protocol.MaxObservationRemoveChannelIDsLength)
	for i := range protocol.MaxObservationRemoveChannelIDsLength {
		removals[llotypes.ChannelID(i)] = struct{}{}
	}
	formats := make(map[llotypes.ReportFormat]struct{}, protocol.MaxObservationSupportedReportFormatsLength)
	for i := range protocol.MaxObservationSupportedReportFormatsLength {
		formats[llotypes.ReportFormat(math.MaxUint32-i)] = struct{}{}
	}

	obs := Observation{
		AttestedPredecessorRetirement: attestedBytes,
		ShouldRetire:                  true,
		UnixTimestampNanoseconds:      math.MaxUint64,
		RemoveChannelIDs:              removals,
		UpdateChannelDefinitions:      updates,
		SupportedReportFormats:        formats,
		PredecessorSigners:            signers,
		PredecessorF:                  math.MaxUint8,
	}
	// The encoder references one blob. A handle carries at most one
	// availability signature per oracle, well under 4 KiB.
	encoded, err := encodeObservation(obs, [][]byte{make([]byte, 4<<10)})
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), ocr3_1types.MaxMaxObservationBytes,
		"worst-case observation (%d bytes) exceeds the declared limit", len(encoded))
	t.Logf("worst-case observation: %d bytes of the %d limit, voting %d of %d definitions",
		len(encoded), ocr3_1types.MaxMaxObservationBytes, len(updates), channels)
}

// TestLimits_PrecursorWorstCaseFitsDeclared builds the largest precursor the
// admission budgets and the aggregate caps permit and measures it against the
// declared libocr limit.
func TestLimits_PrecursorWorstCaseFitsDeclared(t *testing.T) {
	const channels = protocol.MaxOutcomeChannelDefinitionsLength
	defs := worstCaseDefinitions(channels, protocol.MaxTotalStreamEntries/channels, protocol.MaxTotalOptsBytes/channels)

	validAfter := make(map[llotypes.ChannelID]uint64, channels)
	for id := range defs {
		validAfter[id] = math.MaxUint64
	}

	// The largest decimal observation decode and the calculated stream write
	// path accept.
	coefficient := new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits)
	coefficient.Sub(coefficient, big.NewInt(1))
	largest := decimal.NewFromBigInt(coefficient, -protocol.MaxDecimalExponent)

	// Observed pairs at the persisted-aggregate cap, each a quote of three
	// largest decimals, plus calculated streams at their own cap.
	aggregates := make(protocol.StreamAggregates, protocol.MaxPersistedAggregates+protocol.MaxTotalCalculatedStreams)
	for i := range protocol.MaxPersistedAggregates {
		aggregates[llotypes.StreamID(math.MaxUint32-i)] = map[llotypes.Aggregator]protocol.StreamValue{
			llotypes.AggregatorMedian: &protocol.Quote{Bid: largest, Benchmark: largest, Ask: largest},
		}
	}
	for i := range protocol.MaxTotalCalculatedStreams {
		aggregates[llotypes.StreamID(math.MaxUint32-protocol.MaxPersistedAggregates-i)] = map[llotypes.Aggregator]protocol.StreamValue{
			llotypes.AggregatorCalculated: protocol.ToDecimal(largest),
		}
	}

	// Every channel reportable, under the largest IDs.
	reportable := slices.Sorted(maps.Keys(defs))

	encoded, err := encodePrecursor(precursor{
		LifeCycleStage:                  llotypes.LifeCycleStage(strings.Repeat("x", 32)),
		ObservationTimestampNanoseconds: math.MaxUint64,
		ChannelDefinitions:              defs,
		ValidAfterNanoseconds:           validAfter,
		StreamAggregates:                aggregates,
		ChannelStateSeqNr:               math.MaxUint64,
		ReportableChannelIDs:            reportable,
	})
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), ocr3_1types.MaxMaxReportsPlusPrecursorBytes,
		"worst-case precursor (%d bytes) exceeds the declared limit", len(encoded))
	t.Logf("worst-case precursor: %d bytes of the %d limit",
		len(encoded), ocr3_1types.MaxMaxReportsPlusPrecursorBytes)
}
