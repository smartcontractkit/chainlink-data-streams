package llo

import (
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// selectBackfillCandidate returns the next history-backfill observation to emit
// for backfillCID: its timestamp (nanoseconds), the raw opts key, and the parsed
// opts. ok is false when the channel is not a valid, in-progress backfill (no
// definition, wrong format, missing or tombstoned target, no watermark, or
// backfill complete).
//
// The watermark is validAfter[backfillCID] (the last emitted backfill observation
// time); the next candidate is the smallest emittable observation strictly after
// the watermark and strictly before obsTsNanos. This mirrors the v30
// SelectBackfillCandidate but operates on plain fields (shared by precursor and
// kvState) and returns a bool instead of an UnreportableChannelError.
//
// skipped counts the earlier rows in the window passed over as unemittable.
// Selecting the next row consumes them: the watermark advances past them.
func selectBackfillCandidate(defs llotypes.ChannelDefinitions, validAfter map[llotypes.ChannelID]uint64, obsTsNanos uint64, backfillCID llotypes.ChannelID, optsCache *protocol.OptsCache) (tsNanos uint64, rawTS uint64, opts protocol.HistoryBackfillOpts, skipped int, ok bool) {
	cd, exists := defs[backfillCID]
	if !exists || cd.ReportFormat != llotypes.ReportFormatHistoryBackfill {
		return 0, 0, protocol.HistoryBackfillOpts{}, 0, false
	}
	o, err := protocol.GetHistoryBackfillOpts(optsCache, cd, backfillCID)
	if err != nil {
		return 0, 0, protocol.HistoryBackfillOpts{}, 0, false
	}
	// A tombstoned target no longer reports, so its history is not backfilled
	// either. The watermark holds, and backfill resumes if the target returns.
	target, exists := defs[o.TargetChannelID]
	if !exists || target.Tombstone {
		return 0, 0, protocol.HistoryBackfillOpts{}, 0, false
	}
	res, err := protocol.TargetChannelTimeResolution(target)
	if err != nil {
		return 0, 0, protocol.HistoryBackfillOpts{}, 0, false
	}
	// Reports needs the target's report-timestamp resolution. It depends on the
	// target alone, so when it fails no row can be emitted.
	if _, err := protocol.ReportTimestampResolutionNanos(target); err != nil {
		return 0, 0, protocol.HistoryBackfillOpts{}, 0, false
	}
	watermark, exists := validAfter[backfillCID]
	if !exists {
		return 0, 0, protocol.HistoryBackfillOpts{}, 0, false
	}

	type candidate struct{ raw, nanos uint64 }
	var candidates []candidate
	for rawKey := range o.Observations {
		tsN, ok := protocol.ObservationTimestampKeyToNanoseconds(rawKey, res)
		if !ok {
			// Not expressible in nanoseconds; scaling it would wrap into a
			// timestamp that looks like a valid past one.
			continue
		}
		if tsN >= obsTsNanos || tsN <= watermark {
			continue
		}
		candidates = append(candidates, candidate{raw: rawKey, nanos: tsN})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].nanos != candidates[j].nanos {
			return candidates[i].nanos < candidates[j].nanos
		}
		return candidates[i].raw < candidates[j].raw
	})
	// The candidate must be emittable, not merely selectable, or Reports skips
	// it after the watermark has already advanced past it. Building the row's
	// stream values is a pure function of (target definition, row), so checking
	// it here keeps selection and emission on one path. A row that cannot be
	// built never will be: pass over it rather than stall the backfill on it.
	for _, c := range candidates {
		if _, err := protocol.BuildBackfillStreamValues(target, o.Observations[c.raw]); err != nil {
			skipped++
			continue
		}
		return c.nanos, c.raw, o, skipped, true
	}
	return 0, 0, protocol.HistoryBackfillOpts{}, skipped, false
}
