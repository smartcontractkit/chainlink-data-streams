package protocol

import (
	"sync"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// channelFacts is everything analyzeChannelDefinitions derives from a single
// channel definition by decoding its opts: the codec's verdicts, the feed ID the
// channel publishes under, and the calculated stream IDs its expressions
// declare.
//
// Every one of these is contractually a pure function of the definition alone
// (see ReportCodec.Verify, AdmissionVerifier and FeedIDer), which is what makes
// them cacheable. Findings that involve more than one definition are not here:
// they are recomputed from these facts on every analysis, so that a finding
// never depends on which set was analyzed first.
type channelFacts struct {
	verifyErr     error
	admissionErr  error
	feedID        [32]byte
	hasFeedID     bool
	feedIDErr     error
	calculatedIDs []llotypes.StreamID
	calculatedErr error
}

// ChannelAnalysisCache memoizes channelFacts so that an unchanged channel
// definition has its opts decoded once rather than on every analysis.
//
// Verification runs several times per round over overlapping sets: the
// committed definitions and the desired ones in Observation, and the set each
// update-carrying observation advocates in ValidateObservation, which runs in
// its own goroutine. Decoding the opts dominates all of them, and between them
// the definitions are almost entirely the same and almost never change.
//
// Entries are keyed by channel ID and validated against the definition they
// were derived from, so a hit is an exact identity check rather than a digest
// comparison. A digest would be cheaper by a hair and would trade that
// exactness for the possibility of serving a verdict for a definition that was
// never verified, on a set whose contents are decided by vote.
//
// Definitions being changed alternate between the committed and the desired
// value within a round, so the cache holds a small number of entries per
// channel rather than one. The overlapping sets agree on everything else.
//
// A nil *ChannelAnalysisCache is usable and simply memoizes nothing.
type ChannelAnalysisCache struct {
	mu    sync.Mutex
	facts map[llotypes.ChannelID][]cachedChannelFacts
}

// cachedChannelFacts pairs the facts with the definition they were derived
// from. cd is the identity of the entry, not payload: a lookup is a hit only
// when the definition presented equals this one.
type cachedChannelFacts struct {
	cd    llotypes.ChannelDefinition
	facts channelFacts
}

// channelFactsRetained bounds how many definitions of one channel are
// remembered. Two is what a round asks for while a channel is being changed
// (the committed value and the proposed one); the third leaves room for a
// second proposal in flight.
const channelFactsRetained = 3

func NewChannelAnalysisCache() *ChannelAnalysisCache {
	return &ChannelAnalysisCache{
		facts: make(map[llotypes.ChannelID][]cachedChannelFacts),
	}
}

// Prune drops the entries of every channel channelDefs does not hold. Verifying
// a set does not by itself mean the channels it omits are gone, so this is for
// callers that know the committed set: on the v31 hot path, the round that
// loads it.
func (c *ChannelAnalysisCache) Prune(channelDefs llotypes.ChannelDefinitions) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for channelID := range c.facts {
		if _, ok := channelDefs[channelID]; !ok {
			delete(c.facts, channelID)
		}
	}
}

// get returns the facts derived from exactly cd, if they are cached.
func (c *ChannelAnalysisCache) get(channelID llotypes.ChannelID, cd llotypes.ChannelDefinition) (channelFacts, bool) {
	if c == nil {
		return channelFacts{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.facts[channelID] {
		if entry.cd.Equals(cd) {
			return entry.facts, true
		}
	}
	return channelFacts{}, false
}

// put records facts as derived from cd, evicting the channel's oldest entry
// once the bound is reached. cd is cloned: the caller's definition may share
// memory with a set that is mutated later, and the entry's identity has to stay
// what it was derived from.
func (c *ChannelAnalysisCache) put(channelID llotypes.ChannelID, cd llotypes.ChannelDefinition, facts channelFacts) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.facts[channelID]
	for _, entry := range entries {
		if entry.cd.Equals(cd) {
			// A concurrent analysis got there first. The facts are a pure
			// function of the definition, so the two agree.
			return
		}
	}
	if c.facts == nil {
		c.facts = make(map[llotypes.ChannelID][]cachedChannelFacts)
	}
	entries = append(entries, cachedChannelFacts{cd: cloneChannelDefinition(cd), facts: facts})
	if len(entries) > channelFactsRetained {
		entries = entries[len(entries)-channelFactsRetained:]
	}
	c.facts[channelID] = entries
}

// channelFactsFor returns the facts for cd, decoding its opts only when they are
// not already cached.
func channelFactsFor(cache *ChannelAnalysisCache, codecs map[llotypes.ReportFormat]ReportCodec, channelID llotypes.ChannelID, cd llotypes.ChannelDefinition) channelFacts {
	if facts, ok := cache.get(channelID, cd); ok {
		return facts
	}
	facts := deriveChannelFacts(codecs, channelID, cd)
	cache.put(channelID, cd, facts)
	return facts
}

// deriveChannelFacts decodes the channel's opts and applies every check that
// needs nothing but this definition. The errors are returned unwrapped: the
// caller wraps them with the channel ID, so that the same facts read the same
// way whichever set they are being reported against.
func deriveChannelFacts(codecs map[llotypes.ReportFormat]ReportCodec, channelID llotypes.ChannelID, cd llotypes.ChannelDefinition) (facts channelFacts) {
	if HasCalculatedStreams(cd) {
		facts.calculatedIDs, facts.calculatedErr = CalculatedStreamIDs(nil, cd, channelID)
	}

	codec, ok := codecs[cd.ReportFormat]
	if !ok {
		return facts
	}

	facts.verifyErr = codec.Verify(cd)
	if facts.verifyErr != nil {
		// Verify and FeedID are only consulted for definitions Verify accepted.
		return facts
	}
	if av, ok := codec.(AdmissionVerifier); ok {
		facts.admissionErr = av.VerifyForAdmission(cd)
	}
	if feedIDer, ok := codec.(FeedIDer); ok {
		facts.feedID, facts.hasFeedID, facts.feedIDErr = feedIDer.FeedID(cd)
	}
	return facts
}
