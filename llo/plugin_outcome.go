package llo

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

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
		return nil, fmt.Errorf("error decoding previous outcome: %w", err)
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
	// outcome.ObservationTimestampNanoseconds
	/////////////////////////////////
	outcome.ObservationTimestampNanoseconds = medianTimestamp(timestampsNanoseconds)

	/////////////////////////////////
	// outcome.LifeCycleStage
	/////////////////////////////////
	if previousOutcome.LifeCycleStage == LifeCycleStageStaging && validPredecessorRetirementReport != nil {
		// Promote this protocol instance to the production stage! 🚀
		p.Logger.Infow("Promoting protocol instance from staging to production 🎖️", "seqNr", outctx.SeqNr, "stage", "Outcome", "validAfterNanoseconds", validPredecessorRetirementReport.ValidAfterNanoseconds)

		// override ValidAfterNanoseconds with the value from the retirement report
		// so that we have no gaps in the validity time range.
		outcome.ValidAfterNanoseconds = validPredecessorRetirementReport.ValidAfterNanoseconds
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

	removedChannelIDs := make([]llotypes.ChannelID, 0, len(removeChannelVotesByID))
	for channelID, voteCount := range removeChannelVotesByID {
		if voteCount <= p.F {
			continue
		}
		removedChannelIDs = append(removedChannelIDs, channelID)
		delete(outcome.ChannelDefinitions, channelID)
		p.ChannelDefinitionOptsCache.Delete(channelID)
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

		// Invalidate cache if the channel is added or updated
		p.ChannelDefinitionOptsCache.Delete(defWithID.ChannelID)
	}

	// at this point the outcome.ChannelDefinitions is fully populated both from carry forward of previousOutcome 
	// and from the new channels added/updated via voting.
	// only once we've assembled the outcome.ChannelDefinitions we should cache the opts for all channels.
	for channelID, cd := range outcome.ChannelDefinitions {
		if _, cached := p.ChannelDefinitionOptsCache.Get(channelID); !cached {
			p.parseAndCacheChannelOpts(channelID, cd)
		}
	}

	/////////////////////////////////
	// outcome.ValidAfterNanoseconds
	/////////////////////////////////

	// ValidAfterNanoseconds can be non-nil here if earlier code already
	// populated ValidAfterNanoseconds during promotion to production. In this
	// case, nothing to do.
	if outcome.ValidAfterNanoseconds == nil {
		outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{}
		for channelID, previousValidAfterNanoseconds := range previousOutcome.ValidAfterNanoseconds {
			if err3 := previousOutcome.IsReportable(channelID, p.ProtocolVersion, p.DefaultMinReportIntervalNanoseconds, p.ReportCodecs, p.ChannelDefinitionOptsCache); err3 != nil {
				if p.Config.VerboseLogging {
					p.Logger.Debugw("Channel is not reportable", "channelID", channelID, "err", err3, "stage", "Outcome", "seqNr", outctx.SeqNr)
				}
				// previous outcome did not report; keep the same ValidAfterNanoseconds
				outcome.ValidAfterNanoseconds[channelID] = previousValidAfterNanoseconds
			} else {
				// previous outcome reported; update ValidAfterNanoseconds to the previousObservationTimestamp
				outcome.ValidAfterNanoseconds[channelID] = previousOutcome.ObservationTimestampNanoseconds
			}
		}
	}

	for channelID := range outcome.ChannelDefinitions {
		if _, ok := outcome.ValidAfterNanoseconds[channelID]; !ok {
			// new channel, set ValidAfterNanoseconds to observations timestamp
			outcome.ValidAfterNanoseconds[channelID] = outcome.ObservationTimestampNanoseconds
		}
	}

	// One might think that we should simply delete any channel from
	// ValidAfterNanoseconds that is not mentioned in the ChannelDefinitions. This
	// could, however, lead to gaps being created if this protocol instance is
	// promoted from staging to production while we're still "ramping up" the
	// full set of channels. We do the "safe" thing (i.e. minimizing occurrence
	// of gaps) here and only remove channels if there has been an explicit vote
	// to remove them.
	for _, channelID := range removedChannelIDs {
		delete(outcome.ValidAfterNanoseconds, channelID)
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
		if cd.Tombstone {
			continue
		}

		for _, strm := range cd.Streams {
			sid, agg := strm.StreamID, strm.Aggregator

			// Calculated streams are handled after all streams are aggregated
			if strm.Aggregator == llotypes.AggregatorCalculated {
				continue
			}

			if _, exists := outcome.StreamAggregates[sid][agg]; exists {
				// Should only happen in the case of duplicate
				// streams, no need to aggregate twice.
				//
				// This isn't an error, its possible for report formats to
				// specify the same stream multiple times if they wish.
				continue
			}

			// Create the aggregator => stream ID map if it doesn't already exist
			m, exists := outcome.StreamAggregates[sid]
			if !exists {
				m = make(map[llotypes.Aggregator]StreamValue)
				outcome.StreamAggregates[sid] = m
			}

			// Copy over previous results if its a TimestampedStreamValue
			// This may be replaced later if we get an observation with a newer timestamp
			if prev, exists := previousOutcome.StreamAggregates[sid]; exists {
				if prevValue, exists := prev[agg]; exists {
					if timestampedValue, is := prevValue.(*TimestampedStreamValue); is {
						m[agg] = timestampedValue
					}
				}
			}

			// Perform the aggregation
			aggF := GetAggregatorFunc(agg)
			if aggF == nil {
				return nil, fmt.Errorf("no aggregator function defined for aggregator of type %v", agg)
			}
			result, err := aggF(streamObservations[sid], p.F)

			// Handle aggregation results
			switch v := result.(type) {
			case *TimestampedStreamValue:
				// In case of failed aggregation, keep the copied value from
				// last time.
				if err != nil {
					if p.Config.VerboseLogging {
						p.Logger.Debugw("Aggregation failed for TimestampedStreamValue, carrying forwards previous value", "aggregator", agg, "channelID", cid, "f", p.F, "streamID", sid, "observations", streamObservations[sid], "stage", "Outcome", "seqNr", outctx.SeqNr, "err", err, "previousValue", m[agg])
					}
					continue
				}
				// If timestamp is later than the one in previous outcome,
				// update, otherwise keep the one from the previous outcome
				// copied over.
				//
				// In other words, we never overwrite a newer value with an
				// older one, guaranteeing monotonicity.
				prevValue, exists := m[agg]
				if !exists {
					// It may not exist in case we never had a successful
					// aggregation on this timestamped stream before, e.g.
					// for a brand new stream.
					// In which case, always write the value.
					m[agg] = v
					continue
				}
				prevTSV, is := prevValue.(*TimestampedStreamValue)
				if !is {
					// If the copied previous value is nil or not a
					// TimestampedStreamValue, always write the new value.
					m[agg] = v
					continue
				}
				if v.ObservedAtNanoseconds <= prevTSV.ObservedAtNanoseconds {
					if p.Config.VerboseLogging {
						p.Logger.Debugw("Aggregation result is older than previous value, keeping previous value", "aggregator", agg, "channelID", cid, "f", p.F, "streamID", sid, "observations", streamObservations[sid], "stage", "Outcome", "seqNr", outctx.SeqNr, "previousValue", prevTSV, "newValue", v)
					}
					continue
				}
				// Overwrite if newer
				m[agg] = v
			default:
				if err != nil {
					if p.Config.VerboseLogging {
						p.Logger.Warnw("Aggregation failed", "aggregator", agg, "channelID", cid, "f", p.F, "streamID", sid, "observations", streamObservations[sid], "stage", "Outcome", "seqNr", outctx.SeqNr, "err", err)
					}
					// Ignore stream that cannot be aggregated; this stream
					// ID/value will be missing from the outcome
					continue
				}

				// Update the value in the outcome
				m[agg] = result
			}
		}
	}

	// ProcessStreamCalculated checks if channels have calculated streams and if so
	// it will evaluate the expression and update the outcome.StreamAggregates with the result
	p.ProcessCalculatedStreams(&outcome)

	if p.Config.VerboseLogging {
		p.Logger.Debugw("Generated outcome", "outcome", outcome, "stage", "Outcome", "seqNr", outctx.SeqNr)
	}
	p.captureOutcomeTelemetry(outcome, outctx)
	return p.OutcomeCodec.Encode(outcome)
}

func (p *Plugin) decodeObservations(aos []types.AttributedObservation, outctx ocr3types.OutcomeContext) (timestampsNanoseconds []uint64, validPredecessorRetirementReport *RetirementReport, shouldRetireVotes int, removeChannelVotesByID map[llotypes.ChannelID]int, updateChannelDefinitionsByHash map[ChannelHash]ChannelDefinitionWithID, updateChannelVotesByHash map[ChannelHash]int, streamObservations map[llotypes.StreamID][]StreamValue) {
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
	// ObservationTimestampNanoseconds is the median timestamp from the
	// latest set of observations
	ObservationTimestampNanoseconds uint64
	// ChannelDefinitions defines the set & structure of channels for which we
	// generate reports
	ChannelDefinitions llotypes.ChannelDefinitions
	// Latest ValidAfterNanoseconds value for each channel, reports for each channel
	// span from ValidAfterNanoseconds to ObservationTimestampNanoseconds
	ValidAfterNanoseconds map[llotypes.ChannelID]uint64
	// StreamAggregates contains stream IDs mapped to various aggregations.
	// Usually you will only have one aggregation type per stream but since
	// channels can define different aggregation methods, sometimes we will
	// need multiple.
	StreamAggregates StreamAggregates
}

func (out *Outcome) GenRetirementReport(protocolVersion uint32) RetirementReport {
	return RetirementReport{
		ProtocolVersion:       protocolVersion,
		ValidAfterNanoseconds: out.ValidAfterNanoseconds,
	}
}

// Channel becomes reportable when:
// ObservationTimestampNanoseconds > ValidAfterNanoseconds(previous observation timestamp)+MinReportInterval

// Indicates whether a report can be generated for the given channel.
// Returns nil if channel is reportable
// NOTE: A channel is still reportable even if missing some or all stream
// values. The report codec is expected to handle nils and act accordingly
// (e.g. some values may be optional).
func (out *Outcome) IsReportable(channelID llotypes.ChannelID, protocolVersion uint32, minReportInterval uint64, codecsMap map[llotypes.ReportFormat]ReportCodec, optsCache ChannelDefinitionOptsCache) *UnreportableChannelError {
	if out.LifeCycleStage == LifeCycleStageRetired {
		return &UnreportableChannelError{nil, "IsReportable=false; retired channel", channelID}
	}

	cd, exists := out.ChannelDefinitions[channelID]
	if !exists {
		return &UnreportableChannelError{nil, "IsReportable=false; no channel definition with this ID", channelID}
	}

	if cd.Tombstone {
		// Tombstone channels are not reportable
		return &UnreportableChannelError{nil, "IsReportable=false; tombstone channel", channelID}
	}

	codec, hasCodec := codecsMap[cd.ReportFormat]
	if !hasCodec {
		return &UnreportableChannelError{nil, fmt.Sprintf("IsReportable=false; no codec found for report format %d", cd.ReportFormat), channelID}
	}

	validAfterNanos, ok := out.ValidAfterNanoseconds[channelID]
	if !ok {
		// No ValidAfterNanoseconds entry yet, this must be a new channel.
		// ValidAfterNanoseconds will be populated in Outcome() so the channel
		// becomes reportable in later protocol rounds.
		return &UnreportableChannelError{nil, "IsReportable=false; no ValidAfterNanoseconds entry yet, this must be a new channel", channelID}
	}
	obsTsNanos := out.ObservationTimestampNanoseconds

	// Enforce minReportInterval
	if protocolVersion > 0 {
		// observation timestamp must be at least validAfterNanoseconds-1 +
		// minReportInterval in order to report (i.e. reports are separated by a
		// minimum of minReportInterval nanoseconds)
		if obsTsNanos < validAfterNanos+minReportInterval {
			nsUntilReportable := (validAfterNanos + minReportInterval) - obsTsNanos
			return &UnreportableChannelError{nil, fmt.Sprintf("IsReportable=false; not valid yet (ObservationTimestampNanoseconds=%d, validAfterNanoseconds=%d, minReportInterval=%d); %f seconds (%dns) until reportable", obsTsNanos, validAfterNanos, minReportInterval, float64(nsUntilReportable)/1e9, nsUntilReportable), channelID}
		}
	}

	// Prevent overlaps or reports not valid yet based on time resolution
	//
	// Truncate timestamps to second resolution for version 0
	// This keeps compatibility with old nodes that may not have nanosecond resolution
	//
	// Also use seconds resolution for report formats that require it to prevent overlap
	isSecondsResolution, err := IsSecondsResolution(channelID, codec, optsCache)
	if err != nil {
		return &UnreportableChannelError{err, "IsReportable=false; failed to determine time resolution", channelID}
	}
	if protocolVersion == 0 || isSecondsResolution {
		validAfterSeconds := validAfterNanos / 1e9
		obsTsSeconds := obsTsNanos / 1e9
		if validAfterSeconds >= obsTsSeconds {
			return &UnreportableChannelError{nil, fmt.Sprintf("ChannelID: 1; Reason: IsReportable=false; not valid yet (observationsTimestampSeconds=%d, validAfterSeconds=%d)", obsTsSeconds, validAfterSeconds), channelID}
		}
	}

	return nil
}

func IsSecondsResolution(channelID llotypes.ChannelID, codec ReportCodec, optsCache ChannelDefinitionOptsCache) (bool, error) {
	// Try to determine the time resolution from the channel definition opts
	if optsCache != nil {
		if cachedOpts, cached := optsCache.Get(channelID); cached {
			if timeResProvider, ok := codec.(TimeResolutionProvider); ok {
				resolution, err := timeResProvider.TimeResolution(cachedOpts)
				if err != nil {
					// This should not happen since caching the cached opts should logically ensure that this channel's opts
					// are valid for channels report codec. If this error occurs it would indicate a wrong codec/opts mismatch.
					return false, fmt.Errorf("invariant violation: failed to parse time resolution from opts (wrong codec/opts mismatch?) :%w", err)
				}
				return resolution == ResolutionSeconds, nil
			}
		}
	}

	// Fall back to protocol default time resolution when codecs don't implement TimeResolutionProvider
	return false, nil
}

// List of reportable channels (according to IsReportable), sorted according
// to a canonical ordering
func (out *Outcome) ReportableChannels(protocolVersion uint32, defaultMinReportInterval uint64, codecsMap map[llotypes.ReportFormat]ReportCodec, optsCache ChannelDefinitionOptsCache) (reportable []llotypes.ChannelID, unreportable []*UnreportableChannelError) {
	for channelID := range out.ChannelDefinitions {
		// In theory in future, minReportInterval could be overridden on a
		// per-channel basis in the ChannelDefinitions
		if err := out.IsReportable(channelID, protocolVersion, defaultMinReportInterval, codecsMap, optsCache); err != nil {
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

type UnreportableChannelError struct {
	Inner     error `json:",omitempty"`
	Reason    string
	ChannelID llotypes.ChannelID
}

func (e *UnreportableChannelError) Error() string {
	s := fmt.Sprintf("ChannelID: %d; Reason: %s", e.ChannelID, e.Reason)
	if e.Inner != nil {
		s += fmt.Sprintf("; Err: %v", e.Inner)
	}
	return s
}

func (e *UnreportableChannelError) String() string {
	return e.Error()
}

func (e *UnreportableChannelError) Unwrap() error {
	return e.Inner
}

// MakeChannelHash is used for mapping ChannelDefinitionWithIDs
func MakeChannelHash(cd ChannelDefinitionWithID) ChannelHash {
	h := sha256.New()
	merr := errors.Join(
		binary.Write(h, binary.BigEndian, cd.ChannelID),
		binary.Write(h, binary.BigEndian, cd.ReportFormat),
		binary.Write(h, binary.BigEndian, uint32(len(cd.Streams))), //nolint:gosec // number of streams is limited by MaxStreamsPerChannel
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

func medianTimestamp(timestampsNanoseconds []uint64) uint64 {
	sort.Slice(timestampsNanoseconds, func(i, j int) bool { return timestampsNanoseconds[i] < timestampsNanoseconds[j] })
	return timestampsNanoseconds[len(timestampsNanoseconds)/2]
}

func (p *Plugin) captureOutcomeTelemetry(outcome Outcome, outctx ocr3types.OutcomeContext) {
	if p.OutcomeTelemetryCh != nil {
		ot, err := makeOutcomeTelemetry(outcome, p.ConfigDigest, outctx.SeqNr, p.DonID)
		if err != nil {
			p.Logger.Warnw("Error making outcome telemetry", "err", err)
		} else {
			select {
			case p.OutcomeTelemetryCh <- ot:
			default:
				p.Logger.Warn("OutcomeTelemetryCh is full, dropping telemetry")
			}
		}
	}
}

func makeOutcomeTelemetry(outcome Outcome, configDigest types.ConfigDigest, seqNr uint64, donID uint32) (*LLOOutcomeTelemetry, error) {
	ot := &LLOOutcomeTelemetry{
		LifeCycleStage:                  string(outcome.LifeCycleStage),
		ObservationTimestampNanoseconds: outcome.ObservationTimestampNanoseconds,
		ChannelDefinitions:              make(map[uint32]*LLOChannelDefinitionProto, len(outcome.ChannelDefinitions)),
		ValidAfterNanoseconds:           make(map[uint32]uint64, len(outcome.ValidAfterNanoseconds)),
		StreamAggregates:                make(map[uint32]*LLOAggregatorStreamValue, len(outcome.StreamAggregates)),
		SeqNr:                           seqNr,
		ConfigDigest:                    configDigest[:],
		DonId:                           donID,
	}
	for id, cd := range outcome.ChannelDefinitions {
		ot.ChannelDefinitions[id] = makeChannelDefinitionProto(cd)
	}
	for id, va := range outcome.ValidAfterNanoseconds {
		ot.ValidAfterNanoseconds[id] = va
	}
	for sid, aggMap := range outcome.StreamAggregates {
		if len(aggMap) == 0 {
			continue
		}
		aggVals := make(map[uint32]*LLOStreamValue, len(aggMap))
		for agg, sv := range aggMap {
			v, err := makeLLOStreamValue(sv)
			if err != nil {
				return nil, fmt.Errorf("failed to make outcome telemetry; %w", err)
			}
			aggVals[uint32(agg)] = v
		}
		ot.StreamAggregates[sid] = &LLOAggregatorStreamValue{AggregatorValues: aggVals}
	}
	return ot, nil
}

// parseAndCacheChannelOpts parses and caches channel opts to avoid repeated JSON parsing
func (p *Plugin) parseAndCacheChannelOpts(channelID llotypes.ChannelID, cd llotypes.ChannelDefinition) {
	codec, exists := p.ReportCodecs[cd.ReportFormat]
	if !exists {
		return
	}

	if err := p.ChannelDefinitionOptsCache.Set(channelID, cd.Opts, codec); err != nil {
		p.Logger.Warnw("Failed to parse opts for caching", "channelID", channelID, "reportFormat", cd.ReportFormat, "err", err)
	}
}
