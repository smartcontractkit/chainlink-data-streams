package llo

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"golang.org/x/exp/maps"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func (p *Plugin) observation(ctx context.Context, outctx ocr3types.OutcomeContext, query types.Query) (types.Observation, error) {
	// NOTE: First sequence number is always 1 (0 is invalid)
	if outctx.SeqNr < 1 {
		return types.Observation{}, fmt.Errorf("got invalid seqnr=%d, must be >=1", outctx.SeqNr)
	} else if outctx.SeqNr == 1 {
		// First round always has empty PreviousOutcome
		// Don't bother observing on the first ever round, because the result
		// will never be used anyway.
		// See case at the top of Outcome()
		return types.Observation{}, nil
	}
	// SeqNr==2 will have no channel definitions yet, so will not make any
	// observations, but it may vote to add new channel definitions

	previousOutcome, err := p.OutcomeCodec.Decode(outctx.PreviousOutcome)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling previous outcome: %w", err)
	}

	obs := Observation{
		// QUESTION: is there a way to have this captured in EAs so we get something
		// closer to the source?
		UnixTimestampNanoseconds: time.Now().UnixNano(),
	}

	if previousOutcome.LifeCycleStage == LifeCycleStageRetired {
		p.Logger.Debugw("Node is retired, will generate empty observation", "stage", "Observation", "seqNr", outctx.SeqNr)
	} else {
		if err = VerifyChannelDefinitions(previousOutcome.ChannelDefinitions); err != nil {
			// This is not expected, unless the majority of nodes are using a
			// different verification method than this one.
			//
			// If it does happen, it's an invariant violation and we cannot
			// generate an observation.
			return nil, fmt.Errorf("previousOutcome.Definitions is invalid: %w", err)
		}

		// Only try to fetch this from the cache if this instance if configured
		// with a predecessor and we're still in the staging stage.
		if p.PredecessorConfigDigest != nil && previousOutcome.LifeCycleStage == LifeCycleStageStaging {
			var err2 error
			obs.AttestedPredecessorRetirement, err2 = p.PredecessorRetirementReportCache.AttestedRetirementReport(*p.PredecessorConfigDigest)
			if err2 != nil {
				return nil, fmt.Errorf("error fetching attested retirement report from cache: %w", err2)
			}
		}

		obs.ShouldRetire, err = p.ShouldRetireCache.ShouldRetire(p.ConfigDigest)
		if err != nil {
			return nil, fmt.Errorf("error fetching shouldRetire from cache: %w", err)
		}
		if obs.ShouldRetire && p.Config.VerboseLogging {
			p.Logger.Debugw("Voting to retire", "seqNr", outctx.SeqNr, "stage", "Observation")
		}

		// vote to remove channel ids if they're in the previous outcome
		// ChannelDefinitions
		obs.RemoveChannelIDs = map[llotypes.ChannelID]struct{}{}
		// vote to add channel definitions that aren't present in the previous
		// outcome ChannelDefinitions
		{
			// NOTE: Be careful using maps, since key ordering is randomized! All
			// addition/removal lists must be built deterministically so that nodes
			// can agree on the same set of changes.
			//
			// ChannelIDs should always be sorted the same way (channel ID ascending).
			expectedChannelDefs := p.ChannelDefinitionCache.Definitions()
			if err = VerifyChannelDefinitions(expectedChannelDefs); err != nil {
				// If channel definitions is invalid, do not error out but instead
				// don't vote on any new channels.
				//
				// This prevents protocol halts in the event of an invalid channel
				// definitions file.
				p.Logger.Errorw("ChannelDefinitionCache.Definitions is invalid", "err", err)
			} else {
				removeChannelDefinitions := subtractChannelDefinitions(previousOutcome.ChannelDefinitions, expectedChannelDefs, MaxObservationRemoveChannelIDsLength)
				for channelID := range removeChannelDefinitions {
					obs.RemoveChannelIDs[channelID] = struct{}{}
				}

				// NOTE: This is slow because it deeply compares every value in the map.
				// To improve performance, consider changing channel voting to happen
				// every N rounds instead of every round. Or, alternatively perhaps the
				// first e.g. 100 rounds could check every round to allow for fast feed
				// spinup, then after that every 10 or 100 rounds.
				obs.UpdateChannelDefinitions = make(llotypes.ChannelDefinitions)
				expectedChannelIDs := maps.Keys(expectedChannelDefs)
				// Sort so we cut off deterministically
				sortChannelIDs(expectedChannelIDs)
				for _, channelID := range expectedChannelIDs {
					prev, exists := previousOutcome.ChannelDefinitions[channelID]
					channelDefinition := expectedChannelDefs[channelID]
					if exists && prev.Equals(channelDefinition) {
						continue
					}
					// Add or replace channel
					obs.UpdateChannelDefinitions[channelID] = channelDefinition
					if len(obs.UpdateChannelDefinitions) >= MaxObservationUpdateChannelDefinitionsLength {
						// Never add more than MaxObservationUpdateChannelDefinitionsLength
						break
					}
				}
			}

			if len(obs.UpdateChannelDefinitions) > 0 {
				p.Logger.Debugw("Voting to update channel definitions",
					"updateChannelDefinitions", obs.UpdateChannelDefinitions,
					"seqNr", outctx.SeqNr,
					"stage", "Observation")
			}
			if len(obs.RemoveChannelIDs) > 0 {
				p.Logger.Debugw("Voting to remove channel definitions",
					"removeChannelIDs", obs.RemoveChannelIDs,
					"seqNr", outctx.SeqNr,
					"stage", "Observation",
				)
			}
		}

		if len(previousOutcome.ChannelDefinitions) == 0 {
			p.Logger.Debugw("ChannelDefinitions is empty, will not generate any observations", "stage", "Observation", "seqNr", outctx.SeqNr)
		} else {
			obs.StreamValues = make(StreamValues)
			for _, channelDefinition := range previousOutcome.ChannelDefinitions {
				for _, strm := range channelDefinition.Streams {
					obs.StreamValues[strm.StreamID] = nil
				}
			}

			// NOTE: Timeouts/context cancelations are likely to be rather
			// common here, since Observe may have to query 100s of streams,
			// any one of which could be slow.
			observationCtx, cancel := context.WithTimeout(ctx, p.MaxDurationObservation)
			defer cancel()
			if err = p.DataSource.Observe(observationCtx, obs.StreamValues, dsOpts{p.Config.VerboseLogging, outctx, p.ConfigDigest}); err != nil {
				return nil, fmt.Errorf("DataSource.Observe error: %w", err)
			}
		}
	}

	serialized, err := p.ObservationCodec.Encode(obs)
	if err != nil {
		return nil, fmt.Errorf("Observation encode error: %w", err)
	}

	return serialized, nil
}

type Observation struct {
	// Attested (i.e. signed by f+1 oracles) retirement report from predecessor
	// protocol instance
	AttestedPredecessorRetirement []byte
	// Should this protocol instance be retired?
	ShouldRetire bool
	// Timestamp from when observation is made
	// Note that this is the timestamp immediately before we initiate any
	// observations
	UnixTimestampNanoseconds int64
	// Votes to remove/add channels. Subject to MAX_OBSERVATION_*_LENGTH limits
	RemoveChannelIDs map[llotypes.ChannelID]struct{}
	// Votes to add or replace channel definitions
	UpdateChannelDefinitions llotypes.ChannelDefinitions
	// Observed (numeric) stream values. Subject to
	// MaxObservationStreamValuesLength limit
	StreamValues StreamValues
}

// deterministic sort of channel IDs
func sortChannelIDs(cids []llotypes.ChannelID) {
	sort.Slice(cids, func(i, j int) bool {
		return cids[i] < cids[j]
	})
}
