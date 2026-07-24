package llo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/exp/maps"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"

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
	PredecessorRetirementReportCache llocommon.PredecessorRetirementReportCache
	ShouldRetireCache                ShouldRetireCache
	ChannelDefinitionCache           llotypes.ChannelDefinitionCache
	DataSource                       DataSource
	Logger                           logger.Logger
	N                                int
	F                                int
	RetirementReportCodec            llocommon.RetirementReportCodec
	ReportCodecs                     map[llotypes.ReportFormat]llocommon.ReportCodec
	DonID                            uint32
	OptsCache                        *llocommon.OptsCache

	MaxDurationObservation time.Duration

	// From offchain config
	ProtocolVersion                     uint32
	DefaultMinReportIntervalNanoseconds uint64

	// BlobThreshold is the serialized stream-value payload size (bytes) above
	// which observations offload stream values to a blob. 0 disables offloading.
	BlobThreshold int
}

// Query is empty: LLO oracles do not coordinate on what to observe.
func (p *Plugin) Query(ctx context.Context, seqNr uint64, _ ocr3_1types.KeyValueStateReader, _ ocr3_1types.BlobBroadcastFetcher) (ocrtypes.Query, error) {
	return nil, nil
}

// Observation reads current state from the KeyValueState, gathers stream
// observations, votes on channel changes, and returns a (possibly blob-backed)
// serialized observation.
func (p *Plugin) Observation(ctx context.Context, seqNr uint64, _ ocrtypes.AttributedQuery, kvReader ocr3_1types.KeyValueStateReader, bbf ocr3_1types.BlobBroadcastFetcher) (ocrtypes.Observation, error) {
	if seqNr < 1 {
		return nil, fmt.Errorf("got invalid seqnr=%d, must be >=1", seqNr)
	} else if seqNr == 1 {
		// First round: state is empty and the result is never used (see StateTransition).
		return nil, nil
	}

	state, err := loadKVState(kvReader)
	if err != nil {
		return nil, fmt.Errorf("failed to load KV state: %w", err)
	}

	var obs Observation

	if state.lifeCycleStage == llocommon.LifeCycleStageRetired {
		p.Logger.Debugw("Node is retired, will generate empty observation", "stage", "Observation", "seqNr", seqNr)
	} else {
		if err = llocommon.VerifyChannelDefinitions(p.ReportCodecs, state.channelDefinitions); err != nil {
			return nil, fmt.Errorf("state.channelDefinitions is invalid: %w", err)
		}

		if p.PredecessorConfigDigest != nil && state.lifeCycleStage == llocommon.LifeCycleStageStaging {
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

		if len(state.channelDefinitions) > 0 {
			obs.StreamValues = make(llocommon.StreamValues)
			for _, cd := range state.channelDefinitions {
				if cd.Tombstone {
					continue
				}
				for _, strm := range cd.Streams {
					if strm.Aggregator == llotypes.AggregatorCalculated {
						continue
					}
					obs.StreamValues[strm.StreamID] = nil
				}
			}

			observationCtx, cancel := context.WithTimeout(ctx, p.MaxDurationObservation)
			defer cancel()
			opts := &dsOpts{p.Config.VerboseLogging, seqNr, p.ConfigDigest, time.Now()}
			if err = p.DataSource.Observe(observationCtx, obs.StreamValues, opts); err != nil {
				return nil, fmt.Errorf("DataSource.Observe error: %w", err)
			}
		}
	}

	obsTSNanos := time.Now().UnixNano()
	if obsTSNanos < 0 {
		return nil, fmt.Errorf("negative observation timestamps are not supported, got: %d", obsTSNanos)
	}
	obs.UnixTimestampNanoseconds = uint64(obsTSNanos)

	return encodeObservation(ctx, obs, seqNr, p.BlobThreshold, bbf)
}

// voteOnChannels populates obs.RemoveChannelIDs / obs.UpdateChannelDefinitions
// by comparing the desired channel definitions against current KV state.
func (p *Plugin) voteOnChannels(obs *Observation, state *kvState, seqNr uint64) {
	obs.RemoveChannelIDs = map[llotypes.ChannelID]struct{}{}

	expectedChannelDefs := p.ChannelDefinitionCache.Definitions(state.channelDefinitions)
	if err := llocommon.VerifyChannelDefinitions(p.ReportCodecs, expectedChannelDefs); err != nil {
		// Don't halt on an invalid channel-definitions file; just don't vote.
		p.Logger.Errorw("ChannelDefinitionCache.Definitions is invalid", "err", err)
		return
	}

	removeChannelDefinitions := llocommon.SubtractChannelDefinitions(state.channelDefinitions, expectedChannelDefs, llocommon.MaxObservationRemoveChannelIDsLength)
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
		if len(obs.UpdateChannelDefinitions) >= llocommon.MaxObservationUpdateChannelDefinitionsLength {
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
	if len(observation.UpdateChannelDefinitions) > llocommon.MaxObservationUpdateChannelDefinitionsLength {
		return fmt.Errorf("UpdateChannelDefinitions is too long: %v vs %v", len(observation.UpdateChannelDefinitions), llocommon.MaxObservationUpdateChannelDefinitionsLength)
	}
	if len(observation.RemoveChannelIDs) > llocommon.MaxObservationRemoveChannelIDsLength {
		return fmt.Errorf("RemoveChannelIDs is too long: %v vs %v", len(observation.RemoveChannelIDs), llocommon.MaxObservationRemoveChannelIDsLength)
	}

	defsForVerify := observation.UpdateChannelDefinitions
	if len(observation.UpdateChannelDefinitions) > 0 {
		state, serr := loadKVState(kvReader)
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
	if err := llocommon.VerifyChannelDefinitions(p.ReportCodecs, defsForVerify); err != nil {
		return fmt.Errorf("UpdateChannelDefinitions is invalid: %w", err)
	}

	if len(observation.StreamValues) > llocommon.MaxObservationStreamValuesLength {
		return fmt.Errorf("StreamValues is too long: %v vs %v", len(observation.StreamValues), llocommon.MaxObservationStreamValuesLength)
	}
	for _, streamValue := range observation.StreamValues {
		if v, ok := streamValue.(*llocommon.TimestampedStreamValue); ok {
			if v.StreamValue.Type() != llocommon.LLOStreamValue_Decimal {
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
// guaranteed to be called for every seqNr.
// TODO(v31-parity): optionally emit outcome telemetry from the committed snapshot.
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
	return nil
}
