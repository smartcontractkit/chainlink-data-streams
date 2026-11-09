package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/crypto"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// Merkle batched report signing.
//
// Channels with SigningSchemeMerkle are not signed one report at a time.
// Reports groups them by signing family into bundle reports, the keyring signs
// the Merkle root of each bundle once, and the transmitter expands an attested
// bundle into one delivery per leaf, carrying the proof from the leaf to the
// root. The verifier recomputes the root from the delivery and checks the
// signatures over it.
//
// The tree is domain separated as in RFC 6962. A leaf hashes as
// H(0x00 ‖ family ‖ reportFormat ‖ channelID ‖ H(report)) and a node as
// H(0x01 ‖ left ‖ right). An odd node is promoted unchanged to the next level,
// never duplicated, and the leaf count is part of the signed message, so two
// different leaf sets cannot share a signed root. Integers are big endian:
// uint32 for the family, format, channel ID and leaf count, uint64 for seqNr.
// H is keccak256 for the EVM family, the only family so far.

const (
	merkleLeafPrefix byte = 0x00
	merkleNodePrefix byte = 0x01

	// MerkleSigningDomain prefixes the message a bundle root is signed under.
	MerkleSigningDomain = "LLO_MERKLE_V1"

	// MaxMerkleBundleLeaves bounds the leaves of one bundle. A bundle holds at
	// most one report per channel definition.
	MaxMerkleBundleLeaves = MaxOutcomeChannelDefinitionsLength
)

// MerkleHash is a node of the tree.
type MerkleHash = [32]byte

// MerkleFamily returns the signing family that channels in format join under
// SigningSchemeMerkle, and false if format has none.
//
// The EVM family covers every format with a codec in llo/reportcodec/evm. They
// all produce reports for EVM verifiers and hash with keccak256, so one bundle
// can hold all of them. An EVMStreamlined channel joins only with a feed ID, so
// its reports start with one like the others (see ReportCodecEVMStreamlined).
func MerkleFamily(format llotypes.ReportFormat) (llotypes.ReportFormat, bool) {
	switch format {
	case llotypes.ReportFormatEVMPremiumLegacy,
		llotypes.ReportFormatEVMABIEncodeUnpacked,
		llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		llotypes.ReportFormatEVMStreamlined:
		return llotypes.ReportFormatEVMMerkle, true
	default:
		return 0, false
	}
}

// IsMerkleFamily reports whether format is a signing family.
func IsMerkleFamily(format llotypes.ReportFormat) bool {
	return format == llotypes.ReportFormatEVMMerkle
}

// SigningFamily returns how the reports of a channel defined by cd are signed:
// the family they join as Merkle leaves and true, or false when each report is
// signed on its own. It fails for an unknown scheme, or SigningSchemeMerkle on
// a format with no signing family.
func SigningFamily(cd llotypes.ChannelDefinition) (llotypes.ReportFormat, bool, error) {
	switch cd.SigningScheme {
	case llotypes.SigningSchemeDefault:
		return 0, false, nil
	case llotypes.SigningSchemeMerkle:
		family, ok := MerkleFamily(cd.ReportFormat)
		if !ok {
			return 0, false, fmt.Errorf("signing scheme %q is not supported for report format %s", cd.SigningScheme, cd.ReportFormat)
		}
		return family, true, nil
	default:
		return 0, false, fmt.Errorf("unknown signing scheme %q", cd.SigningScheme)
	}
}

// VerifySigningScheme checks how a channel definition is signed. A signing
// family is never a channel format, and SigningFamily must accept the scheme.
func VerifySigningScheme(cd llotypes.ChannelDefinition) error {
	if IsMerkleFamily(cd.ReportFormat) {
		return fmt.Errorf("report format %s is a signing family, not a channel format", cd.ReportFormat)
	}
	_, _, err := SigningFamily(cd)
	return err
}

func keccak(parts ...[]byte) (h MerkleHash) {
	copy(h[:], crypto.Keccak256(parts...))
	return h
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// MerkleLeafHash returns the leaf hash of leaf in a bundle of family.
func MerkleLeafHash(family llotypes.ReportFormat, leaf *LLOMerkleLeafProto) MerkleHash {
	reportHash := keccak(leaf.Report)
	return keccak([]byte{merkleLeafPrefix}, be32(uint32(family)), be32(leaf.ReportFormat), be32(leaf.ChannelID), reportHash[:])
}

func merkleNodeHash(left, right MerkleHash) MerkleHash {
	return keccak([]byte{merkleNodePrefix}, left[:], right[:])
}

// MerkleSigningMessage returns the message the keyring signs for a bundle.
// The config digest and seqNr play the role of the report context, so a root
// cannot be replayed into another round or another configuration.
func MerkleSigningMessage(digest ocr2types.ConfigDigest, seqNr uint64, family llotypes.ReportFormat, leafCount uint32, root MerkleHash) MerkleHash {
	return keccak([]byte(MerkleSigningDomain), digest[:], binary.BigEndian.AppendUint64(nil, seqNr), be32(uint32(family)), be32(leafCount), root[:])
}

// merkleTree holds every level of a tree, leaves first and the root last.
type merkleTree [][]MerkleHash

func newMerkleTree(leaves []MerkleHash) (merkleTree, error) {
	if len(leaves) == 0 {
		return nil, errors.New("merkle tree needs at least one leaf")
	}
	tree := merkleTree{leaves}
	for level := leaves; len(level) > 1; {
		next := make([]MerkleHash, 0, (len(level)+1)/2)
		for i := 0; i+1 < len(level); i += 2 {
			next = append(next, merkleNodeHash(level[i], level[i+1]))
		}
		if len(level)%2 == 1 {
			next = append(next, level[len(level)-1])
		}
		tree = append(tree, next)
		level = next
	}
	return tree, nil
}

func (t merkleTree) root() MerkleHash { return t[len(t)-1][0] }

// proof returns the sibling hashes from leaf index to the root. A level where
// the node is promoted contributes none.
func (t merkleTree) proof(index int) [][]byte {
	var proof [][]byte
	for _, level := range t[:len(t)-1] {
		if sibling := index ^ 1; sibling < len(level) {
			proof = append(proof, level[sibling][:])
		}
		index /= 2
	}
	return proof
}

// MerkleRoot returns the root over leaves.
func MerkleRoot(leaves []MerkleHash) (MerkleHash, error) {
	tree, err := newMerkleTree(leaves)
	if err != nil {
		return MerkleHash{}, err
	}
	return tree.root(), nil
}

// MerkleRootFromProof returns the root that leaf at index of a tree with
// leafCount leaves hashes up to with proof. It fails when the proof has the
// wrong length for the position, so a valid proof cannot be padded.
func MerkleRootFromProof(leaf MerkleHash, index, leafCount uint32, proof [][]byte) (MerkleHash, error) {
	if leafCount == 0 || index >= leafCount {
		return MerkleHash{}, fmt.Errorf("leaf index %d out of range for %d leaves", index, leafCount)
	}
	h := leaf
	for n := leafCount; n > 1; n = (n + 1) / 2 {
		if index^1 < n {
			if len(proof) == 0 {
				return MerkleHash{}, errors.New("merkle proof too short")
			}
			if len(proof[0]) != len(MerkleHash{}) {
				return MerkleHash{}, fmt.Errorf("merkle proof hash has %d bytes, expected %d", len(proof[0]), len(MerkleHash{}))
			}
			sibling := MerkleHash(proof[0])
			proof = proof[1:]
			if index%2 == 0 {
				h = merkleNodeHash(h, sibling)
			} else {
				h = merkleNodeHash(sibling, h)
			}
		}
		index /= 2
	}
	if len(proof) != 0 {
		return MerkleHash{}, errors.New("merkle proof too long")
	}
	return h, nil
}

// EncodeMerkleBundle encodes a bundle deterministically, so every oracle
// produces the same bytes from the same leaves.
func EncodeMerkleBundle(bundle *LLOMerkleBundleProto) ([]byte, error) {
	return deterministicMarshal.Marshal(bundle)
}

// DecodeMerkleBundle decodes and checks a bundle: a known family, between one
// and MaxMerkleBundleLeaves leaves, and every leaf in a member format of the
// family.
func DecodeMerkleBundle(b []byte) (*LLOMerkleBundleProto, error) {
	bundle := new(LLOMerkleBundleProto)
	if err := proto.Unmarshal(b, bundle); err != nil {
		return nil, fmt.Errorf("failed to decode merkle bundle: %w", err)
	}
	family := llotypes.ReportFormat(bundle.Family)
	if !IsMerkleFamily(family) {
		return nil, fmt.Errorf("merkle bundle has unknown family %s", family)
	}
	if len(bundle.Leaves) == 0 || len(bundle.Leaves) > MaxMerkleBundleLeaves {
		return nil, fmt.Errorf("merkle bundle has %d leaves, expected between 1 and %d", len(bundle.Leaves), MaxMerkleBundleLeaves)
	}
	for i, leaf := range bundle.Leaves {
		if err := verifyMerkleLeaf(family, leaf); err != nil {
			return nil, fmt.Errorf("merkle bundle leaf %d: %w", i, err)
		}
	}
	return bundle, nil
}

func verifyMerkleLeaf(family llotypes.ReportFormat, leaf *LLOMerkleLeafProto) error {
	if leaf == nil {
		return errors.New("nil leaf")
	}
	if f, ok := MerkleFamily(llotypes.ReportFormat(leaf.ReportFormat)); !ok || f != family {
		return fmt.Errorf("report format %s is not a member of family %s", llotypes.ReportFormat(leaf.ReportFormat), family)
	}
	return nil
}

func bundleTree(bundle *LLOMerkleBundleProto) (merkleTree, error) {
	family := llotypes.ReportFormat(bundle.Family)
	leaves := make([]MerkleHash, len(bundle.Leaves))
	for i, leaf := range bundle.Leaves {
		leaves[i] = MerkleLeafHash(family, leaf)
	}
	return newMerkleTree(leaves)
}

// MerkleBundleRoot returns the root of a decoded bundle.
func MerkleBundleRoot(bundle *LLOMerkleBundleProto) (MerkleHash, error) {
	tree, err := bundleTree(bundle)
	if err != nil {
		return MerkleHash{}, err
	}
	return tree.root(), nil
}

// ExpandMerkleBundle returns one delivery per leaf of a decoded bundle, in
// leaf order, each with its proof to the bundle root.
func ExpandMerkleBundle(bundle *LLOMerkleBundleProto) ([]*LLOMerkleDeliveryProto, error) {
	tree, err := bundleTree(bundle)
	if err != nil {
		return nil, err
	}
	return expandMerkleBundle(bundle, tree), nil
}

func expandMerkleBundle(bundle *LLOMerkleBundleProto, tree merkleTree) []*LLOMerkleDeliveryProto {
	leafCount := uint32(len(bundle.Leaves)) //nolint:gosec // bounded by MaxMerkleBundleLeaves on decode
	deliveries := make([]*LLOMerkleDeliveryProto, len(bundle.Leaves))
	for i, leaf := range bundle.Leaves {
		deliveries[i] = &LLOMerkleDeliveryProto{
			Family:    bundle.Family,
			LeafCount: leafCount,
			LeafIndex: uint32(i), //nolint:gosec // below leafCount
			Leaf:      leaf,
			Proof:     tree.proof(i),
		}
	}
	return deliveries
}

// EncodeMerkleDelivery encodes a delivery deterministically.
func EncodeMerkleDelivery(delivery *LLOMerkleDeliveryProto) ([]byte, error) {
	return deterministicMarshal.Marshal(delivery)
}

// DecodeMerkleDelivery decodes a delivery and returns the root its leaf and
// proof hash up to. The caller checks the signatures over
// MerkleSigningMessage with that root.
func DecodeMerkleDelivery(b []byte) (*LLOMerkleDeliveryProto, MerkleHash, error) {
	delivery := new(LLOMerkleDeliveryProto)
	if err := proto.Unmarshal(b, delivery); err != nil {
		return nil, MerkleHash{}, fmt.Errorf("failed to decode merkle delivery: %w", err)
	}
	family := llotypes.ReportFormat(delivery.Family)
	if !IsMerkleFamily(family) {
		return nil, MerkleHash{}, fmt.Errorf("merkle delivery has unknown family %s", family)
	}
	if delivery.LeafCount > MaxMerkleBundleLeaves {
		return nil, MerkleHash{}, fmt.Errorf("merkle delivery has %d leaves, max %d", delivery.LeafCount, MaxMerkleBundleLeaves)
	}
	if err := verifyMerkleLeaf(family, delivery.Leaf); err != nil {
		return nil, MerkleHash{}, fmt.Errorf("merkle delivery: %w", err)
	}
	root, err := MerkleRootFromProof(MerkleLeafHash(family, delivery.Leaf), delivery.LeafIndex, delivery.LeafCount, delivery.Proof)
	if err != nil {
		return nil, MerkleHash{}, fmt.Errorf("merkle delivery: %w", err)
	}
	return delivery, root, nil
}

// SplitMerkleBundle encodes leaves, in order, into as few consecutive bundles
// of family as keep each encoded bundle within maxBytes. A leaf that does not
// fit in a bundle on its own is left out and its index returned in oversized.
func SplitMerkleBundle(family llotypes.ReportFormat, leaves []*LLOMerkleLeafProto, maxBytes int) (bundles [][]byte, oversized []int, err error) {
	familyField := 0
	if family != 0 {
		familyField = protowire.SizeTag(1) + protowire.SizeVarint(uint64(family))
	}
	var current []*LLOMerkleLeafProto
	size := familyField
	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		b, err := EncodeMerkleBundle(&LLOMerkleBundleProto{Family: uint32(family), Leaves: current})
		if err != nil {
			return err
		}
		bundles = append(bundles, b)
		current = nil
		size = familyField
		return nil
	}
	for i, leaf := range leaves {
		leafSize := protowire.SizeTag(2) + protowire.SizeBytes(proto.Size(leaf))
		if familyField+leafSize > maxBytes {
			oversized = append(oversized, i)
			continue
		}
		if size+leafSize > maxBytes || len(current) == MaxMerkleBundleLeaves {
			if err := flush(); err != nil {
				return nil, nil, err
			}
		}
		current = append(current, leaf)
		size += leafSize
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	return bundles, oversized, nil
}
