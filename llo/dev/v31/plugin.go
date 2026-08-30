package llo

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

	// loggedHistorySkip remembers which channels have already had their
	// sampling-rate warning logged, so it is said once per channel per instance
	// rather than every cycle. Node-local and for logging only: it must never
	// influence anything StateTransition computes, or nodes would diverge.
	loggedHistorySkipMu sync.Mutex
	loggedHistorySkip   map[llotypes.ChannelID]struct{}

	// From offchain config
	ProtocolVersion                          uint32
	DefaultMinReportIntervalNanoseconds      uint64
	DefaultMinObservationIntervalNanoseconds uint64
}

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

	obsTSNanos := time.Now().UnixNano()
	if obsTSNanos < 0 {
		return nil, fmt.Errorf("negative observation timestamps are not supported, got: %d", obsTSNanos)
	}

	var obs Observation
	var streams []llotypes.StreamID

	if state.lifeCycleStage == protocol.LifeCycleStageRetired {
		p.Logger.Debugw("Node is retired, will generate empty observation", "stage", "Observation", "seqNr", seqNr)
	} else {
		if err = protocol.VerifyChannelDefinitions(p.ReportCodecs, state.channelDefinitions); err != nil {
			return nil, fmt.Errorf("state.channelDefinitions is invalid: %w", err)
		}

		if p.PredecessorConfigDigest != nil && state.lifeCycleStage == protocol.LifeCycleStageStaging {
			obs.AttestedPredecessorRetirement, err = p.PredecessorRetirementReportCache.AttestedRetirementReport(*p.PredecessorConfigDigest)
			if err != nil {
				return nil, fmt.Errorf("error fetching attested retirement report from cache: %w", err)
			}
		}

		obs.ShouldRetire, err = p.ShouldRetireCache.ShouldRetire(p.ConfigDigest)
		if err != nil {
			return nil, fmt.Errorf("error fetching shouldRetire from cache: %w", err)
		}

		p.voteOnChannels(&obs, state, seqNr)

		if p.DefaultMinObservationIntervalNanoseconds > 0 {
			if err := readHotStateForObservation(kvReader, state); err != nil {
				return nil, fmt.Errorf("failed to load hot state for observation skip: %w", err)
			}
			// The hot state lags one round: a channel that reported in the round
			// which wrote it has not had its schedule advanced yet, because that
			// happens in the next StateTransition. Apply the same advancement
			// here so Observation and StateTransition agree on what is due.
			for cid, reported := range state.reportedLastRound {
				if reported {
					state.observationDueNanoseconds[cid] = nextObservationDue(
						state.observationDueNanoseconds, cid,
						p.DefaultMinObservationIntervalNanoseconds, state.observationTimestampNs)
				}
			}
		}
		streams = observableStreams(state, p.DefaultMinObservationIntervalNanoseconds, uint64(obsTSNanos))
	}

	// Stream values are gathered asynchronously by the blob pump and are always
	// carried by a blob, never inline. Publish this round's context, then pick up
	// whatever the pump has ready; a round that finds nothing usable emits an
	// observation carrying only votes and its timestamp. Missing values cost the
	// affected streams that round's aggregate (which needs >F values), not the
	// round itself.
	var handles [][]byte
	// A nil pump means stream values were never wired up (or the plugin was built
	// without the factory).
	//
	// The input is published every round, including rounds that observe nothing,
	// so a later cycle can never gather a stale stream set (a cycle with no
	// streams parks nothing and is a no-op). Take is called only when this round
	// wants values: it consumes and clears the parked snapshot, so calling it on
	// a round with nothing to observe would discard a snapshot a later round
	// could still use.
	if p.pump != nil {
		p.pump.SetInput(pumpInput{streams: streams, seqNr: seqNr, lifeCycleStage: state.lifeCycleStage})

		if len(streams) > 0 {
			if snap, reason := p.pump.Take(seqNr); snap != nil {
				handles = append(handles, snap.handleBytes)
			} else {
				p.Logger.Debugw("No usable stream-value snapshot for this round", "stage", "Observation", "seqNr", seqNr, "reason", reason, "misses", p.pump.Misses(), "cycles", p.pump.Cycles())
			}
		}
	}

	obs.UnixTimestampNanoseconds = uint64(obsTSNanos)

	return encodeObservation(obs, handles)
}

// isObservationDue reports whether the channel is due for observation and
// aggregation this round. When minObservationInterval is 0 the feature is
// disabled and every channel is due.
//
// A channel with no schedule entry is due. That covers a channel that has never
// reported, including a newly effective one, which therefore aggregates from its
// first round and builds its initial aggregates and history exactly as it did
// before this interval existed. Entries appear once a channel has reported.
func isObservationDue(observationDue map[llotypes.ChannelID]uint64, channelID llotypes.ChannelID, minObservationInterval, now uint64) bool {
	if minObservationInterval == 0 {
		return true
	}
	dueAt, scheduled := observationDue[channelID]
	if !scheduled {
		return true
	}
	return now >= dueAt
}

// nextObservationDue returns the channel's next due timestamp, given that it
// reported at reportedAt.
//
// The schedule is fixed-rate: it advances from the channel's own previous due
// timestamp rather than from the round that reported. That distinction is the
// whole point. A channel's stream values are gathered asynchronously and arrive
// a round after its streams enter the pump's input, so the first due round after
// a skip window withholds and the report lands a round late. Advancing from the
// report would fold that delay into every later cycle and the cadence would
// creep; advancing from the schedule makes it a one-time phase offset instead.
//
// The offset is also what supplies the lead the pump needs: because the schedule
// runs ahead of the watermark by it, a channel becomes due for observation that
// far before it is allowed to report, so its values are gathered by the time it
// reports. The lead is therefore however much the data source actually needs,
// and is not configured anywhere.
//
// This is deliberately not validAfter. That watermark is a report boundary,
// emitted in the report and defining the window (validAfter, observationTimestamp]
// that consecutive reports must tile exactly; anchoring it to a schedule would
// leave gaps between reports.
func nextObservationDue(observationDue map[llotypes.ChannelID]uint64, channelID llotypes.ChannelID, minObservationInterval, reportedAt uint64) uint64 {
	prevDue, scheduled := observationDue[channelID]
	if !scheduled {
		// First report: start the schedule from it.
		return reportedAt + minObservationInterval
	}
	if next := prevDue + minObservationInterval; next > reportedAt {
		return next
	}
	// More than one interval behind, so the channel was unable to report for a
	// while. Skip the missed slots rather than firing every round to catch up,
	// while staying on the original phase.
	missed := (reportedAt - prevDue) / minObservationInterval
	return prevDue + (missed+1)*minObservationInterval
}

// exemptFromObservationSkip reports whether a channel must be observed and
// aggregated every round no matter what its schedule says.
//
// Only history_backfill is: its watermark is a history timestamp rather than a
// report time, so a report cadence means nothing for it.
//
// Channels reading History(...) are deliberately NOT exempt. The skip makes a
// channel's report cadence the sampling rate for its windows, which lowers their
// resolution but does not make them wrong - records carry their own observation
// timestamp, and TWAP integrates over real time. Nor can it stall silently: an
// unreadable window leaves the channel unreportable, which also stops its
// schedule advancing, so it reverts to observing every round until the window is
// satisfied. See DefaultMinObservationIntervalNanoseconds for how to size a
// window against the interval.
func exemptFromObservationSkip(cd llotypes.ChannelDefinition) bool {
	return cd.ReportFormat == llotypes.ReportFormatHistoryBackfill
}

// warnHistorySampledAtReportCadence says once per channel that a channel reading
// stream history is being skipped, so its windows are now sampled at its report
// cadence rather than at the round rate. Whether that is fine depends on the
// depths and thresholds the channel was configured with, which this cannot know,
// so it reports the fact and the interval and leaves the arithmetic to whoever
// reads it. See DefaultMinObservationIntervalNanoseconds.
func (p *Plugin) warnHistorySampledAtReportCadence(channelID llotypes.ChannelID, seqNr uint64) {
	p.loggedHistorySkipMu.Lock()
	if p.loggedHistorySkip == nil {
		p.loggedHistorySkip = map[llotypes.ChannelID]struct{}{}
	}
	_, said := p.loggedHistorySkip[channelID]
	if !said {
		p.loggedHistorySkip[channelID] = struct{}{}
	}
	p.loggedHistorySkipMu.Unlock()
	if said {
		return
	}
	p.Logger.Infow("Channel reads stream history and is now sampled at its report cadence, not the round rate; check its history depths and any TWAP thresholds against the observation interval",
		"channelID", channelID,
		"minObservationIntervalNanoseconds", p.DefaultMinObservationIntervalNanoseconds,
		"stage", "StateTransition", "seqNr", seqNr)
}

// observableDefinitions returns the subset of defs whose channels are due for
// observation/aggregation. Tombstoned and history_backfill channels are always
// retained, as are the channels exemptFromObservationSkip names. That keeps the
// set handed to aggregate and ProcessCalculatedStreams the same whether or not
// the interval is configured. When minObservationInterval is 0, defs is returned
// unchanged.
func observableDefinitions(defs llotypes.ChannelDefinitions, observationDue map[llotypes.ChannelID]uint64, minObservationInterval, now uint64) llotypes.ChannelDefinitions {
	if minObservationInterval == 0 {
		return defs
	}
	filtered := make(llotypes.ChannelDefinitions, len(defs))
	for channelID, cd := range defs {
		if cd.Tombstone || exemptFromObservationSkip(cd) ||
			isObservationDue(observationDue, channelID, minObservationInterval, now) {
			filtered[channelID] = cd
		}
	}
	return filtered
}

// observableStreams lists the streams a round should observe: every stream of
// every live channel, minus calculated streams (which are derived in
// StateTransition rather than observed).
//
// When minObservationInterval is non-zero, channels that are not yet due on the
// observation schedule are skipped: their streams are not observed unless shared
// with a channel that is due or exempt (see exemptFromObservationSkip). A
// channel with no schedule entry yet is always considered due.
func observableStreams(state *kvState, minObservationInterval uint64, now uint64) []llotypes.StreamID {
	if len(state.channelDefinitions) == 0 {
		return nil
	}
	seen := make(map[llotypes.StreamID]struct{})
	streams := make([]llotypes.StreamID, 0, len(state.channelDefinitions))
	for channelID, cd := range state.channelDefinitions {
		if cd.Tombstone {
			continue
		}
		if !exemptFromObservationSkip(cd) &&
			!isObservationDue(state.observationDueNanoseconds, channelID, minObservationInterval, now) {
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

// voteOnChannels populates obs.RemoveChannelIDs / obs.UpdateChannelDefinitions
// by comparing the desired channel definitions against current KV state.
func (p *Plugin) voteOnChannels(obs *Observation, state *kvState, seqNr uint64) {
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

	// Only the baseline checks run here. A definition is installed on more than
	// f votes for its exact hash, so at least one honest oracle must have voted
	// for it, and an honest oracle only votes for what its own admission-time
	// verification accepted (see voteOnChannels). Repeating the admission-only
	// checks here would add nothing, and would make oracles running different
	// versions of those checks disagree on whether an observation is valid.
	defsForVerify := observation.UpdateChannelDefinitions
	if len(observation.UpdateChannelDefinitions) > 0 {
		state, serr := loadColdKVState(kvReader, p.ChannelCache)
		if serr != nil {
			return fmt.Errorf("failed to load KV state for channel definition validation: %w", serr)
		}
		merged := make(llotypes.ChannelDefinitions, len(state.channelDefinitions)+len(observation.UpdateChannelDefinitions))
		for id, def := range state.channelDefinitions {
			merged[id] = def
		}
		for id, def := range observation.UpdateChannelDefinitions {
			merged[id] = def
		}
		defsForVerify = merged
	}
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
