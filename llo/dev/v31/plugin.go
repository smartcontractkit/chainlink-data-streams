package llo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"golang.org/x/exp/maps"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	"github.com/smartcontractkit/libocr/quorumhelper"
)

// Config holds v31 plugin behavior toggles.
type Config struct {
	// VerboseLogging enables additional, potentially expensive logging.
	VerboseLogging bool
}

var _ ocr3_1types.ReportingPlugin[llotypes.ReportInfo] = &Plugin{}

// Plugin is the OCR3.1 LLO reporting plugin.
type Plugin struct {
	Config                           Config
	PredecessorConfigDigest          *ocrtypes.ConfigDigest
	ConfigDigest                     ocrtypes.ConfigDigest
	PredecessorRetirementReportCache protocol.PredecessorRetirementReportCache
	ShouldRetireCache                ShouldRetireCache
	ChannelDefinitionCache           llotypes.ChannelDefinitionCache
	DataSource                       DataSource
	Logger                           logger.Logger
	N                                int
	F                                int
	RetirementReportCodec            protocol.RetirementReportCodec
	ReportCodecs                     map[llotypes.ReportFormat]protocol.ReportCodec
	DonID                            uint32
	// ChannelCache memoizes the channel definitions record, together with the
	// opts decoded from it, across rounds so it is re-read only when its
	// sequence number changes. May be nil, in which case a fresh generation is
	// built every round. Never read opts from anywhere else: each round must use
	// the generation it loaded (kvState.opts), or a concurrently running round
	// could swap decoded opts out from under it.
	ChannelCache *protocol.ChannelCache

	// Optional telemetry sinks; best-effort, non-blocking.
	OutcomeTelemetryCh chan<- *protocol.LLOOutcomeTelemetry
	ReportTelemetryCh  chan<- *protocol.LLOReportTelemetry

	// pump gathers stream observations and broadcasts them as blobs off the OCR
	// critical path. Observation only picks up the handle it parked.
	pump *blobPump

	// From offchain config
	ProtocolVersion                     uint32
	DefaultMinReportIntervalNanoseconds uint64
	// AggregationFaultTolerance is how many Byzantine contributors per-stream
	// aggregation tolerates. It sets the contribution floor, see
	// minContributions.
	AggregationFaultTolerance int
}

// minContributions is the contribution floor: the fewest contributions a stream
// aggregate may be built from. 2*AggregationFaultTolerance+1 keeps the result
// inside the honest value range when up to AggregationFaultTolerance
// contributors are Byzantine.
//
// Distinct from the consensus quorum, which counts attributed observations, not
// the per-stream contributions inside them.
func (p *Plugin) minContributions() int { return 2*p.AggregationFaultTolerance + 1 }

// Query is empty: LLO oracles do not coordinate on what to observe.
func (p *Plugin) Query(ctx context.Context, seqNr uint64, _ ocr3_1types.KeyValueStateReader, _ ocr3_1types.BlobBroadcastFetcher) (ocrtypes.Query, error) {
	return nil, nil
}

// Observation reads current state from the KeyValueState, votes on channel
// changes, and returns a serialized observation referencing the blob of stream
// values most recently gathered by the blob pump.
// The per-round BlobBroadcastFetcher is unused: broadcasting happens in the blob
// pump, which holds the identical fetcher handed to the factory.
func (p *Plugin) Observation(_ context.Context, seqNr uint64, _ ocrtypes.AttributedQuery, kvReader ocr3_1types.KeyValueStateReader, _ ocr3_1types.BlobBroadcastFetcher) (ocrtypes.Observation, error) {
	if seqNr < 1 {
		return nil, fmt.Errorf("got invalid seqnr=%d, must be >=1", seqNr)
	} else if seqNr == 1 {
		// First round: state is empty and the result is never used (see StateTransition).
		return nil, nil
	}

	state, err := loadColdKVState(kvReader, p.ChannelCache)
	if err != nil {
		return nil, fmt.Errorf("failed to load KV state: %w", err)
	}

	var obs Observation
	var streams []llotypes.StreamID

	if state.lifeCycleStage == protocol.LifeCycleStageRetired {
		p.Logger.Debugw("Node is retired, will generate empty observation", "stage", "Observation", "seqNr", seqNr)
	} else {
		// Committed state is replicated, so failing verification here on every
		// node would halt the DON with no way out, as the nodes would never vote
		// to remove offending channels.
		// For a DON where all participants share the same version this is unreachable,
		// as ValidateObservation runs the same baseline checks over the merged set,
		// but a version skew can make it reachable.
		// Report the finding and carry on, which is the same treatment the
		// admission-only findings get in voteOnChannels.
		if badChannels, verifyErr := protocol.UnverifiableChannelIDs(p.ReportCodecs, state.channelDefinitions); len(badChannels) > 0 || verifyErr != nil {
			p.Logger.Errorw("Committed channel definitions fail baseline verification on this build", "stage", "Observation", "seqNr", seqNr, "channelIDs", sortedChannelIDSet(badChannels), "err", verifyErr)
		}

		if p.PredecessorConfigDigest != nil && state.lifeCycleStage == protocol.LifeCycleStageStaging {
			obs.AttestedPredecessorRetirement, err = p.PredecessorRetirementReportCache.AttestedRetirementReport(*p.PredecessorConfigDigest)
			if err != nil {
				// Best-effort: the state transition only needs one node to
				// supply a valid retirement report, so omit it rather than
				// failing the round.
				obs.AttestedPredecessorRetirement = nil
				p.Logger.Errorw("Failed to fetch attested retirement report from cache, omitting it from this observation", "stage", "Observation", "seqNr", seqNr, "err", err)
			}
		}

		obs.ShouldRetire, err = p.ShouldRetireCache.ShouldRetire(p.ConfigDigest)
		if err != nil {
			// Best-effort: retirement is decided by a quorum of votes, so
			// abstain rather than failing the round.
			obs.ShouldRetire = false
			p.Logger.Errorw("Failed to fetch shouldRetire from cache, not voting to retire this round", "stage", "Observation", "seqNr", seqNr, "err", err)
		}

		p.voteOnChannels(&obs, state)

		streams = observableStreams(state)
	}

	// Stream values are gathered asynchronously by the blob pump and are always
	// carried by a blob, never inline. Publish this round's context, then pick up
	// whatever the pump has ready; a round that finds nothing usable emits an
	// observation carrying only votes and its timestamp. Missing values cost the
	// affected streams that round's aggregate (which needs >F values), not the
	// round itself.
	var handles [][]byte
	var snap *blobSnapshot
	// A nil pump means stream values were never wired up (or the plugin was built
	// without the factory); rounds that observe no streams have nothing for the
	// pump to gather, so neither publishes input nor consumes a snapshot.
	if p.pump != nil && len(streams) > 0 {
		p.pump.SetInput(pumpInput{streams: streams, seqNr: seqNr, lifeCycleStage: state.lifeCycleStage})

		var reason string
		if snap, reason = p.pump.Take(seqNr); snap != nil {
			handles = append(handles, snap.handleBytes)
		} else {
			p.Logger.Debugw("No usable stream-value snapshot for this round", "stage", "Observation", "seqNr", seqNr, "reason", reason, "misses", p.pump.Misses(), "cycles", p.pump.Cycles())
		}
	}

	// Timestamp the data, not the round. The pump gathers stream values off the
	// critical path, so they were read before this round started; stamping
	// time.Now() would have the report claim the values are newer than they are
	// for every aggregate that does not carry its own timestamp. A round with no
	// snapshot carries only votes, for which the round time is the right stamp.
	obsTime := time.Now()
	if snap != nil {
		obsTime = snap.observedAt
	}
	obsTSNanos := obsTime.UnixNano()
	if obsTSNanos < 0 {
		return nil, fmt.Errorf("negative observation timestamps are not supported, got: %d", obsTSNanos)
	}
	obs.UnixTimestampNanoseconds = uint64(obsTSNanos)

	// Advertised every round, including when retired: a statement about this
	// binary, not about the round or about what this node wants admitted.
	// voteOnChannels above is deliberately unaware of p.ReportCodecs: whether a
	// channel can be encoded DON-wide is decided from these advertisements in
	// the state transition, not locally per voter.
	obs.SupportedReportFormats = supportedReportFormats(p.ReportCodecs)

	return encodeObservation(obs, handles)
}

// supportedReportFormats lists the report formats this node can encode, taken
// from the codecs it was constructed with. Truncated to the advertisable bound
// so the observation stays within its size budget; a real codec map is far
// smaller than the bound, so this never fires in practice.
func supportedReportFormats(codecs map[llotypes.ReportFormat]protocol.ReportCodec) []llotypes.ReportFormat {
	out := make([]llotypes.ReportFormat, 0, len(codecs))
	for format := range codecs {
		out = append(out, format)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > protocol.MaxObservationSupportedReportFormatsLength {
		out = out[:protocol.MaxObservationSupportedReportFormatsLength]
	}
	return out
}

// observableStreams lists the streams a round should observe: every stream of
// every live channel, minus calculated streams (which are derived in
// StateTransition rather than observed).
func observableStreams(state *kvState) []llotypes.StreamID {
	if len(state.channelDefinitions) == 0 {
		return nil
	}
	seen := make(map[llotypes.StreamID]struct{})
	streams := make([]llotypes.StreamID, 0, len(state.channelDefinitions))
	for _, cd := range state.channelDefinitions {
		if cd.Tombstone {
			continue
		}
		for _, strm := range cd.Streams {
			if strm.Aggregator == llotypes.AggregatorCalculated {
				continue
			}
			if _, dup := seen[strm.StreamID]; dup {
				continue
			}
			seen[strm.StreamID] = struct{}{}
			streams = append(streams, strm.StreamID)
		}
	}
	return streams
}

// sortedChannelIDSet renders a channel ID set in ascending order, for logs.
func sortedChannelIDSet(set map[llotypes.ChannelID]struct{}) []llotypes.ChannelID {
	ids := make([]llotypes.ChannelID, 0, len(set))
	for channelID := range set {
		ids = append(ids, channelID)
	}
	sortChannelIDs(ids)
	return ids
}

// voteOnChannels populates obs.RemoveChannelIDs / obs.UpdateChannelDefinitions
// by comparing the desired channel definitions against current KV state.
func (p *Plugin) voteOnChannels(obs *Observation, state *kvState) {
	obs.RemoveChannelIDs = map[llotypes.ChannelID]struct{}{}

	expectedChannelDefs := p.ChannelDefinitionCache.Definitions(state.channelDefinitions)
	// Only the channels this node would vote to add or change are held to the
	// admission-only checks; the ones already committed are not, or a
	// grandfathered channel would freeze channel voting entirely.
	admitting := protocol.ChangedChannelIDs(state.channelDefinitions, expectedChannelDefs)
	if err := protocol.VerifyChannelDefinitionsForAdmission(p.ReportCodecs, expectedChannelDefs, admitting); err != nil {
		// Don't halt on an invalid channel-definitions file; just don't vote.
		p.Logger.Errorw("ChannelDefinitionCache.Definitions is invalid", "err", err)
		return
	}

	removeChannelDefinitions := protocol.SubtractChannelDefinitions(state.channelDefinitions, expectedChannelDefs, protocol.MaxObservationRemoveChannelIDsLength)
	for channelID := range removeChannelDefinitions {
		obs.RemoveChannelIDs[channelID] = struct{}{}
	}

	obs.UpdateChannelDefinitions = make(llotypes.ChannelDefinitions)
	expectedChannelIDs := maps.Keys(expectedChannelDefs)
	sortChannelIDs(expectedChannelIDs)
	for _, channelID := range expectedChannelIDs {
		prev, exists := state.channelDefinitions[channelID]
		channelDefinition := expectedChannelDefs[channelID]
		if exists && prev.Equals(channelDefinition) {
			continue
		}
		obs.UpdateChannelDefinitions[channelID] = channelDefinition
		if len(obs.UpdateChannelDefinitions) >= protocol.MaxObservationUpdateChannelDefinitionsLength {
			break
		}
	}
}

// ValidateObservation checks an observation is well-formed. Blob-referenced
// stream values are fetched so lengths can be validated.
func (p *Plugin) ValidateObservation(ctx context.Context, seqNr uint64, _ ocrtypes.AttributedQuery, ao ocrtypes.AttributedObservation, kvReader ocr3_1types.KeyValueStateReader, bf ocr3_1types.BlobFetcher) error {
	if seqNr < 1 {
		return fmt.Errorf("invalid SeqNr: %d", seqNr)
	} else if seqNr == 1 {
		if len(ao.Observation) != 0 {
			return fmt.Errorf("expected empty observation for first round, got: 0x%x", ao.Observation)
		}
		return nil
	}

	observation, err := decodeObservation(ctx, ao.Observation, bf)
	if err != nil {
		return fmt.Errorf("observation decode error: %w", err)
	}

	if p.PredecessorConfigDigest == nil && len(observation.AttestedPredecessorRetirement) != 0 {
		return errors.New("AttestedPredecessorRetirement is not empty even though this instance has no predecessor")
	}
	if len(observation.UpdateChannelDefinitions) > protocol.MaxObservationUpdateChannelDefinitionsLength {
		return fmt.Errorf("UpdateChannelDefinitions is too long: %v vs %v", len(observation.UpdateChannelDefinitions), protocol.MaxObservationUpdateChannelDefinitionsLength)
	}
	if len(observation.RemoveChannelIDs) > protocol.MaxObservationRemoveChannelIDsLength {
		return fmt.Errorf("RemoveChannelIDs is too long: %v vs %v", len(observation.RemoveChannelIDs), protocol.MaxObservationRemoveChannelIDsLength)
	}
	if len(observation.SupportedReportFormats) > protocol.MaxObservationSupportedReportFormatsLength {
		return fmt.Errorf("SupportedReportFormats is too long: %v vs %v", len(observation.SupportedReportFormats), protocol.MaxObservationSupportedReportFormatsLength)
	}

	// Only the baseline checks run here. A definition is installed on more than
	// f votes for its exact hash, so at least one honest oracle must have voted
	// for it, and an honest oracle only votes for what its own admission-time
	// verification accepted (see voteOnChannels). Repeating the admission-only
	// checks here would add nothing, and would make oracles running different
	// versions of those checks disagree on whether an observation is valid.
	// The set verified is the one this observation advocates: committed state
	// with its updates applied and its removals taken out.
	//
	// Whole-set budgets cannot be enforced per observation anyway, votes from
	// different oracles combine, and what gets committed is decided by the
	// per-hash threshold, not by any single observation.
	defsForVerify := observation.UpdateChannelDefinitions
	if len(observation.UpdateChannelDefinitions) > 0 {
		state, serr := loadColdKVState(kvReader, p.ChannelCache)
		if serr != nil {
			return fmt.Errorf("failed to load KV state for channel definition validation: %w", serr)
		}
		merged := make(llotypes.ChannelDefinitions, len(state.channelDefinitions)+len(observation.UpdateChannelDefinitions))
		for id, def := range state.channelDefinitions {
			if _, removed := observation.RemoveChannelIDs[id]; removed {
				continue
			}
			merged[id] = def
		}
		for id, def := range observation.UpdateChannelDefinitions {
			merged[id] = def
		}
		defsForVerify = merged
	}
	// Unlike the committed-state check in Observation, this one stays fatal:
	// rejecting a peer observation is not a halt, and these checks are the only
	// thing standing between a proposer and a malformed committed definition.
	// Under version skew a stricter build rejects observations that carry
	// updates from a staler one, which costs update-voting liveness only. An
	// observation that votes no update has nothing to verify here, so rounds
	// themselves are unaffected.
	if err := protocol.VerifyChannelDefinitions(p.ReportCodecs, defsForVerify); err != nil {
		return fmt.Errorf("UpdateChannelDefinitions is invalid: %w", err)
	}

	if len(observation.StreamValues) > protocol.MaxObservationStreamValuesLength {
		return fmt.Errorf("StreamValues is too long: %v vs %v", len(observation.StreamValues), protocol.MaxObservationStreamValuesLength)
	}
	for _, streamValue := range observation.StreamValues {
		if v, ok := streamValue.(*protocol.TimestampedStreamValue); ok {
			if v.StreamValue.Type() != protocol.LLOStreamValue_Decimal {
				return fmt.Errorf("nested stream value on TimestampedStreamValue must be a Decimal, got: %v", v.StreamValue.Type())
			}
		}
	}

	return nil
}

// ObservationQuorum uses the standard 2f+1 quorum.
func (p *Plugin) ObservationQuorum(ctx context.Context, seqNr uint64, _ ocrtypes.AttributedQuery, aos []ocrtypes.AttributedObservation, _ ocr3_1types.KeyValueStateReader, _ ocr3_1types.BlobFetcher) (bool, error) {
	return quorumhelper.ObservationCountReachesObservationQuorum(quorumhelper.QuorumTwoFPlusOne, p.N, p.F, aos), nil
}

// Committed is a no-op: LLO has no on-commit side effects, and Committed is not
// guaranteed to be called for every seqNr. Outcome telemetry is emitted from
// StateTransition, so there is nothing to do here.
func (p *Plugin) Committed(ctx context.Context, seqNr uint64, _ ocr3_1types.KeyValueStateReader) error {
	return nil
}

func (p *Plugin) ShouldAcceptAttestedReport(context.Context, uint64, ocr3types.ReportWithInfo[llotypes.ReportInfo]) (bool, error) {
	return true, nil
}

func (p *Plugin) ShouldTransmitAcceptedReport(context.Context, uint64, ocr3types.ReportWithInfo[llotypes.ReportInfo]) (bool, error) {
	return true, nil
}

func (p *Plugin) Close() error {
	if p.pump != nil {
		p.pump.Close()
	}
	return nil
}
