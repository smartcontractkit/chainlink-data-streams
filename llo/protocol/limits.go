package protocol

import "github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

// Additional limits so we can more effectively bound the size of observations
// NOTE: These are hardcoded because these exact values are relied upon as a
// property of coming to consensus, it's too dangerous to make these
// configurable on a per-node basis. It may be possible to add them to the
// OffchainConfig if they need to be changed dynamically and in a
// backwards-compatible way.
//
// These LLO-protocol limits are shared across all plugin versions.
const (
	// MaxReportCount is the maximum number of reports (and therefore channels)
	// supported. CAREFUL! If we ever accidentally exceed this e.g. through too
	// many channels/streams, the protocol will halt.
	// https://smartcontract-it.atlassian.net/browse/MERC-6468
	MaxReportCount = ocr3types.MaxMaxReportCount

	// Maximum amount of channels that can be removed per round (if more than
	// this need to be removed, they will be removed in batches until
	// everything is up-to-date)
	MaxObservationRemoveChannelIDsLength = 5
	// Maximum amount of channels that can be added/updated per round (if more
	// than this need to be added, they will be added in batches until
	// everything is up-to-date)
	MaxObservationUpdateChannelDefinitionsLength = 5
	// Maximum number of streams that can be observed per round
	MaxObservationStreamValuesLength = 10_000
	// Maximum allowed number of streams per channel
	MaxStreamsPerChannel = 10_000
	// MaxDecimalExponent bounds the absolute value of the base-10 exponent of
	// any decimal decoded from an untrusted source (peer observations, stored
	// state etc).
	// Stream values need only a couple of dozen decimal places, so this
	// leaves enough headroom.
	MaxDecimalExponent = 1_000
	// MaxOutcomeChannelDefinitionsLength is the maximum number of channels that
	// can be supported
	MaxOutcomeChannelDefinitionsLength = MaxReportCount

	// Stream history limits.
	//
	// A history "pair" is a (streamID, aggregator) tuple: the identity of one
	// persisted window of past agreed values. The aggregator is part of the
	// identity because the same stream can be aggregated differently by
	// different channels, and appending both into one series would silently
	// interleave two unrelated value series. The stream value's field
	// (bid/ask/benchmark) is NOT part of the identity: one stored window serves
	// every field, because a whole stream value is stored per record and the
	// field is selected when the window is read.
	//
	// Measured sizes of a full 1024-record window, marshaled:
	//
	//	Decimal, small integers               19.7 KiB  (19.7 B/record)
	//	Decimal, 18-digit price               26.0 KiB  (26.0 B/record)
	//	Decimal, 38-digit                     37.0 KiB  (37.0 B/record)
	//	TimestampedStreamValue, 18-digit      42.0 KiB  (42.0 B/record)
	//	Quote, three 18-digit decimals        60.0 KiB  (60.0 B/record)

	// MaxHistoryRecordsPerPair bounds the depth of the persisted history window
	// for a single pair, and therefore the maximum depth any expression may
	// request. Note this is per pair, not per stream: a stream aggregated two
	// ways holds two windows of up to this depth each.
	MaxHistoryRecordsPerPair = 1024
	// MaxHistoryPairs bounds how many pairs may have history at once. Pairs are
	// ordered by (streamID, aggregator) and those beyond the cap are denied
	// history; channels referencing them become unreportable rather than
	// silently getting a shortened window. A stream aggregated two ways
	// consumes two of these, not one.
	MaxHistoryPairs = 128
	// MaxHistoryTotalBytes bounds the estimated total history bytes rewritten
	// per round (sum over pairs of requiredCount * MaxHistoryRecordBytes). The
	// OCR3.1 per-round budget for modified keys plus values is 10 MiB and is
	// shared with channel definitions and the other key prefixes, so history is
	// held well below it.
	MaxHistoryTotalBytes = 4 << 20
	// MaxHistoryRecordsPerExpression bounds the total history depth a single
	// expression may request, summed over all of its History calls. Each
	// requested record is work the evaluator does every round, so without this
	// one expression could combine many legal per-pair depths into an
	// arbitrarily expensive evaluation.
	MaxHistoryRecordsPerExpression = 4 * MaxHistoryRecordsPerPair
	// MaxHistoryRecordBytes is the maximum serialized size of one history
	// record, enforced on append (StreamHistory.Append) and used as the
	// per-record size when admitting pairs against MaxHistoryTotalBytes.
	// Enforcing the same number that the budget assumes is what makes the
	// budget a real bound rather than an estimate.
	//
	// It has to be enforced rather than assumed because MaxDecimalExponent
	// bounds a decimal's exponent but not its coefficient length: a 1000-digit
	// coefficient is ~415 bytes, so an unchecked Quote of three of them would be
	// ~1.3 KB per record and a full window ~1.3 MB — orders of magnitude past
	// what the byte budget was sized for, and still under libocr's 2 MiB
	// per-key limit, so nothing else would reject it.
	//
	// 128 B is roughly twice the largest measured record (a quote of three
	// 18-digit decimals, 60 B), so it accommodates real feeds while rejecting
	// pathological values. A rejected record leaves a gap in the series, which
	// is honest, and is logged.
	MaxHistoryRecordBytes = 128

	// MaxHistoryChunkRecords is the number of records held by one slot of the
	// chunked ring layout. It is the single knob of that layout: per-round write
	// bytes are proportional to it, per-round reads are proportional to depth
	// divided by it, and both are comfortable at 64.
	//
	// For a quote stream (60 B/record) a full chunk is 3.9 KiB, so a pair costs
	// ~1.9 KiB of writes in an average round instead of rewriting the whole
	// window, and a 1024-deep window is read in 18 point reads instead of 1024.
	// The value is consensus-relevant — it determines the bytes every oracle
	// writes — so it is a constant here and never per-node configurable.
	// Changing it changes the stored layout and requires resetting every window.
	//
	// It must divide MaxHistoryRecordsPerPair, so that a window at maximum depth
	// is an exact number of chunks (asserted by TestHistoryChunkLimits).
	MaxHistoryChunkRecords = 64
	// MaxHistoryChunkSlots is the size of the ring: the number of distinct chunk
	// slots one pair may occupy.
	//
	// A window retains at most MaxHistoryRecordsPerPair/MaxHistoryChunkRecords+1
	// chunks — the +1 for the partially consumed oldest chunk — and a round may
	// hold one more transiently, between appending into a freshly created chunk
	// and evicting the oldest. Hence +2.
	//
	// The ring is deliberately a fixed, small, statically known slot space
	// rather than an unbounded sequence: the in-round reader offers no range
	// scan, so if a header is ever unreadable there is no way to discover which
	// chunk keys exist. A bounded slot space makes recovery a blind delete of
	// every slot. Reuse of a slot across a lap of the ring is caught by the
	// sequence stored inside each chunk.
	MaxHistoryChunkSlots = MaxHistoryRecordsPerPair/MaxHistoryChunkRecords + 2
	// MaxHistoryRetainedRecords is the most records a window may hold. Retention
	// works in whole chunks, so a window overshoots its required depth by up to
	// one chunk less one record. Readers ask for an exact depth and never see
	// the overshoot.
	MaxHistoryRetainedRecords = MaxHistoryRecordsPerPair + MaxHistoryChunkRecords - 1
	// MaxHistoryHeaderBytes bounds the serialized size of a window header: a
	// couple of scalars plus two arrays of at most MaxHistoryChunkSlots entries,
	// which lands under 300 B. 512 leaves headroom without mattering to the
	// budget it feeds.
	MaxHistoryHeaderBytes = 512
	// MaxHistoryPairRoundBytes is what one pair can cost the per-round byte
	// budget: the newest chunk, rewritten every round, plus the header. Sealed
	// chunks are immutable and evictions are deletes, so nothing else is
	// written however deep the window is.
	//
	// This is the number that makes the chunked layout worth having: it is
	// independent of the window depth, where the single-blob layout charged
	// requiredCount * MaxHistoryRecordBytes every round.
	MaxHistoryPairRoundBytes = MaxHistoryChunkRecords*MaxHistoryRecordBytes + MaxHistoryHeaderBytes

	// MaxDecompressedObservationLength bounds the size of an observation after
	// zstd decompression.
	//
	// A legitimate observation is bounded by
	// MaxObservationStreamValuesLength stream values plus
	// MaxObservationUpdateChannelDefinitionsLength channel definitions of up
	// to MaxStreamsPerChannel streams each, which lands in the low single-digit
	// MiB range. 16 MiB leaves generous headroom.
	MaxDecompressedObservationLength = 16 << 20
)
