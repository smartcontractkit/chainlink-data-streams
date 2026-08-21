package llo

import (
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// selectBackfillCandidate returns the next history-backfill observation to emit
// for backfillCID: its timestamp (nanoseconds), the raw opts key, and the parsed
// opts. ok is false when the channel is not a valid, in-progress backfill (no
// definition, wrong format, missing target, no watermark, or backfill complete).
//
// The watermark is validAfter[backfillCID] (the last emitted backfill observation
// time); the next candidate is the smallest observation strictly after the
// watermark and strictly before obsTsNanos. This mirrors the v30
// SelectBackfillCandidate but operates on plain fields (shared by precursor and
// kvState) and returns a bool instead of an UnreportableChannelError.
func selectBackfillCandidate(defs llotypes.ChannelDefinitions, validAfter map[llotypes.ChannelID]uint64, obsTsNanos uint64, backfillCID llotypes.ChannelID, optsCache *protocol.OptsCache) (tsNanos uint64, rawTS uint64, opts protocol.HistoryBackfillOpts, ok bool) {
	cd, exists := defs[backfillCID]
	if !exists || cd.ReportFormat != llotypes.ReportFormatHistoryBackfill {
		return 0, 0, protocol.HistoryBackfillOpts{}, false
	}
	o, err := protocol.GetHistoryBackfillOpts(optsCache, cd, backfillCID)
	if err != nil {
		return 0, 0, protocol.HistoryBackfillOpts{}, false
	}
	target, exists := defs[o.TargetChannelID]
	if !exists {
		return 0, 0, protocol.HistoryBackfillOpts{}, false
	}
	res, err := protocol.TargetChannelTimeResolution(target)
	if err != nil {
		return 0, 0, protocol.HistoryBackfillOpts{}, false
	}
	watermark, exists := validAfter[backfillCID]
	if !exists {
		return 0, 0, protocol.HistoryBackfillOpts{}, false
	}

	var bestRaw, bestNanos uint64
	found := false
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
		if !found || tsN < bestNanos || (tsN == bestNanos && rawKey < bestRaw) {
			found = true
			bestNanos = tsN
			bestRaw = rawKey
		}
	}
	if !found {
		return 0, 0, protocol.HistoryBackfillOpts{}, false
	}
	return bestNanos, bestRaw, o, true
}
