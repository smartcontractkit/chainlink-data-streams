package llo

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// StateTransition mutates the replicated KeyValueState based on the round's
// observations and returns a self-sufficient precursor for Reports.
//
// This is a faithful port of the core of the v30 Outcome computation, adapted
// to read/write per-channel keys in the KeyValueState instead of decoding and
// re-encoding a monolithic previous outcome.
func (p *Plugin) StateTransition(ctx context.Context, seqNr uint64, _ ocrtypes.AttributedQuery, aos []ocrtypes.AttributedObservation, kvRW ocr3_1types.KeyValueStateReadWriter, bf ocr3_1types.BlobFetcher) (ocr3_1types.ReportsPlusPrecursor, error) {
	if len(aos) < 2*p.F+1 {
		return nil, fmt.Errorf("invariant violation: expected at least 2f+1 attributed observations, got %d (f: %d)", len(aos), p.F)
	}

	// Initial round: establish the lifecycle stage and initial states,
	if seqNr <= 1 {
		stage := protocol.LifeCycleStageProduction
		if p.PredecessorConfigDigest != nil {
			stage = protocol.LifeCycleStageStaging
		}
		if err := writeLifecycle(kvRW, stage); err != nil {
			return nil, err
		}
		if err := writeChannelState(kvRW, seqNr, nil); err != nil {
			return nil, err
		}
		if err := writeHotState(kvRW, 0, nil, nil, nil); err != nil {
			return nil, err
		}
		p.ChannelCache.invalidate()
		return encodePrecursor(precursor{LifeCycleStage: stage})
	}

	prev, err := loadKVState(kvRW, p.ChannelCache)
	if err != nil {
		return nil, fmt.Errorf("failed to load KV state: %w", err)
	}

	timestamps, validPredecessorRetirementReport, shouldRetireVotes, removeChannelVotesByID, updateDefsByHash, updateVotesByHash, streamObservations, err := p.decodeObservations(ctx, aos, seqNr, bf)
	if err != nil {
		return nil, err
	}
	if len(timestamps) == 0 {
		return nil, fmt.Errorf("no valid observations")
	}

	out := precursor{
		ObservationTimestampNanoseconds: medianTimestamp(timestamps),
		ChannelDefinitions:              cloneChannelDefinitions(prev.channelDefinitions),
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{},
		StreamAggregates:                protocol.StreamAggregates{},
	}

	// Lifecycle stage & promotion.
	promotedValidAfter := map[llotypes.ChannelID]uint64(nil)
	if prev.lifeCycleStage == protocol.LifeCycleStageStaging && validPredecessorRetirementReport != nil {
		p.Logger.Infow("Promoting protocol instance from staging to production 🎖️", "seqNr", seqNr, "validAfterNanoseconds", validPredecessorRetirementReport.ValidAfterNanoseconds)
		out.LifeCycleStage = protocol.LifeCycleStageProduction
		promotedValidAfter = validPredecessorRetirementReport.ValidAfterNanoseconds
	} else {
		out.LifeCycleStage = prev.lifeCycleStage
	}
	if out.LifeCycleStage == protocol.LifeCycleStageProduction && shouldRetireVotes > p.F {
		p.Logger.Infow("Retiring production protocol instance ⚰️", "seqNr", seqNr)
		out.LifeCycleStage = protocol.LifeCycleStageRetired
	}

	// Keep the node-local OptsCache in sync with the channel set. It is decode
	// memoization, not replicated state, but it is READ at consensus time by
	// calculated-stream evaluation and by opts-dependent report codecs, so it
	// must be populated identically on every oracle. After a mismatch (e.g. a
	// restart leaves it empty) repopulate it wholesale from the current channel
	// definitions; applyChannelVotes then keeps it current incrementally. This
	// mirrors the v30 Outcome() safeguard.
	if p.OptsCache.Len() != len(out.ChannelDefinitions) {
		p.OptsCache.ResetTo(out.ChannelDefinitions)
	}

	// Channel definition changes (skipped once retired).
	var removedChannelIDs []llotypes.ChannelID
	if out.LifeCycleStage != protocol.LifeCycleStageRetired {
		removedChannelIDs = applyChannelVotes(out.ChannelDefinitions, removeChannelVotesByID, updateDefsByHash, updateVotesByHash, p.F, p.OptsCache)
	}

	// validAfter.
	if promotedValidAfter != nil {
		// Promotion round: seed validAfter solely from the predecessor's
		// retirement report (gapless handover). Do NOT carry forward this
		// staging instance's own watermarks — staging channels absent from the
		// report were never covered by the predecessor's production reports, so
		// they fall through to the new-channel loop below (validAfter = obsTs).
		// This mirrors v30, which replaces ValidAfterNanoseconds wholesale with
		// the report's map and skips carry-forward during promotion.
		for id, va := range promotedValidAfter {
			out.ValidAfterNanoseconds[id] = va
		}
	} else {
		for channelID, prevValidAfter := range prev.validAfterNanoseconds {
			if _, done := out.ValidAfterNanoseconds[channelID]; done {
				continue
			}
			if cd, ok := prev.channelDefinitions[channelID]; ok && cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
				// Backfill: the watermark advances to whatever observation the
				// previous round would have selected (and emitted), or stays put.
				if tsNanos, _, _, found := selectBackfillCandidate(prev.channelDefinitions, prev.validAfterNanoseconds, prev.observationTimestampNs, channelID); found {
					out.ValidAfterNanoseconds[channelID] = tsNanos
				} else {
					out.ValidAfterNanoseconds[channelID] = prevValidAfter
				}
				continue
			}
			if prevReportable(prev, channelID) {
				// Previous round reported; advance to the previous observation timestamp.
				out.ValidAfterNanoseconds[channelID] = prev.observationTimestampNs
			} else {
				out.ValidAfterNanoseconds[channelID] = prevValidAfter
			}
		}
	}
	for channelID := range out.ChannelDefinitions {
		if _, ok := out.ValidAfterNanoseconds[channelID]; !ok {
			if out.ChannelDefinitions[channelID].ReportFormat == llotypes.ReportFormatHistoryBackfill {
				// New backfill channel: watermark starts at 0 (before any observation).
				out.ValidAfterNanoseconds[channelID] = 0
			} else {
				// New channel; becomes reportable in later rounds.
				out.ValidAfterNanoseconds[channelID] = out.ObservationTimestampNanoseconds
			}
		}
	}
	for _, channelID := range removedChannelIDs {
		delete(out.ValidAfterNanoseconds, channelID)
	}

	// Aggregation (regular fresh; timestamped with cross-round carry-forward via
	// the r/agg record). carryForward accumulates the values to persist for the
	// next round.
	carryForward := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
	if err := p.aggregate(prev.carryForward, carryForward, out.ChannelDefinitions, streamObservations, out.StreamAggregates); err != nil {
		return nil, err
	}

	// Evaluate calculated streams (EVMABIEncodeUnpackedExpr channels): appends
	// the calculated streams to their channel definitions and writes the
	// evaluated values into StreamAggregates. The engine is shared via
	// llo/protocol/calculated.
	calculated.ProcessCalculatedStreams(p.Logger, out.ChannelDefinitions, out.StreamAggregates, out.ObservationTimestampNanoseconds, p.OptsCache)

	// Flush KV mutations.
	if err := p.flushKV(kvRW, seqNr, prev, out, carryForward); err != nil {
		return nil, err
	}

	if p.Config.VerboseLogging {
		p.Logger.Debugw("Generated precursor", "lifeCycleStage", out.LifeCycleStage, "channels", len(out.ChannelDefinitions), "seqNr", seqNr)
	}
	p.captureOutcomeTelemetry(out, seqNr)
	return encodePrecursor(out)
}

func (p *Plugin) decodeObservations(ctx context.Context, aos []ocrtypes.AttributedObservation, seqNr uint64, bf ocr3_1types.BlobFetcher) (
	timestampsNanoseconds []uint64,
	validPredecessorRetirementReport *protocol.RetirementReport,
	shouldRetireVotes int,
	removeChannelVotesByID map[llotypes.ChannelID]int,
	updateChannelDefinitionsByHash map[[32]byte]protocol.ChannelDefinitionWithID,
	updateChannelVotesByHash map[[32]byte]int,
	streamObservations map[llotypes.StreamID][]protocol.StreamValue,
	err error,
) {
	removeChannelVotesByID = make(map[llotypes.ChannelID]int)
	updateChannelDefinitionsByHash = make(map[[32]byte]protocol.ChannelDefinitionWithID)
	updateChannelVotesByHash = make(map[[32]byte]int)
	streamObservations = make(map[llotypes.StreamID][]protocol.StreamValue)

	for _, ao := range aos {
		observation, derr := decodeObservation(ctx, ao.Observation, bf)
		if derr != nil {
			var bfErr *blobFetchError
			if errors.As(derr, &bfErr) {
				// Node-local, possibly transient failure. Dropping this
				// observation on only some oracles would make StateTransition
				// non-deterministic; instead abort the round so every oracle
				// retries uniformly. Determinism is not required when returning
				// an error (see the ReportingPlugin contract).
				err = fmt.Errorf("failed to fetch blob for observation from oracle %v: %w", ao.Observer, derr)
				return
			}
			// Deterministic decode failure (same bytes on every oracle): safe to
			// drop just this observation.
			p.Logger.Warnw("ignoring invalid observation", "oracleID", ao.Observer, "error", derr)
			continue
		}

		if len(observation.AttestedPredecessorRetirement) != 0 && validPredecessorRetirementReport == nil && p.PredecessorConfigDigest != nil {
			pcd := *p.PredecessorConfigDigest
			retirementReport, cerr := p.PredecessorRetirementReportCache.CheckAttestedRetirementReport(pcd, observation.AttestedPredecessorRetirement)
			if cerr != nil {
				p.Logger.Warnw("ignoring observation with invalid attested predecessor retirement", "oracleID", ao.Observer, "error", cerr, "predecessorConfigDigest", pcd)
				continue
			}
			validPredecessorRetirementReport = &retirementReport
		}

		if observation.ShouldRetire {
			shouldRetireVotes++
		}
		timestampsNanoseconds = append(timestampsNanoseconds, observation.UnixTimestampNanoseconds)

		for channelID := range observation.RemoveChannelIDs {
			removeChannelVotesByID[channelID]++
		}
		for channelID, channelDefinition := range observation.UpdateChannelDefinitions {
			defWithID := protocol.ChannelDefinitionWithID{ChannelDefinition: channelDefinition, ChannelID: channelID}
			h := makeChannelHash(defWithID)
			updateChannelVotesByHash[h]++
			updateChannelDefinitionsByHash[h] = defWithID
		}
		for id, sv := range observation.StreamValues {
			if sv == nil {
				continue
			}
			streamObservations[id] = append(streamObservations[id], sv)
		}
	}
	return
}

// applyChannelVotes applies remove/add votes with a >F threshold, in ascending
// channelID order, respecting MaxOutcomeChannelDefinitionsLength. Returns the
// channel IDs that were removed.
func applyChannelVotes(
	defs llotypes.ChannelDefinitions,
	removeVotesByID map[llotypes.ChannelID]int,
	updateDefsByHash map[[32]byte]protocol.ChannelDefinitionWithID,
	updateVotesByHash map[[32]byte]int,
	f int,
	optsCache *protocol.OptsCache,
) []llotypes.ChannelID {
	removed := make([]llotypes.ChannelID, 0, len(removeVotesByID))
	for channelID, voteCount := range removeVotesByID {
		if voteCount <= f {
			continue
		}
		removed = append(removed, channelID)
		delete(defs, channelID)
		if optsCache != nil {
			optsCache.Remove(channelID)
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })

	type hashWithID struct {
		hash [32]byte
		def  protocol.ChannelDefinitionWithID
	}
	ordered := make([]hashWithID, 0, len(updateDefsByHash))
	for h, d := range updateDefsByHash {
		ordered = append(ordered, hashWithID{h, d})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].def.ChannelID < ordered[j].def.ChannelID })
	for _, hwid := range ordered {
		if updateVotesByHash[hwid.hash] <= f {
			continue
		}
		defWithID := hwid.def
		_, exists := defs[defWithID.ChannelID]
		if !exists && len(defs) >= protocol.MaxOutcomeChannelDefinitionsLength {
			// Skip additions beyond the cap; a replacement of an existing channel is still fine.
			continue
		}
		defs[defWithID.ChannelID] = defWithID.ChannelDefinition
		if optsCache != nil {
			optsCache.Set(defWithID.ChannelID, defWithID.Opts)
		}
	}
	return removed
}

// aggregate computes stream aggregates for all non-tombstone, non-backfill
// channels, one aggregation per (streamID, aggregator) pair.
//
// Timestamped stream values carry forward across rounds via the r/agg record
// with newer-wins monotonicity (mirroring v30): the previous value is kept when
// the fresh aggregation is older or fails, and only a strictly-newer value is
// adopted. Regular (non-timestamped) aggregates are recomputed fresh each round
// and never persisted.
//
// prevCarry holds the previous round's carry-forward values (read-only);
// nextCarry is populated with the values to persist for the next round. A pair
// that is not written into nextCarry is dropped from the store, which is how
// carry-forward values orphaned by channel removal or tombstoning are
// reclaimed.
func (p *Plugin) aggregate(
	prevCarry, nextCarry map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue,
	defs llotypes.ChannelDefinitions,
	streamObservations map[llotypes.StreamID][]protocol.StreamValue,
	out protocol.StreamAggregates,
) error {
	keep := func(sid llotypes.StreamID, agg llotypes.Aggregator, tsv *protocol.TimestampedStreamValue) {
		if nextCarry[sid] == nil {
			nextCarry[sid] = map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
		}
		nextCarry[sid][agg] = tsv
	}

	for _, cd := range defs {
		if cd.Tombstone || cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			// Not aggregated, so nothing is carried forward on their behalf. A
			// pair that some other live channel still aggregates is preserved by
			// that channel.
			continue
		}
		for _, strm := range cd.Streams {
			sid, agg := strm.StreamID, strm.Aggregator
			if agg == llotypes.AggregatorCalculated {
				continue // handled after aggregation by ProcessCalculatedStreams
			}
			if _, exists := out[sid][agg]; exists {
				continue
			}
			m, exists := out[sid]
			if !exists {
				m = make(map[llotypes.Aggregator]protocol.StreamValue)
				out[sid] = m
			}

			prevTSV := prevCarry[sid][agg]

			aggF := protocol.GetAggregatorFunc(agg)
			if aggF == nil {
				return fmt.Errorf("no aggregator function defined for aggregator of type %v", agg)
			}
			result, aerr := aggF(streamObservations[sid], p.F)

			switch v := result.(type) {
			case *protocol.TimestampedStreamValue:
				if aerr != nil {
					// Aggregation failed: keep the carried-forward value (if any).
					if prevTSV != nil {
						m[agg] = prevTSV
						keep(sid, agg, prevTSV)
					}
					continue
				}
				if prevTSV == nil || v.ObservedAtNanoseconds > prevTSV.ObservedAtNanoseconds {
					// Strictly newer: adopt and persist.
					m[agg] = v
					keep(sid, agg, v)
				} else {
					// Not newer: keep the previous value (monotonic).
					m[agg] = prevTSV
					keep(sid, agg, prevTSV)
				}
			default:
				if aerr != nil {
					// Ignore streams that cannot be aggregated; absent from the
					// precursor. A previously-carried value for this pair is
					// preserved so a transient aggregation failure does not
					// discard it.
					if prevTSV != nil {
						keep(sid, agg, prevTSV)
					}
					continue
				}
				m[agg] = result
				// Defensive: if this pair was previously timestamped but now
				// yields a non-timestamped value, drop the stale carry-forward
				// by not writing it into nextCarry.
			}
		}
	}
	return nil
}

// flushKV persists the computed state. The per-round record (r/agg) is always
// rewritten; the channel record (c/defs, c/seqnr) and the lifecycle stage are
// written only when they actually change, so that readers can keep serving
// their in-memory copy of the definitions (see channelCache).
func (p *Plugin) flushKV(
	kvRW ocr3_1types.KeyValueStateReadWriter,
	seqNr uint64,
	prev *kvState,
	out precursor,
	carryForward map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue,
) error {
	if out.LifeCycleStage != prev.lifeCycleStage {
		if err := writeLifecycle(kvRW, out.LifeCycleStage); err != nil {
			return err
		}
	}

	// Channel definitions: rewrite the whole record if anything changed. Removed
	// channels disappear by not being part of out.ChannelDefinitions.
	if channelDefinitionsChanged(prev.channelDefinitions, out.ChannelDefinitions) {
		if err := writeChannelState(kvRW, seqNr, out.ChannelDefinitions); err != nil {
			return err
		}
		// The cache entry for prev.channelStateSeqNr is still valid for the
		// state this round read; the next round observes the new c/seqnr and
		// reloads.
	}

	// Reportability: persist this round's decision for each channel so the next
	// round can advance validAfter faithfully (see prevReportable).
	reportable := make(map[llotypes.ChannelID]bool, len(out.ChannelDefinitions))
	for id := range out.ChannelDefinitions {
		reportable[id] = out.isReportable(id, p.DefaultMinReportIntervalNanoseconds, p.OptsCache, p.Logger)
	}

	return writeHotState(kvRW, out.ObservationTimestampNanoseconds, out.ValidAfterNanoseconds, reportable, carryForward)
}

// channelDefinitionsChanged reports whether the channel set or any individual
// definition differs between two rounds.
func channelDefinitionsChanged(prev, next llotypes.ChannelDefinitions) bool {
	if len(prev) != len(next) {
		return true
	}
	for id, cd := range next {
		prevCd, ok := prev[id]
		if !ok || !prevCd.Equals(cd) {
			return true
		}
	}
	return false
}

// prevReportable reports whether the channel was reportable in the previous
// round. This reads the reportability decision persisted by the previous
// round's StateTransition, which already accounts for min-interval,
// seconds-resolution overlap, and DisableNilStreamValues. It is exactly
// the value the v30 code derives from previousOutcome.IsReportable.
func prevReportable(prev *kvState, channelID llotypes.ChannelID) bool {
	return prev.reportedLastRound[channelID]
}

func medianTimestamp(timestampsNanoseconds []uint64) uint64 {
	sort.Slice(timestampsNanoseconds, func(i, j int) bool { return timestampsNanoseconds[i] < timestampsNanoseconds[j] })
	return timestampsNanoseconds[len(timestampsNanoseconds)/2]
}

func makeChannelHash(cd protocol.ChannelDefinitionWithID) [32]byte {
	pb := &protocol.LLOChannelIDAndDefinitionProto{
		ChannelID:         cd.ChannelID,
		ChannelDefinition: protocol.ChannelDefinitionToProto(cd.ChannelDefinition),
	}
	b, err := deterministicMarshal.Marshal(pb)
	if err != nil {
		// Marshaling a well-formed definition cannot fail; hash empty on the
		// impossible error path rather than panicking.
		return sha256.Sum256(nil)
	}
	return sha256.Sum256(b)
}

func sortChannelIDs(cids []llotypes.ChannelID) {
	sort.Slice(cids, func(i, j int) bool { return cids[i] < cids[j] })
}

func cloneChannelDefinitions(in llotypes.ChannelDefinitions) llotypes.ChannelDefinitions {
	out := make(llotypes.ChannelDefinitions, len(in))
	for id, cd := range in {
		out[id] = cd
	}
	return out
}
