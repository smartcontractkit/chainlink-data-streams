package llo

import (
	"context"
	"errors"
	"fmt"
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

		streams = observableStreams(state)
	}

	// Stream values are gathered asynchronously by the blob pump and are always
	// carried by a blob, never inline. Publish this round's context, then pick up
	// whatever the pump has ready; a round that finds nothing usable emits an
	// observation carrying only votes and its timestamp. Missing values cost the
	// affected streams that round's aggregate (which needs >F values), not the
	// round itself.
	var handles [][]byte
	// A nil pump means stream values were never wired up (or the plugin was built
	// without the factory); rounds that observe no streams have nothing for the
	// pump to gather, so neither publishes input nor consumes a snapshot.
	if p.pump != nil && len(streams) > 0 {
		p.pump.SetInput(pumpInput{streams: streams, seqNr: seqNr, lifeCycleStage: state.lifeCycleStage})

		if snap, reason := p.pump.Take(seqNr); snap != nil {
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

	return encodeObservation(obs, handles)
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

// voteOnChannels populates obs.RemoveChannelIDs / obs.UpdateChannelDefinitions
// by comparing the desired channel definitions against current KV state.
func (p *Plugin) voteOnChannels(obs *Observation, state *kvState, seqNr uint64) {
	obs.RemoveChannelIDs = map[llotypes.ChannelID]struct{}{}

	expectedChannelDefs := p.ChannelDefinitionCache.Definitions(state.channelDefinitions)
	if err := protocol.VerifyChannelDefinitions(p.ReportCodecs, expectedChannelDefs); err != nil {
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
