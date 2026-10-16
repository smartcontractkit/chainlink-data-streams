package llo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// StateTransition mutates the replicated KeyValueState based on the round's
// observations and returns a self-sufficient precursor for Reports.
//
// Channel definition changes agreed in this round take effect in the NEXT
// round. Two sets are carried:
//
//   - effective: the definitions committed by the previous round, i.e. exactly
//     what Observation(seqNr) read and gathered stream values for. Aggregation,
//     calculated streams, reportability, validAfter and the precursor (and
//     therefore Reports) all run against this set.
//   - pending: effective plus this round's agreed additions, updates, removals
//     and tombstones. It is what gets persisted to c/defs, so it becomes the
//     effective set of the next round.
//
// This keeps the observed stream values, the channel definitions and the
// decoded channel opts consistent with one another across Observation,
// StateTransition and Reports: no report is ever encoded under a definition
// (or with opts) that the observations behind it did not match. Lifecycle
// changes are NOT deferred - retirement must stop reporting in the round it is
// agreed.
//
// This is a port of the core of the v30 Outcome computation, adapted to the
// KeyValueState and to the deferred-definitions rule above (v30 applies
// definition changes within the same round).
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
		if err := writeHotState(kvRW, 0, nil, nil, nil, p.Logger); err != nil {
			return nil, err
		}
		return encodePrecursor(precursor{LifeCycleStage: stage})
	}

	prev, err := loadKVState(kvRW, p.ChannelCache)
	if err != nil {
		return nil, fmt.Errorf("failed to load KV state: %w", err)
	}

	tally, err := p.decodeObservations(ctx, aos, bf, p.BlobPayloads.round(seqNr), p.collectAttributedObservations(seqNr))
	if err != nil {
		return nil, err
	}
	if len(tally.timestampsNanoseconds) < 2*p.F+1 {
		return nil, fmt.Errorf("invariant violation: expected at least 2f+1 decoded observations, got %d (f: %d)", len(tally.timestampsNanoseconds), p.F)
	}

	// Verifying an attested predecessor retirement report needs the
	// predecessor's signer set, which is node-local. Agree on it from this
	// round's votes, then verify against the agreed set, so every oracle
	// reaches the same verdict.
	validPredecessorRetirementReport := p.resolvePredecessorRetirement(seqNr, prev.lifeCycleStage, tally)

	// Codec coverage is cumulative across rounds: merge this round's
	// advertisements over the persisted ones before counting supporters.
	codecSupport := mergeCodecSupport(prev.codecSupport, tally.supportedFormatsByOracle)

	// The definitions in effect for this round are the ones Observation read.
	// Changes agreed below land in pending and take effect next round.
	effective := cloneChannelDefinitions(prev.channelDefinitions)
	pending := cloneChannelDefinitions(prev.channelDefinitions)

	out := precursor{
		ObservationTimestampNanoseconds: p.agreedObservationTimestamp(tally.timestampsNanoseconds, prev.observationTimestampNs, seqNr),
		ChannelDefinitions:              effective,
		ChannelStateSeqNr:               prev.channelStateSeqNr,
		ValidAfterNanoseconds:           map[llotypes.ChannelID]uint64{},
		StreamAggregates:                protocol.StreamAggregates{},
		SupportByFormat:                 supportVotesForEffectiveFormats(countSupportByFormat(codecSupport), effective),
	}
	p.warnIfAheadOfLocalClock(out.ObservationTimestampNanoseconds, seqNr)

	// Lifecycle stage & promotion.
	promotedValidAfter := map[llotypes.ChannelID]uint64(nil)
	if prev.lifeCycleStage == protocol.LifeCycleStageStaging && validPredecessorRetirementReport != nil {
		p.Logger.Infow("Promoting protocol instance from staging to production 🎖️", "seqNr", seqNr, "validAfterNanoseconds", validPredecessorRetirementReport.ValidAfterNanoseconds)
		out.LifeCycleStage = protocol.LifeCycleStageProduction
		promotedValidAfter = validPredecessorRetirementReport.ValidAfterNanoseconds
	} else {
		out.LifeCycleStage = prev.lifeCycleStage
	}
	// ShouldRetire is a monotone onchain flag per config digest, so f+1 votes
	// imply at least one honest oracle read it set. Requiring more would let f
	// faulty oracles block retirement whenever only 2f+1 observations arrive.
	if out.LifeCycleStage == protocol.LifeCycleStageProduction && tally.shouldRetireVotes > p.F {
		p.Logger.Infow("Retiring production protocol instance ⚰️", "seqNr", seqNr)
		out.LifeCycleStage = protocol.LifeCycleStageRetired
	}

	// Channel definition changes (skipped once retired). These apply to pending
	// only: they take effect next round.
	if out.LifeCycleStage != protocol.LifeCycleStageRetired {
		applyChannelVotes(p.ReportCodecs, p.ChannelAnalysisCache.ReadOnly(), pending, tally.removeChannelVotesByID, tally.updateChannelDefinitionsByHash, tally.updateChannelVotesByHash, p.F)
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
			cd, stillInEffect := effective[channelID]
			if !stillInEffect {
				// A removal agreed in the previous round takes effect now: drop
				// the watermark along with the channel.
				continue
			}
			if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
				if prev.lifeCycleStage != protocol.LifeCycleStageProduction {
					// The previous round was staging, so it emitted no backfill
					// report (see isReportable) and the watermark must not move.
					// Advancing it here would silently consume backfill
					// observations that were never emitted.
					out.ValidAfterNanoseconds[channelID] = prevValidAfter
					continue
				}
				// Backfill: prevReportable and selection conditions must be met, or stays put.
				out.ValidAfterNanoseconds[channelID] = prevValidAfter
				if prevReportable(prev, channelID) {
					if tsNanos, _, _, _, found := selectBackfillCandidate(effective, prev.validAfterNanoseconds, prev.observationTimestampNs, channelID, prev.opts); found {
						out.ValidAfterNanoseconds[channelID] = tsNanos
					}
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
	// A channel added in a previous round becomes effective now and gets its
	// first watermark, which keeps it unreportable for this round.
	for channelID, cd := range effective {
		if _, ok := out.ValidAfterNanoseconds[channelID]; !ok {
			if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
				// New backfill channel: watermark starts at 0 (before any observation).
				out.ValidAfterNanoseconds[channelID] = 0
			} else {
				// New channel; becomes reportable in later rounds.
				out.ValidAfterNanoseconds[channelID] = out.ObservationTimestampNanoseconds
			}
		}
	}

	// Stream history: derive the depth each (stream, aggregator) pair needs from
	// the (replicated) channel definitions and their expressions, then set it on
	// the round's store. This happens before aggregation because the depth is
	// what decides whether a pair's value is recorded at all.
	history, err := newHistoryStore(kvRW, p.Logger)
	if err != nil {
		return nil, err
	}
	requirements := computeHistoryRequirements(effective, prev.opts, history.indexKeys(), p.Logger)
	if err := requirements.apply(history); err != nil {
		return nil, err
	}
	if denied := requirements.sortedDenied(); len(denied) > 0 {
		// Channels reading these pairs cannot evaluate and so will not report.
		// One line per round, however many pairs are denied.
		pairs := make([]string, len(denied))
		for i, key := range denied {
			pairs[i] = fmt.Sprintf("%d/%d", key.streamID, key.aggregator)
		}
		p.Logger.Errorw("Stream history denied; channels reading it will not report",
			"denied", len(denied), "pairs", pairs, "maxPairs", protocol.MaxHistoryPairs, "seqNr", seqNr)
	}

	// Aggregation (regular fresh; timestamped with cross-round carry-forward via
	// the r/agg record). carryForward accumulates the values to persist for the
	// next round. Runs over the effective set, which is what was observed. The
	// agreed value of every pair history requires is recorded as it is computed.
	carryForward := map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue{}
	if err := p.aggregate(prev.carryForward, carryForward, effective, tally.streamObservations, out.StreamAggregates,
		history, requirements, out.ObservationTimestampNanoseconds); err != nil {
		return nil, err
	}

	// Evaluate calculated streams (EVMABIEncodeUnpackedExpr channels): writes
	// the evaluated values into StreamAggregates. The engine is shared via
	// llo/protocol/calculated. The history store is the read side: expressions
	// using History(...) read the windows appended above, and are left
	// unevaluated while a window is still shallower than requested.
	//
	// The channel definitions are not touched. Which calculated streams a
	// channel reports is derived from its opts by protocol.EffectiveStreams, so
	// nothing about evaluation reaches replicated state and a persisted
	// definition stays exactly what was voted on.
	p.ExpressionPins.Set(prev.opts, effective)
	calculated.ProcessCalculatedStreams(p.Logger, effective, out.StreamAggregates, out.ObservationTimestampNanoseconds, prev.opts, history)

	// Flush KV mutations.
	if err := p.flushKV(kvRW, seqNr, prev, out, pending, codecSupport, carryForward, history); err != nil {
		return nil, err
	}

	if p.Config.VerboseLogging {
		p.Logger.Debugw("Generated precursor", "lifeCycleStage", out.LifeCycleStage, "channels", len(out.ChannelDefinitions), "seqNr", seqNr)
	}
	// After the flush, so the recorded window sizes are the ones actually
	// written. Node-local observation only; nothing reads these back.
	p.captureHistoryTelemetry(history, requirements)
	p.captureInsufficientHistory(effective, prev.opts, history)
	p.captureOutcomeTelemetry(out, seqNr)
	p.captureAttributedObservationTelemetry(out, seqNr, tally.attributed)
	return encodePrecursor(out)
}

// observationTally is what the round's observations add up to: the votes,
// timestamps and stream values the state transition works from. Every field is
// accumulated in aos order, so it does not depend on which observation decoded
// first.
type observationTally struct {
	timestampsNanoseconds []uint64
	// attestedRetirements are collected but not verified: verification needs
	// the agreed predecessor signer set, which the votes below decide.
	attestedRetirements            [][]byte
	predConfigsByHash              map[[32]byte]predecessorConfig
	predConfigVotesByHash          map[[32]byte]int
	shouldRetireVotes              int
	removeChannelVotesByID         map[llotypes.ChannelID]int
	updateChannelDefinitionsByHash map[[32]byte]protocol.ChannelDefinitionWithID
	updateChannelVotesByHash       map[[32]byte]int
	// supportedFormatsByOracle[oracleID] is the formats that oracle advertised
	// this round, deduped by decodeObservation. It is merged into the persisted
	// per-oracle record rather than counted here: see writeCodecSupport.
	supportedFormatsByOracle map[commontypes.OracleID]map[llotypes.ReportFormat]struct{}
	streamObservations       map[llotypes.StreamID][]protocol.StreamValue
	// attributed is every observation as decoded, in aos order. Collected only
	// for attributed observation telemetry.
	attributed []attributedObservation
}

func (p *Plugin) decodeObservations(ctx context.Context, aos []ocrtypes.AttributedObservation, bf ocr3_1types.BlobFetcher, memo *roundBlobPayloads, collect bool) (observationTally, error) {
	tally := observationTally{
		predConfigsByHash:              make(map[[32]byte]predecessorConfig),
		predConfigVotesByHash:          make(map[[32]byte]int),
		removeChannelVotesByID:         make(map[llotypes.ChannelID]int),
		updateChannelDefinitionsByHash: make(map[[32]byte]protocol.ChannelDefinitionWithID),
		updateChannelVotesByHash:       make(map[[32]byte]int),
		supportedFormatsByOracle:       make(map[commontypes.OracleID]map[llotypes.ReportFormat]struct{}),
		streamObservations:             make(map[llotypes.StreamID][]protocol.StreamValue),
	}

	// Decode concurrently: each observation may reference blobs that are not yet
	// assembled locally, and waiting for one serially delays every other. The
	// tally below still runs in aos order, so the outcome does not depend on
	// which decode finished first.
	decoded := make([]Observation, len(aos))
	decodeErrs := make([]error, len(aos))
	var wg sync.WaitGroup
	wg.Add(len(aos))
	for i, ao := range aos {
		go func() {
			defer wg.Done()
			decoded[i], decodeErrs[i] = decodeObservation(ctx, ao.Observation, bf, memo)
		}()
	}
	wg.Wait()

	for i, ao := range aos {
		observation, derr := decoded[i], decodeErrs[i]
		if derr != nil {
			var bfErr *blobFetchError
			if errors.As(derr, &bfErr) {
				// Node-local, possibly transient failure. Dropping this
				// observation on only some oracles would make StateTransition
				// non-deterministic, so fail instead. Determinism is not
				// required when returning an error (see the ReportingPlugin
				// contract). libocr does not retry: this oracle sends no
				// Prepare for the round and adopts the committed state through
				// state sync. If more than f oracles fail, the round does not
				// commit and a new epoch starts it over.
				return observationTally{}, fmt.Errorf("failed to fetch blob for observation from oracle %v: %w", ao.Observer, derr)
			}
			// Unreachable while libocr runs ValidateObservation, which decodes
			// the same bytes, before StateTransition. Drop rather than fail, and
			// let the quorum check in StateTransition catch a shrunk sample.
			p.Logger.Warnw("ignoring invalid observation", "oracleID", ao.Observer, "error", derr)
			if collect {
				tally.attributed = append(tally.attributed, attributedObservation{observer: ao.Observer, decodeErr: derr})
			}
			continue
		}
		if collect {
			tally.attributed = append(tally.attributed, attributedObservation{observer: ao.Observer, obs: observation})
		}

		if p.PredecessorConfigDigest != nil {
			if len(observation.AttestedPredecessorRetirement) != 0 {
				tally.attestedRetirements = append(tally.attestedRetirements, observation.AttestedPredecessorRetirement)
			}
			if len(observation.PredecessorSigners) > 0 {
				pc := predecessorConfig{signers: observation.PredecessorSigners, f: observation.PredecessorF}
				h := hashPredecessorConfig(pc)
				tally.predConfigVotesByHash[h]++
				tally.predConfigsByHash[h] = pc
			}
		}

		if observation.ShouldRetire {
			tally.shouldRetireVotes++
		}
		tally.timestampsNanoseconds = append(tally.timestampsNanoseconds, observation.UnixTimestampNanoseconds)

		// Recording the whole advertised set (including an empty one)
		// makes this round's advertisement replace that oracle's last, so an
		// oracle that loses a codec stops counting for it.
		tally.supportedFormatsByOracle[ao.Observer] = observation.SupportedReportFormats
		for channelID := range observation.RemoveChannelIDs {
			tally.removeChannelVotesByID[channelID]++
		}
		for channelID, channelDefinition := range observation.UpdateChannelDefinitions {
			defWithID := protocol.ChannelDefinitionWithID{ChannelDefinition: channelDefinition, ChannelID: channelID}
			h := makeChannelHash(defWithID)
			tally.updateChannelVotesByHash[h]++
			tally.updateChannelDefinitionsByHash[h] = defWithID
		}
		for id, sv := range observation.StreamValues {
			if sv == nil {
				continue
			}
			tally.streamObservations[id] = append(tally.streamObservations[id], sv)
		}
	}
	return tally, nil
}

// mergeCodecSupport overlays this round's advertisements on the persisted ones,
// replacing the entry of every oracle that contributed an observation and
// leaving the rest untouched.
//
// Entries are never expired. An oracle that stops contributing keeps counting
// for its last advertised formats, but it is then faulty and counts against f.
// The 2f+1 supporter threshold still leaves f+1 honest live encoders whenever
// at most f oracles are faulty, and this state is scoped to a single protocol
// instance.
func mergeCodecSupport(persisted, thisRound map[commontypes.OracleID]map[llotypes.ReportFormat]struct{}) map[commontypes.OracleID]map[llotypes.ReportFormat]struct{} {
	merged := make(map[commontypes.OracleID]map[llotypes.ReportFormat]struct{}, len(persisted)+len(thisRound))
	for oracleID, formats := range persisted {
		merged[oracleID] = formats
	}
	for oracleID, formats := range thisRound {
		merged[oracleID] = formats
	}
	return merged
}

// codecSupportChanged reports whether any oracle's advertised set differs.
func codecSupportChanged(prev, next map[commontypes.OracleID]map[llotypes.ReportFormat]struct{}) bool {
	if len(prev) != len(next) {
		return true
	}
	for oracleID, nextFormats := range next {
		prevFormats, ok := prev[oracleID]
		if !ok || !maps.Equal(prevFormats, nextFormats) {
			return true
		}
	}
	return false
}

// countSupportByFormat counts, per report format, the oracles whose last
// advertisement named it.
func countSupportByFormat(support map[commontypes.OracleID]map[llotypes.ReportFormat]struct{}) map[llotypes.ReportFormat]int {
	counts := make(map[llotypes.ReportFormat]int)
	for _, formats := range support {
		for format := range formats {
			counts[format]++
		}
	}
	return counts
}

// supportVotesForEffectiveFormats restricts the codec support tally to the
// report formats this round can actually needs.
func supportVotesForEffectiveFormats(votes map[llotypes.ReportFormat]int, effective llotypes.ChannelDefinitions) map[llotypes.ReportFormat]int {
	pruned := make(map[llotypes.ReportFormat]int, len(votes))
	for _, cd := range effective {
		if n, ok := votes[cd.ReportFormat]; ok {
			pruned[cd.ReportFormat] = n
		}
	}
	return pruned
}

// applyChannelVotes applies remove/add votes with a >F threshold, in ascending
// channelID order, respecting MaxOutcomeChannelDefinitionsLength and the
// whole-set budgets of protocol.ChannelDefinitionsBudget.
//
// Each update passed admission on at least one honest oracle, but against that
// oracle's own desired set. Updates from oracles whose desired sets differ
// can each cross the threshold in the same round, and their union can exceed a
// budget none of them did, so the budgets are enforced here on the merged set.
//
// Removals apply before updates, so an update that crosses the threshold in
// the same round as a removal of the same channel wins. This is intended:
// only committed tombstones are voted out (see voteOnChannels), and the update
// is a newer definition from the owner, so the result is the same as reaping
// the tombstone and adding the definition in consecutive rounds.
//
// A channel with more than one definition past the threshold is left
// unchanged. Definitions carry no version, so the votes cannot tell which is
// newer. This happens when the owner publishes twice within one polling
// window, and any pick would flip each round until the caches converge, since
// oracles skip voting for the committed definition and the others vote the
// competing one back in.
//
// defs is the PENDING set: everything applied here takes effect next round. It
// deliberately does not touch the round's channel generation, whose definitions
// and decoded opts stay pinned to the effective set for the whole round.
//
// The budgets that depend on opts are measured through cache, which holds the
// committed definitions once Observation has verified them. It should be
// read-only, since the updates were proposed by peers. The budget is only
// measured when an update passes the threshold, so a round without changes
// decodes nothing.
func applyChannelVotes(
	codecs map[llotypes.ReportFormat]protocol.ReportCodec,
	cache *protocol.ChannelAnalysisCache,
	defs llotypes.ChannelDefinitions,
	removeVotesByID map[llotypes.ChannelID]int,
	updateDefsByHash map[[32]byte]protocol.ChannelDefinitionWithID,
	updateVotesByHash map[[32]byte]int,
	f int,
) {
	for channelID, voteCount := range removeVotesByID {
		if voteCount <= f {
			continue
		}
		delete(defs, channelID)
	}

	type hashWithID struct {
		hash [32]byte
		def  protocol.ChannelDefinitionWithID
	}
	ordered := make([]hashWithID, 0, len(updateDefsByHash))
	for h, d := range updateDefsByHash {
		ordered = append(ordered, hashWithID{h, d})
	}
	// Sort by (channelID, hash) so the cap and budgets see changes in the same
	// order on every oracle.
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].def.ChannelID != ordered[j].def.ChannelID {
			return ordered[i].def.ChannelID < ordered[j].def.ChannelID
		}
		return bytes.Compare(ordered[i].hash[:], ordered[j].hash[:]) < 0
	})
	passing := make(map[llotypes.ChannelID]int)
	for _, hwid := range ordered {
		if updateVotesByHash[hwid.hash] > f {
			passing[hwid.def.ChannelID]++
		}
	}
	var budget *protocol.ChannelDefinitionsBudget
	for _, hwid := range ordered {
		if updateVotesByHash[hwid.hash] <= f || passing[hwid.def.ChannelID] > 1 {
			continue
		}
		if budget == nil {
			budget = protocol.NewChannelDefinitionsBudget(codecs, defs, cache)
		}
		defWithID := hwid.def
		prev, exists := defs[defWithID.ChannelID]
		if !exists && len(defs) >= protocol.MaxOutcomeChannelDefinitionsLength {
			// Skip additions beyond the cap; a replacement of an existing channel is still fine.
			continue
		}
		var replaced *llotypes.ChannelDefinition
		if exists {
			replaced = &prev
		}
		if !budget.Replace(defWithID.ChannelID, replaced, defWithID.ChannelDefinition) {
			// Skip changes that grow the set past a whole-set budget; they are
			// voted on again in later rounds.
			continue
		}
		defs[defWithID.ChannelID] = defWithID.ChannelDefinition
	}
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
// A timestamped value more than maxTimestampedFutureSkew ahead of the round's
// consensus observation timestamp is never adopted, and a carried value that
// far ahead is dropped. Otherwise a single far-future timestamp would win the
// newer-wins comparison and pin the pair, and its history, indefinitely.
//
// prevCarry holds the previous round's carry-forward values (read-only);
// nextCarry is populated with the values to persist for the next round. A pair
// that is not written into nextCarry is dropped from the store, which is how
// carry-forward values orphaned by channel removal or tombstoning are
// reclaimed.
//
// The agreed value of each pair is also recorded into stream history for the
// pairs that require it. History records what the round actually agreed on,
// the same value written into StreamAggregates, so a window is always a series
// of values that reached consensus. A pair with no aggregate this round
// (aggregation failed, stream absent) contributes nothing: a gap in the series
// is honest, whereas repeating the previous value would silently weight it
// twice.
func (p *Plugin) aggregate(
	prevCarry, nextCarry map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue,
	defs llotypes.ChannelDefinitions,
	streamObservations map[llotypes.StreamID][]protocol.StreamValue,
	out protocol.StreamAggregates,
	history *historyStore,
	requirements historyRequirements,
	observationTimestampNanoseconds uint64,
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
				// Not observed, so nothing to carry forward. Definitions written
				// by older code may still list them inline; calculated values
				// are recomputed each round by ProcessCalculatedStreams.
				continue
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
			if prevTSV != nil && tooFarAhead(prevTSV, observationTimestampNanoseconds) {
				p.Logger.Errorw("Dropping carried-forward timestamped aggregate: timestamp too far ahead of consensus time",
					"streamID", sid, "aggregator", agg, "observedAtNanoseconds", prevTSV.ObservedAtNanoseconds,
					"observationTimestampNanoseconds", observationTimestampNanoseconds)
				prevTSV = nil
			}

			aggF := protocol.GetAggregatorFuncV31(agg)
			if aggF == nil {
				// Unknown aggregator, e.g. one added by a newer version. Admission
				// rejects these, but a committed definition must not halt the
				// protocol: skip the pair and carry forward what it had.
				if prevTSV != nil {
					keep(sid, agg, prevTSV)
				}
				continue
			}
			result, aerr := aggF(streamObservations[sid], p.minContributions())
			if aerr != nil {
				// Aggregation failed: republish and keep the carried-forward
				// value (if any) so a transient failure does not discard it.
				// Without a carry the pair is simply absent from the precursor.
				if prevTSV != nil {
					m[agg] = prevTSV
					keep(sid, agg, prevTSV)
				}
				continue
			}
			if result == nil {
				// An aggregator may agree on no value at all, e.g. mode with an
				// empty bucket. Leave the pair absent rather than writing a nil
				// into the precursor, and keep any carried value.
				if prevTSV != nil {
					m[agg] = prevTSV
					keep(sid, agg, prevTSV)
				}
				continue
			}

			switch v := result.(type) {
			case *protocol.TimestampedStreamValue:
				if tooFarAhead(v, observationTimestampNanoseconds) {
					// Treat as a failed aggregation: keep the carry and record
					// nothing into history.
					p.Logger.Errorw("Rejecting timestamped aggregate: timestamp too far ahead of consensus time",
						"streamID", sid, "aggregator", agg, "observedAtNanoseconds", v.ObservedAtNanoseconds,
						"observationTimestampNanoseconds", observationTimestampNanoseconds)
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
				m[agg] = result
				// Defensive: if this pair was previously timestamped but now
				// yields a non-timestamped value, drop the stale carry-forward
				// by not writing it into nextCarry.
			}

			if err := appendHistory(history, requirements, sid, agg, m[agg], observationTimestampNanoseconds, p.Logger); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxTimestampedFutureSkew bounds how far a timestamped aggregate may sit
// ahead of the round's consensus observation timestamp. It absorbs ordinary
// skew between data source timestamps and oracle clocks.
const maxTimestampedFutureSkew = uint64(10 * time.Second)

// tooFarAhead reports whether tsv is timestamped beyond the allowed skew past
// nowNanoseconds.
func tooFarAhead(tsv *protocol.TimestampedStreamValue, nowNanoseconds uint64) bool {
	return tsv.ObservedAtNanoseconds > nowNanoseconds+maxTimestampedFutureSkew
}

// appendHistory records a pair's agreed value for this round.
//
// The timestamp is the value's own observation time for timestamped aggregates
// and the round's consensus observation timestamp otherwise. Either way the
// append only takes effect if it is strictly newer than the newest stored
// record, which is what stops a carried-forward value from being counted once
// per round until it refreshes.
//
// A value too large to store is dropped with a loud log rather than failing the
// round: the round has already agreed on it, so halting over it would take the
// DON down for a data problem.
func appendHistory(history *historyStore, requirements historyRequirements, sid llotypes.StreamID, agg llotypes.Aggregator, value protocol.StreamValue, observationTimestampNanoseconds uint64, lggr logger.Logger) error {
	if history == nil || value == nil || !requirements.requires(sid, agg) {
		return nil
	}

	observedAt := observationTimestampNanoseconds
	if tsv, ok := value.(*protocol.TimestampedStreamValue); ok {
		observedAt = tsv.ObservedAtNanoseconds
	}

	_, err := history.Append(sid, agg, observedAt, value)
	if errors.Is(err, protocol.ErrHistoryRecordTooLarge) {
		history.oversized++
		lggr.Errorw("Dropping stream history record: value too large to store",
			"streamID", sid, "aggregator", agg, "err", err)
		return nil
	}
	return err
}

// flushKV persists the computed state. The per-round record (r/agg) is always
// rewritten; the channel record (c/defs, c/seqnr) and the lifecycle stage are
// written only when they actually change, so that readers can keep serving
// their in-memory copy of the definitions (see channelCache). Modified history
// windows are flushed alongside it.
func (p *Plugin) flushKV(
	kvRW ocr3_1types.KeyValueStateReadWriter,
	seqNr uint64,
	prev *kvState,
	out precursor,
	pending llotypes.ChannelDefinitions,
	codecSupport map[commontypes.OracleID]map[llotypes.ReportFormat]struct{},
	carryForward map[llotypes.StreamID]map[llotypes.Aggregator]*protocol.TimestampedStreamValue,
	history *historyStore,
) error {
	if out.LifeCycleStage != prev.lifeCycleStage {
		if err := writeLifecycle(kvRW, out.LifeCycleStage); err != nil {
			return err
		}
	}

	// Channel definitions: persist the PENDING set, which becomes the effective
	// set of the next round. Rewrite the whole record only if it changed;
	// removed channels disappear by not being part of pending.
	if channelDefinitionsChanged(prev.channelDefinitions, pending) {
		if err := writeChannelState(kvRW, seqNr, pending); err != nil {
			return err
		}
		// The cache entry for prev.channelStateSeqNr is still valid for the
		// state this round read; the next round observes the new c/seqnr and
		// reloads.
	}

	// Codec coverage: rewrite the record only when an oracle's advertised set
	// actually changed, which is rare outside a rollout.
	if codecSupportChanged(prev.codecSupport, codecSupport) {
		if err := writeCodecSupport(kvRW, codecSupport); err != nil {
			return err
		}
	}

	// Reportability: persist this round's decision for each channel so the next
	// round can advance validAfter faithfully (see prevReportable).
	reportable := make(map[llotypes.ChannelID]bool, len(out.ChannelDefinitions))
	for id := range out.ChannelDefinitions {
		// nill tally, Reports() will handle the logging
		reportable[id] = out.isReportable(id, p.DefaultMinReportIntervalNanoseconds, p.F, prev.opts, nil)
	}

	// Stream history: write modified windows, delete pairs no live channel
	// requires, and rewrite the history index if the stored set changed.
	if history != nil {
		if err := history.Flush(kvRW); err != nil {
			return err
		}
	}

	return writeHotState(kvRW, out.ObservationTimestampNanoseconds, out.ValidAfterNanoseconds, reportable, carryForward, p.Logger)
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
// seconds-resolution overlap, and nil stream values. It is exactly
// the value the v30 code derives from previousOutcome.IsReportable.
func prevReportable(prev *kvState, channelID llotypes.ChannelID) bool {
	return prev.reportedLastRound[channelID]
}

// agreedObservationTimestamp is the round observation timestamp, the median of
// the observed timestamps, held to the previous round's value as a floor.
//
// Monotonically increasing, validAfter advances to it, reportability requires the
// next round to exceed that watermark and appendHistory only records a value
// strictly newer than the newest stored one. A regression leaves every channel
// unreportable and silently drops history appends until the clock catches back up.
//
// The quorum is at least 2f+1 observations, so with at most f faulty oracles
// the median lies within the range of honest timestamps. An oracle with a
// broken clock counts as faulty: f byzantine oracles plus one fast clock can
// move the median ahead of real time, and the floor then holds every channel
// unreportable until real time catches up. No deterministic rule can exclude
// f+1 faults with this quorum, so the only defence is detection, see
// warnIfAheadOfLocalClock.
func (p *Plugin) agreedObservationTimestamp(timestampsNanoseconds []uint64, prevObservationTimestampNs uint64, seqNr uint64) uint64 {
	median := medianTimestamp(timestampsNanoseconds)
	if median < prevObservationTimestampNs {
		p.Logger.Warnw("Observation timestamp median regressed; holding the previous round's timestamp",
			"seqNr", seqNr, "median", median, "prev", prevObservationTimestampNs, "contributors", len(timestampsNanoseconds))
		return prevObservationTimestampNs
	}
	return median
}

// maxConsensusClockLead bounds how far the agreed observation timestamp may
// sit ahead of this node's clock before it is reported. It sits well above
// NTP skew between oracle clocks, and low enough that a forward jump costing
// more than a few rounds of reporting is caught.
const maxConsensusClockLead = uint64(5 * time.Second)

// warnIfAheadOfLocalClock logs when the agreed observation timestamp is ahead
// of this node's clock by more than maxConsensusClockLead. It reads the local
// clock, so it must only produce side effects and never feed the outcome.
// Replayed rounds carry past timestamps and do not trigger it.
func (p *Plugin) warnIfAheadOfLocalClock(agreedNanoseconds uint64, seqNr uint64) {
	nowNanoseconds := uint64(max(time.Now().UnixNano(), 0))
	if aheadOfClock(agreedNanoseconds, nowNanoseconds) {
		p.Logger.Errorw("Agreed observation timestamp is ahead of the local clock; either this node's clock is behind or more than f oracles reported fast timestamps",
			"seqNr", seqNr, "agreed", agreedNanoseconds, "local", nowNanoseconds, "lead", time.Duration(agreedNanoseconds-nowNanoseconds))
	}
}

// aheadOfClock reports whether agreedNanoseconds leads nowNanoseconds by more
// than maxConsensusClockLead.
func aheadOfClock(agreedNanoseconds, nowNanoseconds uint64) bool {
	return agreedNanoseconds > nowNanoseconds+maxConsensusClockLead
}

func medianTimestamp(timestampsNanoseconds []uint64) uint64 {
	sort.Slice(timestampsNanoseconds, func(i, j int) bool { return timestampsNanoseconds[i] < timestampsNanoseconds[j] })
	return timestampsNanoseconds[len(timestampsNanoseconds)/2]
}

// makeChannelHash delegates to the shared implementation so that v3.0 running
// protocol version 2 and v3.1 cannot drift apart on channel identity.
// predecessorConfig is a candidate predecessor instance's signer set and f,
// which is what verifying an attested predecessor retirement report needs. It
// is agreed by vote within a round and never persisted.
type predecessorConfig struct {
	signers [][]byte
	f       uint8
}

// hashPredecessorConfig identifies a candidate predecessor config so votes for
// the same one can be tallied. Signer order is part of the identity: a
// signature names its signer by index, so two sets differing only in order are
// different configs.
func hashPredecessorConfig(pc predecessorConfig) [32]byte {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(pc.f))
	h.Write(buf[:])
	for _, signer := range pc.signers {
		binary.BigEndian.PutUint64(buf[:], uint64(len(signer)))
		h.Write(buf[:])
		h.Write(signer)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// resolvePredecessorRetirement agrees on the predecessor's signer set from
// this round's votes, then verifies this round's attested retirement reports
// against it.
//
// Agreement holds for this round only and is never stored. More than f votes
// for the same set means at least one honest oracle vouches for it, and that
// argument is per round: a coalition of f can never elect a set of its own, in
// this round or any later one, and nothing accumulates between rounds for it
// to build on.
//
// Everything here reads the round's observations only, so every oracle reaches
// the same verdict. A report that fails verification is ignored, not fatal:
// the bytes are the same everywhere, so ignoring them is deterministic too.
func (p *Plugin) resolvePredecessorRetirement(
	seqNr uint64,
	stage llotypes.LifeCycleStage,
	tally observationTally,
) *protocol.RetirementReport {
	// Only a staging instance with a predecessor has a handover to complete.
	if p.PredecessorConfigDigest == nil || stage != protocol.LifeCycleStageStaging {
		return nil
	}

	agreed := electPredecessorConfig(tally.predConfigsByHash, tally.predConfigVotesByHash, p.F)
	if agreed == nil {
		if len(tally.attestedRetirements) > 0 {
			p.Logger.Warnw("Ignoring attested predecessor retirement reports: the predecessor config is not agreed this round", "seqNr", seqNr, "reports", len(tally.attestedRetirements))
		}
		return nil
	}

	// A retired predecessor freezes its watermarks, so every report it signs
	// after retiring carries the same map and the aos order cannot matter. If
	// valid reports disagree, that invariant is broken and the order would pick
	// the watermarks: refuse to promote rather than let the leader choose.
	var chosen *protocol.RetirementReport
	verified := make(map[string]struct{}, len(tally.attestedRetirements))
	for _, attested := range tally.attestedRetirements {
		// Honest oracles usually attach the same bytes: verify them once.
		if _, seen := verified[string(attested)]; seen {
			continue
		}
		verified[string(attested)] = struct{}{}

		retirementReport, verr := p.PredecessorRetirementReportCache.VerifyAttestedRetirementReport(*p.PredecessorConfigDigest, agreed.signers, agreed.f, attested)
		if verr != nil {
			p.Logger.Warnw("Ignoring invalid attested predecessor retirement", "seqNr", seqNr, "error", verr, "predecessorConfigDigest", *p.PredecessorConfigDigest)
			continue
		}
		if verr = retirementReport.CheckCompatible(p.ProtocolVersion); verr != nil {
			p.Logger.Warnw("Ignoring incompatible attested predecessor retirement", "seqNr", seqNr, "error", verr, "predecessorConfigDigest", *p.PredecessorConfigDigest)
			continue
		}
		if chosen == nil {
			chosen = &retirementReport
			continue
		}
		if chosen.ProtocolVersion != retirementReport.ProtocolVersion || !maps.Equal(chosen.ValidAfterNanoseconds, retirementReport.ValidAfterNanoseconds) {
			p.Logger.Errorw("Refusing promotion: valid attested predecessor retirements disagree", "seqNr", seqNr, "predecessorConfigDigest", *p.PredecessorConfigDigest)
			return nil
		}
	}
	return chosen
}

// electPredecessorConfig returns the candidate with more than f votes, or nil.
//
// More than f votes means at least one honest oracle voted for the winner, and
// honest oracles read the set from the predecessor's onchain config, which is
// immutable for a given config digest. So the winner is always the real signer
// set: the f byzantine oracles cannot reach the threshold on their own, and
// there is no honest set for them to outvote.
//
// Candidates are still considered in hash order, so that a tie, which needs
// honest oracles to disagree and therefore should not happen, resolves the same
// way on every oracle instead of following map iteration.
func electPredecessorConfig(byHash map[[32]byte]predecessorConfig, votesByHash map[[32]byte]int, f int) *predecessorConfig {
	hashes := make([][32]byte, 0, len(byHash))
	for h := range byHash {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
	for _, h := range hashes {
		if votesByHash[h] > f {
			pc := byHash[h]
			return &pc
		}
	}
	return nil
}

func makeChannelHash(cd protocol.ChannelDefinitionWithID) [32]byte {
	return protocol.ChannelHashV2(cd)
}

func sortChannelIDs(cids []llotypes.ChannelID) {
	sort.Slice(cids, func(i, j int) bool { return cids[i] < cids[j] })
}

// cloneChannelDefinitions deep-copies the definitions so that this round's
// mutations - the votes applied to the pending set - cannot reach the immutable
// generation the definitions came from, which other rounds are reading
// concurrently.
func cloneChannelDefinitions(in llotypes.ChannelDefinitions) llotypes.ChannelDefinitions {
	return protocol.CloneChannelDefinitions(in)
}
