package llo

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func (p *Plugin) outcome(outctx ocr3types.OutcomeContext, query types.Query, aos []types.AttributedObservation) (ocr3types.Outcome, error) {
	if len(aos) < 2*p.F+1 {
		return nil, fmt.Errorf("invariant violation: expected at least 2f+1 attributed observations, got %d (f: %d)", len(aos), p.F)
	}

	// Initial outcome is kind of a "cornerstone" with minimum extra information
	if outctx.SeqNr <= 1 {
		// Initial Outcome
		var lifeCycleStage llotypes.LifeCycleStage
		// NOTE: Staging instances **require** a predecessor config digest.
		// This is enforced by the contract.
		if p.PredecessorConfigDigest == nil {
			// Start straight in production if we have no predecessor
			lifeCycleStage = LifeCycleStageProduction
		} else {
			lifeCycleStage = LifeCycleStageStaging
		}
		outcome := Outcome{
			lifeCycleStage,
			0,
			nil,
			nil,
			nil,
		}
		return p.OutcomeCodec.Encode(outcome)
	}

	/////////////////////////////////
	// Decode previousOutcome
	/////////////////////////////////
	previousOutcome, err := p.OutcomeCodec.Decode(outctx.PreviousOutcome)
	if err != nil {
		return nil, fmt.Errorf("error decoding previous outcome: %v", err)
	}

	/////////////////////////////////
	// Decode observations
	/////////////////////////////////
	timestampsNanoseconds, validPredecessorRetirementReport, shouldRetireVotes, removeChannelVotesByID, updateChannelDefinitionsByHash, updateChannelVotesByHash, streamObservations := p.decodeObservations(aos, outctx)

	if len(timestampsNanoseconds) == 0 {
		return nil, errors.New("no valid observations")
	}

	var outcome Outcome

	/////////////////////////////////
	// outcome.ObservationsTimestampNanoseconds
	/////////////////////////////////
	outcome.ObservationsTimestampNanoseconds = medianTimestamp(timestampsNanoseconds)

	/////////////////////////////////
	// outcome.LifeCycleStage
	/////////////////////////////////
	if previousOutcome.LifeCycleStage == LifeCycleStageStaging && validPredecessorRetirementReport != nil {
		// Promote this protocol instance to the production stage! 🚀
		p.Logger.Infow("Promoting protocol instance from staging to production 🎖️", "seqNr", outctx.SeqNr, "stage", "Outcome", "validAfterSeconds", validPredecessorRetirementReport.ValidAfterSeconds)

		// override ValidAfterSeconds with the value from the retirement report
		// so that we have no gaps in the validity time range.
		outcome.ValidAfterSeconds = validPredecessorRetirementReport.ValidAfterSeconds
		outcome.LifeCycleStage = LifeCycleStageProduction
	} else {
		outcome.LifeCycleStage = previousOutcome.LifeCycleStage
	}

	if outcome.LifeCycleStage == LifeCycleStageProduction && shouldRetireVotes > p.F {
		p.Logger.Infow("Retiring production protocol instance ⚰️", "seqNr", outctx.SeqNr, "stage", "Outcome")
		outcome.LifeCycleStage = LifeCycleStageRetired
	}

	/////////////////////////////////
	// outcome.ChannelDefinitions
	/////////////////////////////////
	outcome.ChannelDefinitions = previousOutcome.ChannelDefinitions
	if outcome.ChannelDefinitions == nil {
		outcome.ChannelDefinitions = llotypes.ChannelDefinitions{}
	}

	// if retired, stop updating channel definitions
	if outcome.LifeCycleStage == LifeCycleStageRetired {
		removeChannelVotesByID, updateChannelDefinitionsByHash = nil, nil
	}

	var removedChannelIDs []llotypes.ChannelID
	for channelID, voteCount := range removeChannelVotesByID {
		if voteCount <= p.F {
			continue
		}
		removedChannelIDs = append(removedChannelIDs, channelID)
		delete(outcome.ChannelDefinitions, channelID)
	}

	type hashWithID struct {
		ChannelHash
		ChannelDefinitionWithID
	}
	orderedHashes := make([]hashWithID, 0, len(updateChannelDefinitionsByHash))
	for channelHash, dfnWithID := range updateChannelDefinitionsByHash {
		orderedHashes = append(orderedHashes, hashWithID{channelHash, dfnWithID})
	}
	// Use predictable order for adding channels (id asc) so that extras that
	// exceed the max are consistent across all nodes
	sort.Slice(orderedHashes, func(i, j int) bool { return orderedHashes[i].ChannelID < orderedHashes[j].ChannelID })
	for _, hwid := range orderedHashes {
		voteCount := updateChannelVotesByHash[hwid.ChannelHash]
		if voteCount <= p.F {
			continue
		}
		defWithID := hwid.ChannelDefinitionWithID
		if original, exists := outcome.ChannelDefinitions[defWithID.ChannelID]; exists {
			p.Logger.Debugw("Adding channel (replacement)",
				"channelID", defWithID.ChannelID,
				"originalChannelDefinition", original,
				"replaceChannelDefinition", defWithID,
				"seqNr", outctx.SeqNr,
				"stage", "Outcome",
			)
		} else if len(outcome.ChannelDefinitions) >= MaxOutcomeChannelDefinitionsLength {
			p.Logger.Warnw("Adding channel FAILED. Cannot add channel, outcome already contains maximum number of channels",
				"maxOutcomeChannelDefinitionsLength", MaxOutcomeChannelDefinitionsLength,
				"addChannelDefinition", defWithID,
				"seqNr", outctx.SeqNr,
				"stage", "Outcome",
			)
			// continue, don't break here because remaining channels might be a
			// replacement rather than an addition, and this is still ok
			continue
		} else {
			p.Logger.Debugw("Adding channel (new)",
				"channelID", defWithID.ChannelID,
				"addChannelDefinition", defWithID,
				"seqNr", outctx.SeqNr,
				"stage", "Outcome",
			)
		}
		outcome.ChannelDefinitions[defWithID.ChannelID] = defWithID.ChannelDefinition
	}

	/////////////////////////////////
	// outcome.ValidAfterSeconds
	/////////////////////////////////

	// ValidAfterSeconds can be non-nil here if earlier code already
	// populated ValidAfterSeconds during promotion to production. In this
	// case, nothing to do.
	if outcome.ValidAfterSeconds == nil {
		previousObservationsTimestampSeconds, err2 := previousOutcome.ObservationsTimestampSeconds()
		if err2 != nil {
			return nil, fmt.Errorf("error getting previous outcome's observations timestamp: %v", err2)
		}

		outcome.ValidAfterSeconds = map[llotypes.ChannelID]uint32{}
		for channelID, previousValidAfterSeconds := range previousOutcome.ValidAfterSeconds {
			if err3 := previousOutcome.IsReportable(channelID); err3 != nil {
				if p.Config.VerboseLogging {
					p.Logger.Debugw("Channel is not reportable", "channelID", channelID, "err", err3, "stage", "Outcome", "seqNr", outctx.SeqNr)
				}
				// previous outcome did not report; keep the same validAfterSeconds
				outcome.ValidAfterSeconds[channelID] = previousValidAfterSeconds
			} else {
				// previous outcome reported; update validAfterSeconds to the previousObservationsTimestamp
				outcome.ValidAfterSeconds[channelID] = previousObservationsTimestampSeconds
			}
		}
	}

	observationsTimestampSeconds, err := outcome.ObservationsTimestampSeconds()
	if err != nil {
		return nil, fmt.Errorf("error getting outcome's observations timestamp: %w", err)
	}

	for channelID := range outcome.ChannelDefinitions {
		if _, ok := outcome.ValidAfterSeconds[channelID]; !ok {
			// new channel, set validAfterSeconds to observations timestamp
			outcome.ValidAfterSeconds[channelID] = observationsTimestampSeconds
		}
	}

	// One might think that we should simply delete any channel from
	// ValidAfterSeconds that is not mentioned in the ChannelDefinitions. This
	// could, however, lead to gaps being created if this protocol instance is
	// promoted from staging to production while we're still "ramping up" the
	// full set of channels. We do the "safe" thing (i.e. minimizing occurrence
	// of gaps) here and only remove channels if there has been an explicit vote
	// to remove them.
	for _, channelID := range removedChannelIDs {
		delete(outcome.ValidAfterSeconds, channelID)
	}

	/////////////////////////////////
	// outcome.StreamAggregates
	/////////////////////////////////
	outcome.StreamAggregates = make(map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue, len(streamObservations))
	// Aggregation methods are defined on a per-channel basis, but we only want
	// to do the minimum necessary number of aggregations (one per stream/aggregator
	// pair) and re-use the same result, in case multiple channels share the
	// same stream/aggregator pair.
	for cid, cd := range outcome.ChannelDefinitions {
		for _, strm := range cd.Streams {
			sid, agg := strm.StreamID, strm.Aggregator
			if _, exists := outcome.StreamAggregates[sid][agg]; exists {
				p.Logger.Warnw("Invariant violation: unexpected duplicate stream/aggregator pair", "channelID", cid, "streamID", sid, "aggregator", agg, "stage", "Outcome", "seqNr", outctx.SeqNr)
				// Should only happen in the unexpected case of duplicate
				// streams, no need to aggregate twice
				continue
			}
			aggF := GetAggregatorFunc(agg)
			if aggF == nil {
				return nil, fmt.Errorf("no aggregator function defined for aggregator of type %v", agg)
			}
			m, exists := outcome.StreamAggregates[sid]
			if !exists {
				m = make(map[llotypes.Aggregator]StreamValue)
				outcome.StreamAggregates[sid] = m
			}
			result, err := aggF(streamObservations[sid], p.F)
			if err != nil {
				if p.Config.VerboseLogging {
					p.Logger.Warnw("Aggregation failed", "aggregator", agg, "channelID", cid, "f", p.F, "streamID", sid, "observations", streamObservations[sid], "stage", "Outcome", "seqNr", outctx.SeqNr, "err", err)
				}
				// Ignore stream that cannot be aggregated; this stream
				// ID/value will be missing from the outcome
				continue
			}
			m[agg] = result
		}
	}

	if p.Config.VerboseLogging {
		p.Logger.Debugw("Generated outcome", "outcome", outcome, "stage", "Outcome", "seqNr", outctx.SeqNr)
	}
	return p.OutcomeCodec.Encode(outcome)
}

func (p *Plugin) decodeObservations(aos []types.AttributedObservation, outctx ocr3types.OutcomeContext) (timestampsNanoseconds []int64, validPredecessorRetirementReport *RetirementReport, shouldRetireVotes int, removeChannelVotesByID map[llotypes.ChannelID]int, updateChannelDefinitionsByHash map[ChannelHash]ChannelDefinitionWithID, updateChannelVotesByHash map[ChannelHash]int, streamObservations map[llotypes.StreamID][]StreamValue) {
	removeChannelVotesByID = make(map[llotypes.ChannelID]int)
	updateChannelDefinitionsByHash = make(map[ChannelHash]ChannelDefinitionWithID)
	updateChannelVotesByHash = make(map[ChannelHash]int)
	streamObservations = make(map[llotypes.StreamID][]StreamValue)

	for _, ao := range aos {
		observation, err2 := p.ObservationCodec.Decode(ao.Observation)
		if err2 != nil {
			p.Logger.Warnw("ignoring invalid observation", "oracleID", ao.Observer, "error", err2)
			continue
		}

		if len(observation.AttestedPredecessorRetirement) != 0 && validPredecessorRetirementReport == nil {
			// a single valid retirement report is enough
			pcd := *p.PredecessorConfigDigest
			retirementReport, err3 := p.PredecessorRetirementReportCache.CheckAttestedRetirementReport(pcd, observation.AttestedPredecessorRetirement)
			if err3 != nil {
				p.Logger.Warnw("ignoring observation with invalid attested predecessor retirement", "oracleID", ao.Observer, "error", err3, "predecessorConfigDigest", pcd)
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

		// for each channelId count number of votes that mention it and count number of votes that include it.
		for channelID, channelDefinition := range observation.UpdateChannelDefinitions {
			defWithID := ChannelDefinitionWithID{channelDefinition, channelID}
			channelHash := MakeChannelHash(defWithID)
			updateChannelVotesByHash[channelHash]++
			updateChannelDefinitionsByHash[channelHash] = defWithID
		}

		for id, sv := range observation.StreamValues {
			// sv can never be nil here; validation is handled in the decoding
			// of the observation
			streamObservations[id] = append(streamObservations[id], sv)
		}
		if p.Config.VerboseLogging {
			p.Logger.Debugw("Got observations from peer", "stage", "Outcome", "sv", streamObservations, "oracleID", ao.Observer, "seqNr", outctx.SeqNr)
		}
	}

	return
}

type Outcome struct {
	// LifeCycleStage the protocol is in
	LifeCycleStage llotypes.LifeCycleStage
	// ObservationsTimestampNanoseconds is the median timestamp from the
	// latest set of observations
	ObservationsTimestampNanoseconds int64
	// ChannelDefinitions defines the set & structure of channels for which we
	// generate reports
	ChannelDefinitions llotypes.ChannelDefinitions
	// Latest ValidAfterSeconds value for each channel, reports for each channel
	// span from ValidAfterSeconds to ObservationTimestampSeconds
	ValidAfterSeconds map[llotypes.ChannelID]uint32
	// StreamAggregates contains stream IDs mapped to various aggregations.
	// Usually you will only have one aggregation type per stream but since
	// channels can define different aggregation methods, sometimes we will
	// need multiple.
	StreamAggregates StreamAggregates
}

// The Outcome's ObservationsTimestamp rounded down to seconds precision
func (out *Outcome) ObservationsTimestampSeconds() (uint32, error) {
	result := time.Unix(0, out.ObservationsTimestampNanoseconds).Unix()
	if int64(uint32(result)) != result {
		return 0, fmt.Errorf("timestamp doesn't fit into uint32: %v", result)
	}
	return uint32(result), nil
}

func (out *Outcome) GenRetirementReport() RetirementReport {
	return RetirementReport{
		ValidAfterSeconds: out.ValidAfterSeconds,
	}
}

// Indicates whether a report can be generated for the given channel.
// Returns nil if channel is reportable
// NOTE: A channel is still reportable even if missing some or all stream
// values. The report codec is expected to handle nils and act accordingly
// (e.g. some values may be optional).
func (out *Outcome) IsReportable(channelID llotypes.ChannelID) *ErrUnreportableChannel {
	if out.LifeCycleStage == LifeCycleStageRetired {
		return &ErrUnreportableChannel{nil, "IsReportable=false; retired channel", channelID}
	}

	observationsTimestampSeconds, err := out.ObservationsTimestampSeconds()
	if err != nil {
		return &ErrUnreportableChannel{err, "IsReportable=false; invalid observations timestamp", channelID}
	}

	_, exists := out.ChannelDefinitions[channelID]
	if !exists {
		return &ErrUnreportableChannel{nil, "IsReportable=false; no channel definition with this ID", channelID}
	}

	if _, ok := out.ValidAfterSeconds[channelID]; !ok {
		// No validAfterSeconds entry yet, this must be a new channel.
		// validAfterSeconds will be populated in Outcome() so the channel
		// becomes reportable in later protocol rounds.
		return &ErrUnreportableChannel{nil, "IsReportable=false; no validAfterSeconds entry yet, this must be a new channel", channelID}
	}

	if validAfterSeconds := out.ValidAfterSeconds[channelID]; validAfterSeconds >= observationsTimestampSeconds {
		return &ErrUnreportableChannel{nil, fmt.Sprintf("IsReportable=false; not valid yet (observationsTimestampSeconds=%d < validAfterSeconds=%d)", observationsTimestampSeconds, validAfterSeconds), channelID}
	}

	return nil
}

// List of reportable channels (according to IsReportable), sorted according
// to a canonical ordering
func (out *Outcome) ReportableChannels() (reportable []llotypes.ChannelID, unreportable []*ErrUnreportableChannel) {
	for channelID := range out.ChannelDefinitions {
		if err := out.IsReportable(channelID); err != nil {
			unreportable = append(unreportable, err)
		} else {
			reportable = append(reportable, channelID)
		}
	}

	sort.Slice(reportable, func(i, j int) bool {
		return reportable[i] < reportable[j]
	})

	return
}

type ErrUnreportableChannel struct {
	Inner     error `json:",omitempty"`
	Reason    string
	ChannelID llotypes.ChannelID
}

func (e *ErrUnreportableChannel) Error() string {
	s := fmt.Sprintf("ChannelID: %d; Reason: %s", e.ChannelID, e.Reason)
	if e.Inner != nil {
		s += fmt.Sprintf("; Err: %v", e.Inner)
	}
	return s
}

func (e *ErrUnreportableChannel) String() string {
	return e.Error()
}

func (e *ErrUnreportableChannel) Unwrap() error {
	return e.Inner
}

// MakeChannelHash is used for mapping ChannelDefinitionWithIDs
func MakeChannelHash(cd ChannelDefinitionWithID) ChannelHash {
	h := sha256.New()
	merr := errors.Join(
		binary.Write(h, binary.BigEndian, cd.ChannelID),
		binary.Write(h, binary.BigEndian, cd.ReportFormat),
		binary.Write(h, binary.BigEndian, uint32(len(cd.Streams))),
	)
	for _, strm := range cd.Streams {
		merr = errors.Join(merr, binary.Write(h, binary.BigEndian, strm.StreamID))
		merr = errors.Join(merr, binary.Write(h, binary.BigEndian, strm.Aggregator))
	}
	if merr != nil {
		// This should never happen
		panic(merr)
	}
	h.Write(cd.Opts)
	var result [32]byte
	h.Sum(result[:0])
	return result
}

func medianTimestamp(timestampsNanoseconds []int64) int64 {
	sort.Slice(timestampsNanoseconds, func(i, j int) bool { return timestampsNanoseconds[i] < timestampsNanoseconds[j] })
	return timestampsNanoseconds[len(timestampsNanoseconds)/2]
}
