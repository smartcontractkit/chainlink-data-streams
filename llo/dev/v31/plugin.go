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

	// ChannelAnalysisCache memoizes the per-definition verification checks, whose
	// cost is dominated by decoding channel opts. Verification runs three times
	// per round over largely identical sets: the committed definitions and the
	// desired ones in Observation, and the set each update-carrying observation
	// advocates in ValidateObservation. One cache is shared by all of them, so
	// the parallel ValidateObservation calls hit what Observation already
	// decoded. May be nil, in which case nothing is memoized.
	ChannelAnalysisCache *protocol.ChannelAnalysisCache

	// BlobPayloads memoizes blob payloads decoded within one round, so a handle
	// referenced by an observation is fetched and decompressed once for
	// ValidateObservation and StateTransition together. May be nil, in which
	// case nothing is memoized.
	BlobPayloads *blobPayloadCache

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
//
// MaxDurationObservation is not enforced by OCR3.1 (it only logs a warning), so
// ctx carries no observation deadline; it is honoured for cancellation, which is
// what bounds the wait for an in-flight blob pump cycle.
func (p *Plugin) Observation(ctx context.Context, seqNr uint64, _ ocrtypes.AttributedQuery, kvReader ocr3_1types.KeyValueStateReader, _ ocr3_1types.BlobBroadcastFetcher) (ocrtypes.Observation, error) {
	if seqNr < 1 {
		return nil, fmt.Errorf("got invalid seqnr=%d, must be >=1", seqNr)
	} else if seqNr == 1 {
		// First round: state is empty and the result is never used (see StateTransition).
		return nil, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("observation canceled: %w", err)
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
		// The committed set is the authority on which channels exist, so this is
		// where entries for channels that are gone are dropped.
		p.ChannelAnalysisCache.Prune(state.channelDefinitions)
		if badChannels, verifyErr := protocol.UnverifiableChannelIDsWithCache(p.ReportCodecs, state.channelDefinitions, p.ChannelAnalysisCache); len(badChannels) > 0 || verifyErr != nil {
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
			if len(obs.AttestedPredecessorRetirement) != 0 {
				p.voteOnPredecessorConfig(&obs, seqNr)
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
		if snap, reason = p.pump.Take(ctx, seqNr); snap != nil {
			handles = append(handles, snap.handleBytes)
		} else {
			p.Logger.Debugw("No usable stream-value snapshot for this round", "stage", "Observation", "seqNr", seqNr, "reason", reason, "misses", p.pump.Misses(), "cycles", p.pump.Cycles())
		}
	}

	obsTSNanos := time.Now().UnixNano()
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
func supportedReportFormats(codecs map[llotypes.ReportFormat]protocol.ReportCodec) map[llotypes.ReportFormat]struct{} {
	sorted := make([]llotypes.ReportFormat, 0, len(codecs))
	for format := range codecs {
		sorted = append(sorted, format)
	}
	// Truncation must not depend on map iteration order: every node with the
	// same codecs has to advertise the same set.
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if len(sorted) > protocol.MaxObservationSupportedReportFormatsLength {
		sorted = sorted[:protocol.MaxObservationSupportedReportFormatsLength]
	}
	out := make(map[llotypes.ReportFormat]struct{}, len(sorted))
	for _, format := range sorted {
		out[format] = struct{}{}
	}
	return out
}

// observableStreams lists the streams a round should observe: every stream of
// every live channel, minus calculated streams (which are derived in StateTransition)
// and history backfill channels (values come from their opts, not observed).
func observableStreams(state *kvState) []llotypes.StreamID {
	if len(state.channelDefinitions) == 0 {
		return nil
	}
	seen := make(map[llotypes.StreamID]struct{})
	streams := make([]llotypes.StreamID, 0, len(state.channelDefinitions))
	for _, cd := range state.channelDefinitions {
		if cd.Tombstone || cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
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

// voteOnPredecessorConfig populates obs.PredecessorSigners / obs.PredecessorF
// with the predecessor's signer set from the node-local retirement report
// cache, so the DON can agree on it for the round.
//
// Verifying an attested predecessor retirement report needs that signer set.
// Reading it from the local cache inside the state transition would fork the
// state, because the config poller fills the cache asynchronously and a lagging
// node reaches a different verdict from one that is caught up. Voting on it
// here moves the node-local read into the observation, where oracles are
// allowed to differ, and leaves the state transition reading only the round's
// replicated observations.
//
// Only called when this observation carries an attested retirement report, so
// the vote rides only the rounds where a promotion is possible.
func (p *Plugin) voteOnPredecessorConfig(obs *Observation, seqNr uint64) {
	signers, f, exists := p.PredecessorRetirementReportCache.PredecessorConfig(*p.PredecessorConfigDigest)
	if !exists {
		p.Logger.Warnw("Predecessor config not in the local cache yet, not voting on it this round", "stage", "Observation", "seqNr", seqNr, "predecessorConfigDigest", *p.PredecessorConfigDigest)
		return
	}
	if len(signers) == 0 || len(signers) > protocol.MaxObservationPredecessorSignersLength {
		p.Logger.Errorw("Local predecessor config has an unusable signer set, not voting on it", "stage", "Observation", "seqNr", seqNr, "signers", len(signers))
		return
	}
	obs.PredecessorSigners = signers
	obs.PredecessorF = f
}

// voteOnChannels populates obs.RemoveChannelIDs / obs.UpdateChannelDefinitions
// by comparing the desired channel definitions against current KV state.
//
// ChannelDefinitionCache.Definitions(committed) is a reconciliation, not a
// snapshot of a file: the shipped onchain cache merges what it has fetched into
// the committed set it is handed and returns the result, so the desired set is
// normally the committed one plus additions and changes. It deletes nothing
// implicitly.
//
//   - before anything has been fetched, and after a fetch error, a poll that
//     saw nothing, or a stale event, it returns the committed set unchanged. A
//     source it cannot read produces no opinion rather than a removal;
//   - the merge is upsert-only. A channel missing from a newly fetched file is
//     preserved, not dropped. Removal is explicit: the owner marks the channel
//     with Tombstone, which is a definition change like any other;
//   - the single deletion path is reaping an already tombstoned channel, once
//     the owner omits it from a later file. So a channel leaves the committed
//     set only after the DON has already agreed it is a tombstone.
//
// This function must not assume that, because the contract does not require it.
// Definitions may be implemented by anything, and the static cache shipped for
// benchmarks and the dummy relayer ignores the committed set entirely and
// returns its configured JSON verbatim. Everything below is therefore written
// against the weaker guarantee: the desired set is one node's opinion, votes
// decide, and an absent channel means "not mentioned", which is only treated as
// "remove" when the set as a whole is credible. See the empty-set case below.
func (p *Plugin) voteOnChannels(obs *Observation, state *kvState) {
	obs.RemoveChannelIDs = map[llotypes.ChannelID]struct{}{}

	expectedChannelDefs := p.ChannelDefinitionCache.Definitions(state.channelDefinitions)
	// Only the channels this node would vote to add or change are held to the
	// admission-only checks; the ones already committed are not, or a
	// grandfathered channel would freeze channel voting entirely.
	admitting := protocol.ChangedChannelIDs(state.channelDefinitions, expectedChannelDefs)
	if err := protocol.VerifyChannelDefinitionsForAdmissionWithCache(p.ReportCodecs, expectedChannelDefs, admitting, p.ChannelAnalysisCache); err != nil {
		// Don't halt on an invalid channel-definitions file; just don't vote.
		p.Logger.Errorw("ChannelDefinitionCache.Definitions is invalid", "err", err)
		return
	}

	// An empty desired set against a non-empty committed one is not read as
	// "remove every channel". Under the reconciliation above the onchain cache
	// cannot produce that for live channels, but nothing in the interface says
	// so, and an implementation that returns nothing before it has loaded
	// anything, or on a source it could not read, would make every live channel
	// a removal candidate. Every node would do it from the same input, so the
	// channels would really go.
	//
	// Tombstoned channels stay removable. An empty desired set is exactly what
	// the onchain cache produces on the last reap: the owner omits the
	// tombstones it wants dropped, and when every committed channel is a
	// tombstone the merge comes back empty. Abstaining there would stop the
	// removal the DON has already agreed to and strand those channels in the
	// definitions permanently.
	removable := state.channelDefinitions
	if len(expectedChannelDefs) == 0 && len(state.channelDefinitions) > 0 {
		removable = llotypes.ChannelDefinitions{}
		for channelID, cd := range state.channelDefinitions {
			if cd.Tombstone {
				removable[channelID] = cd
			}
		}
		p.Logger.Warnw("ChannelDefinitionCache.Definitions is empty while channels are committed; voting to remove only the tombstoned ones", "committed", len(state.channelDefinitions), "removable", len(removable))
	}

	removeChannelDefinitions := protocol.SubtractChannelDefinitions(removable, expectedChannelDefs, protocol.MaxObservationRemoveChannelIDsLength)
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

	observation, err := decodeObservation(ctx, ao.Observation, bf, p.BlobPayloads.round(seqNr))
	if err != nil {
		return fmt.Errorf("observation decode error: %w", err)
	}

	if p.PredecessorConfigDigest == nil && len(observation.AttestedPredecessorRetirement) != 0 {
		return errors.New("AttestedPredecessorRetirement is not empty even though this instance has no predecessor")
	}
	if p.PredecessorConfigDigest == nil && len(observation.PredecessorSigners) != 0 {
		return errors.New("PredecessorSigners is not empty even though this instance has no predecessor")
	}
	// The signer set is only ever needed to verify a report carried alongside
	// it, so a set without one is dead weight on the wire.
	if len(observation.PredecessorSigners) != 0 && len(observation.AttestedPredecessorRetirement) == 0 {
		return errors.New("PredecessorSigners is not empty even though this observation carries no AttestedPredecessorRetirement")
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
	if err := protocol.VerifyChannelDefinitionsWithCache(p.ReportCodecs, defsForVerify, p.ChannelAnalysisCache); err != nil {
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
	if p.pump != nil && !p.pump.Close() {
		return fmt.Errorf("blob pump did not stop within %s", p.pump.closeTimeout)
	}
	return nil
}
