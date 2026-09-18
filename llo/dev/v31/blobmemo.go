package llo

import (
	"sync"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// blobPayloadCache memoizes the stream values decoded from blob payloads within
// one sequence number. ValidateObservation and StateTransition decode the same
// observations in the same round, and every FetchBlob re-verifies the blob's
// certificate and re-reads its payload before the plugin decompresses and
// unmarshals it again, so the second decode is entirely repeated work.
//
// Entries are keyed by the marshaled blob handle and scoped to a single
// sequence number. That scope is what keeps the memo consistent with the blob
// transport, which refuses a handle that has expired as of the round's sequence
// number: a hit can never resurrect a blob the round itself would have
// rejected. What one round can hold is bounded by what its observations may
// reference, at most maxObservationBlobHandles handles per observation, each
// contributing at most maxObservationDecompressedBytes, and the whole map is
// dropped when the sequence number advances.
//
// Decoded stream values are treated as immutable: a hit copies the map entries
// into the observation rather than handing out the memoized map.
type blobPayloadCache struct {
	mu      sync.Mutex
	seqNr   uint64
	entries map[string]blobPayloadEntry
}

// blobPayloadEntry is one memoized blob payload. size is the decompressed byte
// count, memoized alongside the values because it is charged against the
// observation's decompression budget: a hit must consume exactly what the
// original decode consumed, or the same observation would decode differently on
// a hit than on a miss.
type blobPayloadEntry struct {
	values protocol.StreamValues
	size   int
}

func newBlobPayloadCache() *blobPayloadCache {
	return &blobPayloadCache{}
}

// round returns the memo scoped to seqNr, discarding whatever was held for an
// earlier one. Returns nil for a nil cache, which disables memoization.
func (c *blobPayloadCache) round(seqNr uint64) *roundBlobPayloads {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seqNr != seqNr || c.entries == nil {
		c.seqNr = seqNr
		c.entries = make(map[string]blobPayloadEntry)
	}
	return &roundBlobPayloads{cache: c, seqNr: seqNr}
}

// roundBlobPayloads is a handle on the memo for one sequence number. Reads and
// writes through a handle whose round has been superseded are dropped, so a
// round can never see another round's payloads.
type roundBlobPayloads struct {
	cache *blobPayloadCache
	seqNr uint64
}

func (r *roundBlobPayloads) get(handle []byte) (blobPayloadEntry, bool) {
	if r == nil {
		return blobPayloadEntry{}, false
	}
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	if r.cache.seqNr != r.seqNr {
		return blobPayloadEntry{}, false
	}
	entry, ok := r.cache.entries[string(handle)]
	return entry, ok
}

func (r *roundBlobPayloads) put(handle []byte, entry blobPayloadEntry) {
	if r == nil {
		return
	}
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	if r.cache.seqNr != r.seqNr {
		return
	}
	r.cache.entries[string(handle)] = entry
}
