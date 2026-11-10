package protocol

import (
	"crypto/sha256"
	"errors"
	"sync"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// merkleBundleCacheSize bounds the bundles remembered. libocr verifies every
// peer's signature over the same bundle and the transmitter expands it after
// attestation, so a few recent rounds are enough to build each tree once.
const merkleBundleCacheSize = 32

// MerkleBundle is the tree built over the leaves of a bundle. It does not keep
// the leaves, which are as large as the encoded bundle.
type MerkleBundle struct {
	Family    llotypes.ReportFormat
	LeafCount uint32
	Root      MerkleHash

	tree merkleTree
}

// Expand returns one report per leaf of encoded, in leaf order, each with its
// proof to the bundle root. It decodes encoded again and reuses the tree, so
// encoded must be the bytes this MerkleBundle was loaded from.
func (m *MerkleBundle) Expand(encoded []byte) ([]*LLOMerkleReportProto, error) {
	bundle, err := DecodeMerkleBundle(encoded)
	if err != nil {
		return nil, err
	}
	if llotypes.ReportFormat(bundle.Family) != m.Family || len(bundle.Leaves) != int(m.LeafCount) {
		return nil, errors.New("bundle does not match the loaded merkle tree")
	}
	return expandMerkleBundle(bundle, m.tree), nil
}

type merkleBundleCache struct {
	mu      sync.Mutex
	size    int
	entries map[[32]byte]*MerkleBundle
	order   [][32]byte
}

func newMerkleBundleCache(size int) *merkleBundleCache {
	return &merkleBundleCache{size: size, entries: make(map[[32]byte]*MerkleBundle, size)}
}

// load decodes the bundle and builds its tree, or returns the cached result
// when the same bytes were seen before.
func (c *merkleBundleCache) load(b []byte) (*MerkleBundle, error) {
	key := sha256.Sum256(b)
	c.mu.Lock()
	cached, ok := c.entries[key]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	bundle, err := DecodeMerkleBundle(b)
	if err != nil {
		return nil, err
	}
	tree, err := bundleTree(bundle)
	if err != nil {
		return nil, err
	}
	entry := &MerkleBundle{
		Family:    llotypes.ReportFormat(bundle.Family),
		LeafCount: uint32(len(bundle.Leaves)), //nolint:gosec // bounded by MaxMerkleBundleLeaves on decode
		Root:      tree.root(),
		tree:      tree,
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		return existing, nil
	}
	if len(c.order) == c.size {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[key] = entry
	c.order = append(c.order, key)
	return entry, nil
}

var sharedMerkleBundles = newMerkleBundleCache(merkleBundleCacheSize)

// LoadMerkleBundle decodes a bundle and builds its tree. The tree is shared by
// the keyring, which signs the root, and the transmitter, which expands the
// attested bundle, so it is built once per bundle.
func LoadMerkleBundle(b []byte) (*MerkleBundle, error) {
	return sharedMerkleBundles.load(b)
}
