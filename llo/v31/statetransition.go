package llo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// StateTransition mutates the replicated KeyValueState based on the round's
// observations and returns a self-sufficient precursor for Reports.
//
// This is a faithful port of the core of the v30 Outcome computation, adapted
// to read/write per-channel keys in the KeyValueState instead of decoding and
// re-encoding a monolithic previous outcome.
//
// TODO(v31-parity): history-backfill channel selection, calculated streams,
// seconds-resolution overlap prevention, DisableNilStreamValues influence on
// validAfter advancement, and cross-round timestamped-aggregate carry-forward
// are not yet ported. See doc.go.
func (p *Plugin) StateTransition(ctx context.Context, seqNr uint64, _ ocrtypes.AttributedQuery, aos []ocrtypes.AttributedObservation, kvRW ocr3_1types.KeyValueStateReadWriter, bf ocr3_1types.BlobFetcher) (ocr3_1types.ReportsPlusPrecursor, error) {
	if len(aos) < 2*p.F+1 {
		return nil, fmt.Errorf("invariant violation: expected at least 2f+1 attributed observations, got %d (f: %d)", len(aos), p.F)
	}

	// Initial round: establish the lifecycle stage and nothing else.
	if seqNr <= 1 {
		stage := llocommon.LifeCycleStageProduction
		if p.PredecessorConfigDigest != nil {
			stage = llocommon.LifeCycleStageStaging
		}
		if err := writeLifecycle(kvRW, stage); err != nil {
			return nil, err
		}
		if err := writeChannelIndex(kvRW, nil); err != nil {
			return nil, err
		}
		return encodePrecursor(precursor{LifeCycleStage: stage})
	}

	prev, err := loadKVState(kvRW)
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
		StreamAggregates:                llocommon.StreamAggregates{},
	}

	// Lifecycle stage & promotion.
	promotedValidAfter := map[llotypes.ChannelID]uint64(nil)
	if prev.lifeCycleStage == llocommon.LifeCycleStageStaging && validPredecessorRetirementReport != nil {
		p.Logger.Infow("Promoting protocol instance from staging to production 🎖️", "seqNr", seqNr, "validAfterNanoseconds", validPredecessorRetirementReport.ValidAfterNanoseconds)
		out.LifeCycleStage = llocommon.LifeCycleStageProduction
		promotedValidAfter = validPredecessorRetirementReport.ValidAfterNanoseconds
	} else {
		out.LifeCycleStage = prev.lifeCycleStage
	}
	if out.LifeCycleStage == llocommon.LifeCycleStageProduction && shouldRetireVotes > p.F {
		p.Logger.Infow("Retiring production protocol instance ⚰️", "seqNr", seqNr)
		out.LifeCycleStage = llocommon.LifeCycleStageRetired
	}

	// Channel definition changes (skipped once retired).
	var removedChannelIDs []llotypes.ChannelID
	if out.LifeCycleStage != llocommon.LifeCycleStageRetired {
		removedChannelIDs = applyChannelVotes(out.ChannelDefinitions, removeChannelVotesByID, updateDefsByHash, updateVotesByHash, p.F, p.OptsCache)
	}

	// validAfter.
	if promotedValidAfter != nil {
		for id, va := range promotedValidAfter {
			out.ValidAfterNanoseconds[id] = va
		}
	}
	for channelID, prevValidAfter := range prev.validAfterNanoseconds {
		if _, done := out.ValidAfterNanoseconds[channelID]; done {
			continue
		}
		if prevReportable(prev, channelID, p.DefaultMinReportIntervalNanoseconds) {
			// Previous round reported; advance to the previous observation timestamp.
			out.ValidAfterNanoseconds[channelID] = prev.observationTimestampNs
		} else {
			out.ValidAfterNanoseconds[channelID] = prevValidAfter
		}
	}
	for channelID := range out.ChannelDefinitions {
		if _, ok := out.ValidAfterNanoseconds[channelID]; !ok {
			// New channel; becomes reportable in later rounds.
			out.ValidAfterNanoseconds[channelID] = out.ObservationTimestampNanoseconds
		}
	}
	for _, channelID := range removedChannelIDs {
		delete(out.ValidAfterNanoseconds, channelID)
	}

	// Aggregation (regular + timestamped, computed fresh from this round's observations).
	if err := p.aggregate(out.ChannelDefinitions, streamObservations, out.StreamAggregates); err != nil {
		return nil, err
	}

	// Flush KV mutations.
	if err := p.flushKV(kvRW, prev, out, removedChannelIDs); err != nil {
		return nil, err
	}

	if p.Config.VerboseLogging {
		p.Logger.Debugw("Generated precursor", "lifeCycleStage", out.LifeCycleStage, "channels", len(out.ChannelDefinitions), "seqNr", seqNr)
	}
	return encodePrecursor(out)
}

func (p *Plugin) decodeObservations(ctx context.Context, aos []ocrtypes.AttributedObservation, seqNr uint64, bf ocr3_1types.BlobFetcher) (
	timestampsNanoseconds []uint64,
	validPredecessorRetirementReport *llocommon.RetirementReport,
	shouldRetireVotes int,
	removeChannelVotesByID map[llotypes.ChannelID]int,
	updateChannelDefinitionsByHash map[[32]byte]llocommon.ChannelDefinitionWithID,
	updateChannelVotesByHash map[[32]byte]int,
	streamObservations map[llotypes.StreamID][]llocommon.StreamValue,
	err error,
) {
	removeChannelVotesByID = make(map[llotypes.ChannelID]int)
	updateChannelDefinitionsByHash = make(map[[32]byte]llocommon.ChannelDefinitionWithID)
	updateChannelVotesByHash = make(map[[32]byte]int)
	streamObservations = make(map[llotypes.StreamID][]llocommon.StreamValue)

	for _, ao := range aos {
		observation, derr := decodeObservation(ctx, ao.Observation, bf)
		if derr != nil {
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
			defWithID := llocommon.ChannelDefinitionWithID{ChannelDefinition: channelDefinition, ChannelID: channelID}
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
	updateDefsByHash map[[32]byte]llocommon.ChannelDefinitionWithID,
	updateVotesByHash map[[32]byte]int,
	f int,
	optsCache *llocommon.OptsCache,
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
		def  llocommon.ChannelDefinitionWithID
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
		if !exists && len(defs) >= llocommon.MaxOutcomeChannelDefinitionsLength {
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
func (p *Plugin) aggregate(defs llotypes.ChannelDefinitions, streamObservations map[llotypes.StreamID][]llocommon.StreamValue, out llocommon.StreamAggregates) error {
	for _, cd := range defs {
		if cd.Tombstone || cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			continue
		}
		for _, strm := range cd.Streams {
			sid, agg := strm.StreamID, strm.Aggregator
			if agg == llotypes.AggregatorCalculated {
				continue // TODO(v31-parity): calculated streams
			}
			if _, exists := out[sid][agg]; exists {
				continue
			}
			m, exists := out[sid]
			if !exists {
				m = make(map[llotypes.Aggregator]llocommon.StreamValue)
				out[sid] = m
			}
			aggF := llocommon.GetAggregatorFunc(agg)
			if aggF == nil {
				return fmt.Errorf("no aggregator function defined for aggregator of type %v", agg)
			}
			result, aerr := aggF(streamObservations[sid], p.F)
			if aerr != nil {
				// Ignore streams that cannot be aggregated; they are simply
				// absent from the precursor (matches v30 for the non-carry-forward case).
				continue
			}
			m[agg] = result
		}
	}
	return nil
}

// flushKV persists the computed state, writing only what changed and cleaning
// up removed channels.
func (p *Plugin) flushKV(kvRW ocr3_1types.KeyValueStateReadWriter, prev *kvState, out precursor, removedChannelIDs []llotypes.ChannelID) error {
	if out.LifeCycleStage != prev.lifeCycleStage {
		if err := writeLifecycle(kvRW, out.LifeCycleStage); err != nil {
			return err
		}
	}
	if err := writeObsTS(kvRW, out.ObservationTimestampNanoseconds); err != nil {
		return err
	}

	// Channel definitions: write current set, delete removed.
	prevIDs := make(map[llotypes.ChannelID]struct{}, len(prev.channelDefinitions))
	for id := range prev.channelDefinitions {
		prevIDs[id] = struct{}{}
	}
	for id, cd := range out.ChannelDefinitions {
		prevCd, existed := prev.channelDefinitions[id]
		if existed && prevCd.Equals(cd) {
			continue // unchanged; avoid an unnecessary modified-key
		}
		if err := writeChannelDefinition(kvRW, id, cd); err != nil {
			return err
		}
	}
	for _, id := range removedChannelIDs {
		if err := deleteChannel(kvRW, id); err != nil {
			return err
		}
	}

	// validAfter: write current entries; delete removed.
	for id, va := range out.ValidAfterNanoseconds {
		if prevVA, ok := prev.validAfterNanoseconds[id]; ok && prevVA == va {
			continue
		}
		if err := writeValidAfter(kvRW, id, va); err != nil {
			return err
		}
	}

	// Index: rewrite if the channel set changed.
	if !sameChannelSet(prev.channelDefinitions, out.ChannelDefinitions) {
		ids := make([]llotypes.ChannelID, 0, len(out.ChannelDefinitions))
		for id := range out.ChannelDefinitions {
			ids = append(ids, id)
		}
		if err := writeChannelIndex(kvRW, ids); err != nil {
			return err
		}
	}
	return nil
}

// prevReportable reports whether the channel was reportable in the previous
// round, using the min-report-interval rule. Deferred: backfill, seconds
// resolution, DisableNilStreamValues.
func prevReportable(prev *kvState, channelID llotypes.ChannelID, minReportInterval uint64) bool {
	if prev.lifeCycleStage == llocommon.LifeCycleStageRetired {
		return false
	}
	cd, exists := prev.channelDefinitions[channelID]
	if !exists || cd.Tombstone {
		return false
	}
	if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
		return false // TODO(v31-parity): backfill reportability
	}
	validAfter, ok := prev.validAfterNanoseconds[channelID]
	if !ok {
		return false
	}
	return prev.observationTimestampNs >= validAfter+minReportInterval && prev.observationTimestampNs > validAfter
}

func medianTimestamp(timestampsNanoseconds []uint64) uint64 {
	sort.Slice(timestampsNanoseconds, func(i, j int) bool { return timestampsNanoseconds[i] < timestampsNanoseconds[j] })
	return timestampsNanoseconds[len(timestampsNanoseconds)/2]
}

func makeChannelHash(cd llocommon.ChannelDefinitionWithID) [32]byte {
	pb := &llocommon.LLOChannelIDAndDefinitionProto{
		ChannelID:         cd.ChannelID,
		ChannelDefinition: makeChannelDefinitionProto(cd.ChannelDefinition),
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

func sameChannelSet(a, b llotypes.ChannelDefinitions) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}
