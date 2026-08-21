package protocol

import (
	"errors"
	"fmt"
	"sort"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// VerifyChannelDefinitions applies the checks that any definition set must
// satisfy, whether it is being admitted or has already been committed.
func VerifyChannelDefinitions(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions) error {
	return verifyChannelDefinitions(codecs, channelDefs, nil)
}

// VerifyChannelDefinitionsForAdmission additionally applies the admission-only
// checks, restricted to admitting -- the channels being added or changed.
//
// The admission-only checks are the ones that reject a definition outright
// rather than merely stopping it from reporting: static expression analysis
// (ReportCodec implementations of AdmissionVerifier), calculated stream ID
// collisions, and feed ID uniqueness. Applying them to already-committed
// definitions would mean one grandfathered channel makes verification fail on
// every node, every round, which halts the protocol. Restricting them to
// admitting keeps the gate closed for anything new or changed while leaving
// what is already installed alone.
//
// A cross-definition check involves two channels and is reported against
// whichever of them is seen second, so such a finding is kept when either
// channel is in admitting.
//
// This is a local decision -- an oracle deciding what it is willing to vote for
// -- not a consensus-critical one, so oracles running different versions of the
// admission-only checks disagree only about what they vote for.
func VerifyChannelDefinitionsForAdmission(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions, admitting map[llotypes.ChannelID]struct{}) error {
	return verifyChannelDefinitions(codecs, channelDefs, admitting)
}

// ChangedChannelIDs returns the IDs of the channels desired holds that current
// does not hold identically: the set being added or changed, which is the
// admitting set to verify against.
func ChangedChannelIDs(current, desired llotypes.ChannelDefinitions) map[llotypes.ChannelID]struct{} {
	changed := make(map[llotypes.ChannelID]struct{})
	for channelID, cd := range desired {
		if prev, exists := current[channelID]; exists && prev.Equals(cd) {
			continue
		}
		changed[channelID] = struct{}{}
	}
	return changed
}

// admissionFinding is an admission-only check failure, together with every
// channel it implicates, so that it can be filtered by the admitting set.
type admissionFinding struct {
	channels []llotypes.ChannelID
	err      error
}

func (f admissionFinding) appliesTo(admitting map[llotypes.ChannelID]struct{}) bool {
	for _, channelID := range f.channels {
		if _, ok := admitting[channelID]; ok {
			return true
		}
	}
	return false
}

func verifyChannelDefinitions(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions, admitting map[llotypes.ChannelID]struct{}) (merr error) {
	if len(channelDefs) > MaxOutcomeChannelDefinitionsLength {
		return fmt.Errorf("too many channels, got: %d/%d", len(channelDefs), MaxOutcomeChannelDefinitionsLength)
	}

	// Verify in ascending channel ID order so that the errors a rejected
	// definitions file produces are the same on every oracle.
	channelIDs := make([]llotypes.ChannelID, 0, len(channelDefs))
	for channelID := range channelDefs {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })

	// Admission-only findings are collected as they are discovered and filtered
	// against admitting once at the end. The bookkeeping the cross-definition
	// checks rely on is built for the whole set either way, so that a finding
	// does not depend on which channels are being admitted.
	var admissionFindings []admissionFinding
	admit := func(err error, channels ...llotypes.ChannelID) {
		admissionFindings = append(admissionFindings, admissionFinding{channels: channels, err: err})
	}

	uniqueStreamIDs := make(map[llotypes.StreamID]struct{}, len(channelDefs))
	// Owners of every stream ID that will hold an aggregate: observed streams
	// come from the definitions, calculated streams from the expressions their
	// channel's opts declare. Both are collected here so that the uniqueness
	// check below can run once over the whole set.
	observedBy := make(map[llotypes.StreamID]llotypes.ChannelID, len(channelDefs))
	calculatedBy := make(map[llotypes.StreamID]llotypes.ChannelID)
	// Owners of every feed ID the definitions publish under. Two channels
	// sharing one publish conflicting reports for the same on-chain feed, so the
	// second is rejected.
	feedIDBy := make(map[[32]byte]llotypes.ChannelID)

	for _, channelID := range channelIDs {
		cd := channelDefs[channelID]
		if cd.Tombstone {
			continue
		}

		if len(cd.Streams) == 0 {
			merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has no streams", channelID))
			continue
		}
		if len(cd.Streams) > MaxStreamsPerChannel {
			merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has too many streams, got: %d/%d", channelID, len(cd.Streams), MaxStreamsPerChannel))
			continue
		}
		for _, strm := range cd.Streams {
			if strm.Aggregator == 0 {
				merr = errors.Join(merr, fmt.Errorf("ChannelDefinition with ID %d has stream %d with zero aggregator (this may indicate an uninitialized struct)", channelID, strm.StreamID))
				continue
			}
			uniqueStreamIDs[strm.StreamID] = struct{}{}
			// Calculated streams are derived from the opts that declare them and
			// are not stored on the definition, so anything listed here is
			// observed. Definitions written by older code may still carry their
			// calculated streams inline; those are skipped so that such a
			// channel is not reported as colliding with itself.
			if strm.Aggregator != llotypes.AggregatorCalculated {
				if _, ok := observedBy[strm.StreamID]; !ok {
					observedBy[strm.StreamID] = channelID
				}
			}
		}
		if HasCalculatedStreams(cd) {
			ids, err := CalculatedStreamIDs(nil, cd, channelID)
			if err != nil {
				admit(fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, err), channelID)
			}
			for _, streamID := range ids {
				if owner, ok := calculatedBy[streamID]; ok {
					admit(fmt.Errorf("ChannelDefinition with ID %d declares calculated stream %d already declared by channel %d", channelID, streamID, owner), channelID, owner)
					continue
				}
				calculatedBy[streamID] = channelID
			}
		}
		var verifyErr error
		if codec, ok := codecs[cd.ReportFormat]; ok {
			verifyErr = codec.Verify(cd)
			if verifyErr != nil {
				merr = errors.Join(merr, fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, verifyErr))
			}
			if av, ok := codec.(AdmissionVerifier); ok && verifyErr == nil {
				if err := av.VerifyForAdmission(cd); err != nil {
					admit(fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, err), channelID)
				}
			}
		}
		if feedIDer, ok := codecs[cd.ReportFormat].(FeedIDer); ok && verifyErr == nil {
			feedID, hasFeedID, err := feedIDer.FeedID(cd)
			switch {
			case err != nil:
				admit(fmt.Errorf("invalid ChannelDefinition with ID %d: failed to resolve feed ID: %w", channelID, err), channelID)
			case !hasFeedID:
			default:
				if owner, ok := feedIDBy[feedID]; ok {
					admit(fmt.Errorf("ChannelDefinition with ID %d has feed ID 0x%x already used by channel %d", channelID, feedID, owner), channelID, owner)
				} else {
					feedIDBy[feedID] = channelID
				}
			}
		}
		if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			if err := ValidateHistoryBackfillAgainstDefinitions(cd, channelDefs, 0); err != nil {
				merr = errors.Join(merr, fmt.Errorf("invalid history backfill channel %d: %w", channelID, err))
			}
			if err := ValidateHistoryBackfillTarget(cd, channelDefs); err != nil {
				admit(fmt.Errorf("invalid history backfill channel %d: %w", channelID, err), channelID)
			}
		}
		if verifyErr != nil {
			continue
		}
	}

	// A calculated stream that shares its ID with an observed stream would write
	// over an aggregate that is not its own. Evaluation refuses to do that, so
	// such a channel can never report, and the ID it clashes with is only
	// visible across the whole definition set -- which is why it is checked
	// here, once, rather than per channel. Only observed streams need a second
	// pass: calculated collisions are caught as they are collected above.
	for _, streamID := range sortedStreamIDs(calculatedBy) {
		if owner, ok := observedBy[streamID]; ok {
			admit(fmt.Errorf("ChannelDefinition with ID %d declares calculated stream %d, which channel %d observes", calculatedBy[streamID], streamID, owner), calculatedBy[streamID], owner)
		}
	}

	for _, finding := range admissionFindings {
		if finding.appliesTo(admitting) {
			merr = errors.Join(merr, finding.err)
		}
	}

	if merr != nil {
		return merr
	}
	if len(uniqueStreamIDs) > MaxObservationStreamValuesLength {
		return fmt.Errorf("too many unique stream IDs, got: %d/%d", len(uniqueStreamIDs), MaxObservationStreamValuesLength)
	}
	return nil
}

func sortedStreamIDs(m map[llotypes.StreamID]llotypes.ChannelID) []llotypes.StreamID {
	ids := make([]llotypes.StreamID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func SubtractChannelDefinitions(minuend llotypes.ChannelDefinitions, subtrahend llotypes.ChannelDefinitions, limit int) llotypes.ChannelDefinitions {
	differenceList := []ChannelDefinitionWithID{}
	for channelID, channelDefinition := range minuend {
		if _, ok := subtrahend[channelID]; !ok {
			differenceList = append(differenceList, ChannelDefinitionWithID{channelDefinition, channelID})
		}
	}

	// Sort so we return deterministic result
	sort.Slice(differenceList, func(i, j int) bool {
		return differenceList[i].ChannelID < differenceList[j].ChannelID
	})

	if len(differenceList) > limit {
		differenceList = differenceList[:limit]
	}

	difference := llotypes.ChannelDefinitions{}
	for _, defWithID := range differenceList {
		difference[defWithID.ChannelID] = defWithID.ChannelDefinition
	}

	return difference
}
