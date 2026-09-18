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
	return verifyChannelDefinitions(codecs, channelDefs, nil, nil)
}

// VerifyChannelDefinitionsWithCache is VerifyChannelDefinitions, memoizing the
// per-definition checks in cache. A nil cache memoizes nothing.
func VerifyChannelDefinitionsWithCache(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions, cache *ChannelAnalysisCache) error {
	return verifyChannelDefinitions(codecs, channelDefs, nil, cache)
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
	return verifyChannelDefinitions(codecs, channelDefs, admitting, nil)
}

// VerifyChannelDefinitionsForAdmissionWithCache is
// VerifyChannelDefinitionsForAdmission, memoizing the per-definition checks in
// cache. A nil cache memoizes nothing.
func VerifyChannelDefinitionsForAdmissionWithCache(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions, admitting map[llotypes.ChannelID]struct{}, cache *ChannelAnalysisCache) error {
	return verifyChannelDefinitions(codecs, channelDefs, admitting, cache)
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
//
// A whole-set finding (a budget summed over every channel) implicates no
// particular channel: it is a property of the set as a whole, and the channels
// that made it exceed the budget are not distinguishable from the ones that did
// not. Such a finding sets wholeSet and applies whenever anything is being
// admitted, which is what stops a set already over budget from growing while
// leaving a set that is merely already over it alone.
type admissionFinding struct {
	channels []llotypes.ChannelID
	wholeSet bool
	err      error
}

func (f admissionFinding) appliesTo(admitting map[llotypes.ChannelID]struct{}) bool {
	if f.wholeSet {
		return len(admitting) > 0
	}
	for _, channelID := range f.channels {
		if _, ok := admitting[channelID]; ok {
			return true
		}
	}
	return false
}

// UnverifiableChannelIDs returns the channels of an already-committed
// definition set that fail a baseline check, so that a node can skip them
// instead of halting.
//
// Baseline checks are the ones every definition set must satisfy, admitted or
// committed, and VerifyChannelDefinitions reports them as a single error. That
// is the right answer on the admission path, where the set can simply be
// rejected.
// Committed state is replicated, so failing verification on every
// node would halt the DON with no way out, as the nodes would never vote
// to remove offending channels.
// For a DON where all participants share the same version this is unreachable,
// as ValidateObservation runs the same baseline checks over the merged set,
// but a version skew can make it reachable.
// Report the finding and carry on, which is the same treatment the
// admission-only findings get in voteOnChannels.
//
// Attributing the findings instead lets the caller drop just those channels and
// keep going, which is the same treatment the admission-only findings already
// get. The returned error carries the whole-set baseline findings, which
// implicate no particular channel and so cannot be skipped selectively.
func UnverifiableChannelIDs(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions) (map[llotypes.ChannelID]struct{}, error) {
	return UnverifiableChannelIDsWithCache(codecs, channelDefs, nil)
}

// UnverifiableChannelIDsWithCache is UnverifiableChannelIDs, memoizing the
// per-definition checks in cache. A nil cache memoizes nothing.
func UnverifiableChannelIDsWithCache(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions, cache *ChannelAnalysisCache) (map[llotypes.ChannelID]struct{}, error) {
	res := analyzeChannelDefinitions(codecs, channelDefs, cache)
	ids := make(map[llotypes.ChannelID]struct{}, len(res.channelErrs))
	for channelID := range res.channelErrs {
		ids[channelID] = struct{}{}
	}
	return ids, res.setErr()
}

// verifyResult is the outcome of analyzing a definition set: baseline findings
// attributed to the channel that produced them, baseline findings that belong
// to the set as a whole, and the admission-only findings, which are filtered by
// the admitting set only when an error is materialized.
type verifyResult struct {
	channelErrs       map[llotypes.ChannelID]error
	wholeSetErr       error
	uniqueStreamIDs   int
	admissionFindings []admissionFinding
}

// setErr reports the baseline findings that implicate no particular channel.
// The unique-stream-ID budget is one of them, but it is only meaningful once
// the per-channel findings are clear (a definition that failed verification may
// have contributed stream IDs that a corrected one would not), so it is
// reported only when nothing else failed.
func (r verifyResult) setErr() error {
	if r.wholeSetErr != nil {
		return r.wholeSetErr
	}
	if len(r.channelErrs) == 0 && r.uniqueStreamIDs > MaxObservationStreamValuesLength {
		return fmt.Errorf("too many unique stream IDs, got: %d/%d", r.uniqueStreamIDs, MaxObservationStreamValuesLength)
	}
	return nil
}

// err joins every finding that applies into the single error the verification
// entry points return. Channel findings are joined in ascending channel ID
// order so that a rejected definitions file produces the same error on every
// oracle.
func (r verifyResult) err(admitting map[llotypes.ChannelID]struct{}) error {
	if r.wholeSetErr != nil {
		return r.wholeSetErr
	}

	var merr error
	channelIDs := make([]llotypes.ChannelID, 0, len(r.channelErrs))
	for channelID := range r.channelErrs {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })
	for _, channelID := range channelIDs {
		merr = errors.Join(merr, r.channelErrs[channelID])
	}

	for _, finding := range r.admissionFindings {
		if finding.appliesTo(admitting) {
			merr = errors.Join(merr, finding.err)
		}
	}

	if merr != nil {
		return merr
	}
	if r.uniqueStreamIDs > MaxObservationStreamValuesLength {
		return fmt.Errorf("too many unique stream IDs, got: %d/%d", r.uniqueStreamIDs, MaxObservationStreamValuesLength)
	}
	return nil
}

func verifyChannelDefinitions(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions, admitting map[llotypes.ChannelID]struct{}, cache *ChannelAnalysisCache) error {
	return analyzeChannelDefinitions(codecs, channelDefs, cache).err(admitting)
}

// analyzeChannelDefinitions applies every check to the set. The checks that
// need nothing but a single definition are taken from cache when it already
// holds them for that exact definition (see ChannelAnalysisCache); everything
// that involves more than one definition is computed here on every call, so
// that a finding never depends on what was analyzed before.
func analyzeChannelDefinitions(codecs map[llotypes.ReportFormat]ReportCodec, channelDefs llotypes.ChannelDefinitions, cache *ChannelAnalysisCache) (res verifyResult) {
	res.channelErrs = make(map[llotypes.ChannelID]error)

	if len(channelDefs) > MaxOutcomeChannelDefinitionsLength {
		res.wholeSetErr = fmt.Errorf("too many channels, got: %d/%d", len(channelDefs), MaxOutcomeChannelDefinitionsLength)
		return res
	}

	// Verify in ascending channel ID order so that the errors a rejected
	// definitions file produces are the same on every oracle.
	channelIDs := make([]llotypes.ChannelID, 0, len(channelDefs))
	for channelID := range channelDefs {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })

	// Baseline findings are attributed to the channel that produced them; a
	// cross-definition baseline finding is attributed to every channel it
	// implicates, so that skipping any one of them clears it.
	base := func(err error, channels ...llotypes.ChannelID) {
		for _, channelID := range channels {
			res.channelErrs[channelID] = errors.Join(res.channelErrs[channelID], err)
		}
	}
	// Admission-only findings are collected as they are discovered and filtered
	// against admitting once at the end. The bookkeeping the cross-definition
	// checks rely on is built for the whole set either way, so that a finding
	// does not depend on which channels are being admitted.
	admit := func(err error, channels ...llotypes.ChannelID) {
		res.admissionFindings = append(res.admissionFindings, admissionFinding{channels: channels, err: err})
	}
	admitSet := func(err error) {
		res.admissionFindings = append(res.admissionFindings, admissionFinding{wholeSet: true, err: err})
	}

	// Whole-set budgets, accumulated over the channels the loop below visits
	// (tombstones excluded: they carry neither streams nor opts that anything
	// reads) and checked once at the end.
	var totalStreamEntries, totalOptsBytes int

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
			base(fmt.Errorf("ChannelDefinition with ID %d has no streams", channelID), channelID)
			continue
		}
		if len(cd.Streams) > MaxStreamsPerChannel {
			base(fmt.Errorf("ChannelDefinition with ID %d has too many streams, got: %d/%d", channelID, len(cd.Streams), MaxStreamsPerChannel), channelID)
			continue
		}
		totalStreamEntries += len(cd.Streams)
		// Opts are opaque bytes, so length is the only thing that can be
		// checked here. Admission-only: a committed definition carrying an
		// oversized blob is left alone rather than failing verification on every
		// oracle, every round.
		if len(cd.Opts) > MaxChannelOptsBytes {
			admit(fmt.Errorf("ChannelDefinition with ID %d has opts that are too long, got: %d/%d", channelID, len(cd.Opts), MaxChannelOptsBytes), channelID)
		}
		totalOptsBytes += len(cd.Opts)
		for _, strm := range cd.Streams {
			if strm.Aggregator == 0 {
				base(fmt.Errorf("ChannelDefinition with ID %d has stream %d with zero aggregator (this may indicate an uninitialized struct)", channelID, strm.StreamID), channelID)
				continue
			}
			// An aggregator this binary does not know has no aggregator
			// function, so the pair can never produce an aggregate. Rejected at
			// admission only: a committed definition carrying one is left alone
			// (aggregation skips the pair) rather than failing verification on
			// every oracle, every round.
			if strm.Aggregator != llotypes.AggregatorCalculated && GetAggregatorFunc(strm.Aggregator) == nil {
				admit(fmt.Errorf("ChannelDefinition with ID %d has stream %d with unknown aggregator %d", channelID, strm.StreamID, strm.Aggregator), channelID)
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
		facts := channelFactsFor(cache, codecs, channelID, cd)

		if HasCalculatedStreams(cd) {
			if facts.calculatedErr != nil {
				admit(fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, facts.calculatedErr), channelID)
			}
			for _, streamID := range facts.calculatedIDs {
				if owner, ok := calculatedBy[streamID]; ok {
					admit(fmt.Errorf("ChannelDefinition with ID %d declares calculated stream %d already declared by channel %d", channelID, streamID, owner), channelID, owner)
					continue
				}
				calculatedBy[streamID] = channelID
			}
		}
		if facts.verifyErr != nil {
			base(fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, facts.verifyErr), channelID)
		}
		if facts.admissionErr != nil {
			admit(fmt.Errorf("invalid ChannelDefinition with ID %d: %w", channelID, facts.admissionErr), channelID)
		}
		switch {
		case facts.feedIDErr != nil:
			admit(fmt.Errorf("invalid ChannelDefinition with ID %d: failed to resolve feed ID: %w", channelID, facts.feedIDErr), channelID)
		case !facts.hasFeedID:
		default:
			if owner, ok := feedIDBy[facts.feedID]; ok {
				admit(fmt.Errorf("ChannelDefinition with ID %d has feed ID 0x%x already used by channel %d", channelID, facts.feedID, owner), channelID, owner)
			} else {
				feedIDBy[facts.feedID] = channelID
			}
		}
		if cd.ReportFormat == llotypes.ReportFormatHistoryBackfill {
			if err := ValidateHistoryBackfillAgainstDefinitions(cd, channelDefs, 0); err != nil {
				base(fmt.Errorf("invalid history backfill channel %d: %w", channelID, err), channelID)
			}
			if err := ValidateHistoryBackfillTarget(cd, channelDefs); err != nil {
				admit(fmt.Errorf("invalid history backfill channel %d: %w", channelID, err), channelID)
			}
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

	// Whole-set budgets. Both are what the sizes of the channel-definitions
	// record and the precursor actually depend on; see the limits they name.
	if totalStreamEntries > MaxTotalStreamEntries {
		admitSet(fmt.Errorf("too many stream entries across all channels, got: %d/%d", totalStreamEntries, MaxTotalStreamEntries))
	}
	if totalOptsBytes > MaxTotalOptsBytes {
		admitSet(fmt.Errorf("too many opts bytes across all channels, got: %d/%d", totalOptsBytes, MaxTotalOptsBytes))
	}

	res.uniqueStreamIDs = len(uniqueStreamIDs)
	return res
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
