package llo

import (
	"fmt"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"
)

// historyRequirements is the depth of history each pair needs this round: the
// maximum any live channel's expressions ask for.
type historyRequirements struct {
	// depths holds the admitted pairs and their required window depth.
	depths map[histKey]uint32
	// denied holds pairs that were required but could not be admitted within
	// the pair cap, sorted. Channels referencing them cannot evaluate
	// and so are not reportable; they are reported here so the caller can say so
	// loudly rather than silently producing shorter windows.
	denied []histKey
}

// computeHistoryRequirements derives, from replicated state alone, how deep each
// pair's history window must be.
//
// Determinism is the whole point: the inputs are the channel definitions and
// their opts (both replicated) and expression analysis (a pure function of the
// expression string), and every map is drained into a sorted slice before it
// influences anything. Two oracles running this on the same state must produce
// identical depths, because those depths become persisted state.
//
// Channels whose expressions cannot be analyzed, or that aggregate one stream
// two ways, contribute nothing: they cannot be evaluated either (see
// calculated.ProcessCalculatedStreams), so reserving history for them would keep
// dead windows alive. They are logged at debug level only, since evaluation
// reports the same channel at error level every round.
//
// incumbents are the pairs already holding a window (see admitHistoryRequirements).
func computeHistoryRequirements(defs llotypes.ChannelDefinitions, optsCache *protocol.OptsCache, incumbents []histKey, lggr logger.Logger) historyRequirements {
	required := map[histKey]uint32{}

	// Channels are visited in sorted order so that logging and the
	// admission decision below cannot depend on map iteration order.
	channelIDs := make([]llotypes.ChannelID, 0, len(defs))
	for cid := range defs {
		channelIDs = append(channelIDs, cid)
	}
	sortChannelIDs(channelIDs)

	for _, cid := range channelIDs {
		cd := defs[cid]
		if cd.Tombstone || cd.ReportFormat != llotypes.ReportFormatEVMABIEncodeUnpackedExpr {
			continue
		}

		aggByStream, err := calculated.AggregatorByStream(cd)
		if err != nil {
			lggr.Debugw("Skipping history requirements for channel with ambiguous stream aggregators",
				"channelID", cid, "err", err)
			continue
		}

		expressions, err := calculated.Expressions(optsCache, cd, cid)
		if err != nil {
			lggr.Debugw("Skipping history requirements for channel with unresolvable opts",
				"channelID", cid, "err", err)
			continue
		}

		for _, expression := range expressions {
			refs, err := calculated.AnalyzeExpressionHistory(expression)
			if err != nil {
				lggr.Debugw("Skipping history requirements for invalid expression",
					"channelID", cid, "expression", expression, "err", err)
				continue
			}
			for _, ref := range refs {
				aggregator, ok := aggByStream[ref.StreamID]
				if !ok {
					lggr.Debugw("Expression reads history for a stream the channel does not observe",
						"channelID", cid, "streamID", ref.StreamID, "expression", expression)
					continue
				}
				key := histKey{streamID: ref.StreamID, aggregator: aggregator}
				// The field is not part of the key: one window serves every
				// field, so the depth is the deepest request across fields too.
				if ref.Count > required[key] {
					required[key] = ref.Count
				}
			}
		}
	}

	return admitHistoryRequirements(required, incumbents)
}

// admitHistoryRequirements applies the pair and byte caps.
//
// Required incumbents, the pairs already holding a window, are admitted first,
// then the remaining pairs, each group in (streamID, aggregator) order, until a
// cap is reached; the rest are denied. Admitting incumbents first stops a new
// pair with a lower key from evicting a warm window once the cap is reached.
// Incumbents come from the replicated history index and the order is by key
// rather than by, say, depth or channel count, which is what makes the outcome
// identical on every oracle, the admission decision is part of what
// determines persisted state.
//
// Denial is deliberately visible and total for the pair: a channel referencing a
// denied pair produces no value and does not report, rather than silently
// evaluating over a shorter window than it asked for.
//
// The pair cap is the only rule. Under the chunked layout a round rewrites one
// chunk and one header per pair whatever the depth, so a pair's cost against the
// per-round byte budget is a constant — MaxHistoryPairs of them fit inside
// MaxHistoryTotalBytes by construction, which limits.go asserts at compile
// time. That was not true of the single-blob layout, where cost scaled with
// depth and the budget denied pairs long before the cap did.
func admitHistoryRequirements(required map[histKey]uint32, incumbents []histKey) historyRequirements {
	held := make(map[histKey]struct{}, len(incumbents))
	keys := make([]histKey, 0, len(required))
	for _, key := range incumbents {
		if _, ok := required[key]; ok {
			held[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sortHistKeys(keys)
	newcomers := make([]histKey, 0, len(required)-len(keys))
	for key := range required {
		if _, ok := held[key]; !ok {
			newcomers = append(newcomers, key)
		}
	}
	sortHistKeys(newcomers)
	keys = append(keys, newcomers...)

	out := historyRequirements{depths: make(map[histKey]uint32, len(keys))}
	for _, key := range keys {
		if len(out.depths) >= protocol.MaxHistoryPairs {
			out.denied = append(out.denied, key)
			continue
		}
		out.depths[key] = required[key]
	}
	return out
}

// apply sets the required depth of every admitted pair on the round's store, and
// clears any pair that is no longer required so Flush can reclaim it.
//
// Pairs are applied in sorted order; every stored pair not admitted this round is
// explicitly set to zero, which is what turns "no longer referenced" into a
// deletion rather than state that lingers forever.
func (r historyRequirements) apply(store *historyStore) error {
	keys := make([]histKey, 0, len(r.depths))
	for key := range r.depths {
		keys = append(keys, key)
	}
	sortHistKeys(keys)

	for _, key := range keys {
		if err := store.SetRequired(key.streamID, key.aggregator, r.depths[key]); err != nil {
			return fmt.Errorf("set history requirement for stream %d aggregator %d: %w", key.streamID, key.aggregator, err)
		}
	}

	for _, key := range store.indexKeys() {
		if _, admitted := r.depths[key]; admitted {
			continue
		}
		if err := store.SetRequired(key.streamID, key.aggregator, 0); err != nil {
			return fmt.Errorf("clear history requirement for stream %d aggregator %d: %w", key.streamID, key.aggregator, err)
		}
	}
	return nil
}

// requires reports whether a pair was admitted, i.e. whether this round should
// append to its window.
func (r historyRequirements) requires(sid llotypes.StreamID, agg llotypes.Aggregator) bool {
	_, ok := r.depths[histKey{streamID: sid, aggregator: agg}]
	return ok
}

// sortedDenied returns denied pairs for logging and telemetry.
func (r historyRequirements) sortedDenied() []histKey {
	denied := append([]histKey{}, r.denied...)
	sortHistKeys(denied)
	return denied
}
