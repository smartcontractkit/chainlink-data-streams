package protocol

import (
	"errors"
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// ChannelDefinitionsBudget tracks the whole-set budgets that bound the size of
// the persisted records and the per-round work: MaxTotalStreamEntries and
// MaxTotalOptsBytes for the definitions record and the precursor,
// MaxPersistedAggregates for the carry-forward record, MaxTotalCalculatedStreams
// and MaxTotalTWAPCalls for expression evaluation, and
// MaxObservationStreamValuesLength for the observed stream IDs. It also counts
// the claims on stream IDs that already have an owner, which no set may gain.
//
// It needs nothing but the definitions and the report codecs, whose verdicts
// are pure functions of the definition, so every oracle reaches the same totals
// for the same set, which is what lets the state transition enforce it.
type ChannelDefinitionsBudget struct {
	factsOf func(llotypes.ChannelID, llotypes.ChannelDefinition) channelFacts

	streamEntries     int
	optsBytes         int
	calculatedStreams int
	twapCalls         int
	// Channels naming each (stream, aggregator) pair, so that removing one of
	// them releases the pair only when no other channel still names it.
	aggregatedPairs map[streamAggregatorPair]int
	// Channels listing each stream ID, counted the same way.
	streamIDs map[llotypes.StreamID]int
	// Owners of each stream ID that holds an aggregate: the channels observing
	// it and the expressions declaring it.
	observed   map[llotypes.StreamID]int
	calculated map[llotypes.StreamID]int
	// Claims on a stream ID beyond its first owner, where the observing
	// channels together are one owner and each declaring expression is another.
	clashes int
}

// NewChannelDefinitionsBudget measures defs. The costs derived from opts are
// read through cache; a nil cache memoizes nothing.
func NewChannelDefinitionsBudget(codecs map[llotypes.ReportFormat]ReportCodec, defs llotypes.ChannelDefinitions, cache *ChannelAnalysisCache) *ChannelDefinitionsBudget {
	return newChannelDefinitionsBudget(defs, func(channelID llotypes.ChannelID, cd llotypes.ChannelDefinition) channelFacts {
		return channelFactsFor(cache, codecs, channelID, cd)
	})
}

func newChannelDefinitionsBudget(defs llotypes.ChannelDefinitions, factsOf func(llotypes.ChannelID, llotypes.ChannelDefinition) channelFacts) *ChannelDefinitionsBudget {
	b := &ChannelDefinitionsBudget{
		factsOf:         factsOf,
		aggregatedPairs: make(map[streamAggregatorPair]int),
		streamIDs:       make(map[llotypes.StreamID]int),
		observed:        make(map[llotypes.StreamID]int),
		calculated:      make(map[llotypes.StreamID]int),
	}
	for channelID, cd := range defs {
		b.apply(channelID, cd, 1)
	}
	return b
}

// Replace swaps prev for next as channelID's definition in the measured set,
// with a nil prev for an addition. It refuses the change, leaving the totals as
// they were, when it grows a total past its budget. A total already over budget
// may stay there or shrink, so a set that is over budget can still be trimmed.
func (b *ChannelDefinitionsBudget) Replace(channelID llotypes.ChannelID, prev *llotypes.ChannelDefinition, next llotypes.ChannelDefinition) bool {
	before := b.totals()
	if prev != nil {
		b.apply(channelID, *prev, -1)
	}
	b.apply(channelID, next, 1)
	if b.totals().grows(before) {
		b.apply(channelID, next, -1)
		if prev != nil {
			b.apply(channelID, *prev, 1)
		}
		return false
	}
	return true
}

// budgetTotals is a snapshot of every total the budget bounds.
type budgetTotals struct {
	streamEntries, optsBytes, aggregatedPairs, calculatedStreams, twapCalls, streamIDs, clashes int
}

func (b *ChannelDefinitionsBudget) totals() budgetTotals {
	return budgetTotals{
		streamEntries:     b.streamEntries,
		optsBytes:         b.optsBytes,
		aggregatedPairs:   len(b.aggregatedPairs),
		calculatedStreams: b.calculatedStreams,
		twapCalls:         b.twapCalls,
		streamIDs:         len(b.streamIDs),
		clashes:           b.clashes,
	}
}

// grows reports whether any total went from before past its budget, or further
// past it.
func (t budgetTotals) grows(before budgetTotals) bool {
	grows := func(before, after, limit int) bool { return after > limit && after > before }
	return grows(before.streamEntries, t.streamEntries, MaxTotalStreamEntries) ||
		grows(before.optsBytes, t.optsBytes, MaxTotalOptsBytes) ||
		grows(before.aggregatedPairs, t.aggregatedPairs, MaxPersistedAggregates) ||
		grows(before.calculatedStreams, t.calculatedStreams, MaxTotalCalculatedStreams) ||
		grows(before.twapCalls, t.twapCalls, MaxTotalTWAPCalls) ||
		grows(before.streamIDs, t.streamIDs, MaxObservationStreamValuesLength) ||
		grows(before.clashes, t.clashes, 0)
}

// Err reports every budget the measured set exceeds. The unique stream ID
// budget and the clashing stream IDs are left to analyzeChannelDefinitions,
// which attributes them to the channels involved.
func (b *ChannelDefinitionsBudget) Err() error {
	var err error
	if b.streamEntries > MaxTotalStreamEntries {
		err = errors.Join(err, fmt.Errorf("too many stream entries across all channels, got: %d/%d", b.streamEntries, MaxTotalStreamEntries))
	}
	if len(b.aggregatedPairs) > MaxPersistedAggregates {
		err = errors.Join(err, fmt.Errorf("too many aggregated (stream, aggregator) pairs across all channels, got: %d/%d", len(b.aggregatedPairs), MaxPersistedAggregates))
	}
	if b.optsBytes > MaxTotalOptsBytes {
		err = errors.Join(err, fmt.Errorf("too many opts bytes across all channels, got: %d/%d", b.optsBytes, MaxTotalOptsBytes))
	}
	if b.calculatedStreams > MaxTotalCalculatedStreams {
		err = errors.Join(err, fmt.Errorf("too many calculated streams across all channels, got: %d/%d", b.calculatedStreams, MaxTotalCalculatedStreams))
	}
	if b.twapCalls > MaxTotalTWAPCalls {
		err = errors.Join(err, fmt.Errorf("too many TWAP calls across all channels, got: %d/%d", b.twapCalls, MaxTotalTWAPCalls))
	}
	return err
}

// apply adds cd to the measured set with a delta of 1, and removes it with -1.
// Tombstones count towards the persisted sizes only: nothing reads their
// streams or evaluates their opts.
func (b *ChannelDefinitionsBudget) apply(channelID llotypes.ChannelID, cd llotypes.ChannelDefinition, delta int) {
	b.streamEntries += delta * len(cd.Streams)
	b.optsBytes += delta * len(cd.Opts)
	for _, pair := range aggregatedPairsOf(cd) {
		count(b.aggregatedPairs, pair, delta)
	}
	if cd.Tombstone {
		return
	}
	for _, strm := range cd.Streams {
		count(b.streamIDs, strm.StreamID, delta)
		// Definitions written by older code may list their calculated streams
		// inline. Those are declared by the opts, not observed.
		if strm.Aggregator != llotypes.AggregatorCalculated {
			b.own(b.observed, strm.StreamID, delta)
		}
	}
	facts := b.factsOf(channelID, cd)
	b.twapCalls += delta * facts.twapCalls
	b.calculatedStreams += delta * len(facts.calculatedIDs)
	for _, streamID := range facts.calculatedIDs {
		b.own(b.calculated, streamID, delta)
	}
}

// own counts one owner of streamID in owners, keeping clashes current.
func (b *ChannelDefinitionsBudget) own(owners map[llotypes.StreamID]int, streamID llotypes.StreamID, delta int) {
	b.clashes -= b.excessClaims(streamID)
	count(owners, streamID, delta)
	b.clashes += b.excessClaims(streamID)
}

func (b *ChannelDefinitionsBudget) excessClaims(streamID llotypes.StreamID) int {
	declared := b.calculated[streamID]
	if declared == 0 {
		return 0
	}
	if b.observed[streamID] > 0 {
		return declared
	}
	return declared - 1
}

// count adds delta to m[k], deleting the key once nothing holds it.
func count[K comparable](m map[K]int, k K, delta int) {
	if m[k] += delta; m[k] <= 0 {
		delete(m, k)
	}
}

// aggregatedPairsOf returns the (stream, aggregator) pairs aggregation produces
// a value for on behalf of cd, one entry per stream that names one. Tombstones
// and history backfill channels are not aggregated, and calculated streams are
// recomputed each round rather than carried forward.
func aggregatedPairsOf(cd llotypes.ChannelDefinition) []streamAggregatorPair {
	if cd.Tombstone || cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
		return nil
	}
	pairs := make([]streamAggregatorPair, 0, len(cd.Streams))
	for _, strm := range cd.Streams {
		if strm.Aggregator == 0 || strm.Aggregator == llotypes.AggregatorCalculated {
			continue
		}
		pairs = append(pairs, streamAggregatorPair{streamID: strm.StreamID, aggregator: strm.Aggregator})
	}
	return pairs
}
