package protocol

import (
	"errors"
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// ChannelDefinitionsBudget tracks the whole-set budgets that bound the size of
// the persisted records: MaxTotalStreamEntries and MaxTotalOptsBytes for the
// definitions record and the precursor, MaxPersistedAggregates for the
// carry-forward record.
//
// It needs nothing but the definitions, so every oracle reaches the same totals
// for the same set, which is what lets the state transition enforce it.
type ChannelDefinitionsBudget struct {
	streamEntries int
	optsBytes     int
	// Channels naming each (stream, aggregator) pair, so that removing one of
	// them releases the pair only when no other channel still names it.
	aggregatedPairs map[streamAggregatorPair]int
}

// NewChannelDefinitionsBudget measures defs.
func NewChannelDefinitionsBudget(defs llotypes.ChannelDefinitions) *ChannelDefinitionsBudget {
	b := &ChannelDefinitionsBudget{aggregatedPairs: make(map[streamAggregatorPair]int)}
	for _, cd := range defs {
		b.add(cd)
	}
	return b
}

// Replace swaps prev for next in the measured set, with a nil prev for an
// addition. It refuses the change, leaving the totals as they were, when it
// grows a total past its budget. A total already over budget may stay there or
// shrink, so a set that is over budget can still be trimmed.
func (b *ChannelDefinitionsBudget) Replace(prev *llotypes.ChannelDefinition, next llotypes.ChannelDefinition) bool {
	streamEntries, optsBytes, pairs := b.streamEntries, b.optsBytes, len(b.aggregatedPairs)
	if prev != nil {
		b.remove(*prev)
	}
	b.add(next)
	grows := func(before, after, limit int) bool { return after > limit && after > before }
	if grows(streamEntries, b.streamEntries, MaxTotalStreamEntries) ||
		grows(optsBytes, b.optsBytes, MaxTotalOptsBytes) ||
		grows(pairs, len(b.aggregatedPairs), MaxPersistedAggregates) {
		b.remove(next)
		if prev != nil {
			b.add(*prev)
		}
		return false
	}
	return true
}

// Err reports every budget the measured set exceeds.
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
	return err
}

func (b *ChannelDefinitionsBudget) add(cd llotypes.ChannelDefinition) {
	b.streamEntries += len(cd.Streams)
	b.optsBytes += len(cd.Opts)
	for _, pair := range aggregatedPairsOf(cd) {
		b.aggregatedPairs[pair]++
	}
}

func (b *ChannelDefinitionsBudget) remove(cd llotypes.ChannelDefinition) {
	b.streamEntries -= len(cd.Streams)
	b.optsBytes -= len(cd.Opts)
	for _, pair := range aggregatedPairsOf(cd) {
		if b.aggregatedPairs[pair]--; b.aggregatedPairs[pair] <= 0 {
			delete(b.aggregatedPairs, pair)
		}
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
