package protocol

import (
	"slices"
	"sync"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// channelGenerationsRetained bounds how many generations the cache memoizes.
// More than one is needed because rounds overlap: OCR3.1 runs Observation,
// StateTransition and Reports in separate goroutines, so a StateTransition for
// seqNr N+1 can be in flight while Reports for N (and a straggling N-1) still
// hold the definitions of an older record. Eviction never invalidates a
// generation somebody already holds; it only drops it from the memo index.
const channelGenerationsRetained = 4

// ChannelGeneration is an immutable snapshot of one channel-definitions record
// together with the decoded channel opts belonging to it. seqNr identifies the
// record (the v31 c/seqnr). The record is a pure function of that sequence
// number - both are written by the same StateTransition into the same
// replicated, atomically-committed store - so a generation is fully determined
// by its key.
//
// A generation retains no reference to the store it was built from: no
// KeyValueState reader or transaction (those are invalid once the plugin
// callback returns), no back-pointer to the cache, and no memory shared with a
// mutable value. It is therefore safe to hold for the duration of a round and
// across goroutines, and - crucially - it can never be repointed at a different
// record by a concurrently running round. That is what keeps report encoding
// from using opts that are ahead of (or behind) the definitions being reported.
type ChannelGeneration struct {
	seqNr uint64
	defs  llotypes.ChannelDefinitions
	opts  *OptsCache
}

func newChannelGeneration(seqNr uint64, defs llotypes.ChannelDefinitions) *ChannelGeneration {
	own := CloneChannelDefinitions(defs)
	return &ChannelGeneration{
		seqNr: seqNr,
		defs:  own,
		opts:  newSealedOptsCache(own),
	}
}

// SeqNr returns the sequence number of the record this generation was built
// from.
func (g *ChannelGeneration) SeqNr() uint64 {
	return g.seqNr
}

// Definitions returns the snapshot's channel definitions. They are read-only by
// contract: callers that mutate must clone first (see CloneChannelDefinitions).
func (g *ChannelGeneration) Definitions() llotypes.ChannelDefinitions {
	return g.defs
}

// Opts returns the decoded-opts store for exactly these definitions. It is
// sealed: decode-only, and never re-synced to another record.
func (g *ChannelGeneration) Opts() *OptsCache {
	return g.opts
}

// ChannelCache memoizes ChannelGenerations so that an unchanged record is read
// and decoded once rather than every round. It is purely a memo index: all
// consistency comes from generations being immutable and keyed by their record's
// sequence number.
//
// The lookup is by equality, not "cached is older": a node replaying history or
// restoring from a snapshot can legitimately present an older record, and
// serving newer definitions into an older round would diverge.
//
// A nil *ChannelCache is usable and simply memoizes nothing.
type ChannelCache struct {
	mu    sync.Mutex
	gens  map[uint64]*ChannelGeneration
	order []uint64 // insertion order, oldest first
}

func NewChannelCache() *ChannelCache {
	return &ChannelCache{
		gens: make(map[uint64]*ChannelGeneration, channelGenerationsRetained),
	}
}

// Load returns the generation for seqNr, calling build only on a miss. build
// supplies the definitions of that record; they are deep-copied into the
// generation, so build may return memory it does not own exclusively (a decoded
// KV record, a precursor's definitions) without the generation aliasing it.
func (c *ChannelCache) Load(seqNr uint64, build func() (llotypes.ChannelDefinitions, error)) (*ChannelGeneration, error) {
	if c == nil {
		defs, err := build()
		if err != nil {
			return nil, err
		}
		return newChannelGeneration(seqNr, defs), nil
	}

	if gen, ok := c.get(seqNr); ok {
		return gen, nil
	}

	defs, err := build()
	if err != nil {
		return nil, err
	}
	return c.store(newChannelGeneration(seqNr, defs)), nil
}

func (c *ChannelCache) get(seqNr uint64) (*ChannelGeneration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	gen, ok := c.gens[seqNr]
	return gen, ok
}

// store inserts gen unless a concurrent Load already built the same generation,
// in which case the existing one is returned so that a given sequence number
// resolves to a single generation for as long as it is retained.
func (c *ChannelCache) store(gen *ChannelGeneration) *ChannelGeneration {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.gens[gen.seqNr]; ok {
		return existing
	}
	if c.gens == nil {
		c.gens = make(map[uint64]*ChannelGeneration, channelGenerationsRetained)
	}
	c.gens[gen.seqNr] = gen
	c.order = append(c.order, gen.seqNr)
	for len(c.order) > channelGenerationsRetained {
		delete(c.gens, c.order[0])
		c.order = c.order[1:]
	}
	return gen
}

// CloneChannelDefinitions returns a deep copy: the per-channel Streams slices
// and raw opts bytes are copied too, so appending to or otherwise mutating the
// clone cannot reach memory another round is reading.
func CloneChannelDefinitions(in llotypes.ChannelDefinitions) llotypes.ChannelDefinitions {
	out := make(llotypes.ChannelDefinitions, len(in))
	for id, cd := range in {
		cd.Streams = slices.Clone(cd.Streams)
		cd.Opts = slices.Clone(cd.Opts)
		out[id] = cd
	}
	return out
}
