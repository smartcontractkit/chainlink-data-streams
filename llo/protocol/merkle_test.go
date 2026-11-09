package protocol

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

const evmMerkle = llotypes.ReportFormatEVMMerkle

func testLeaves(n int) []*LLOMerkleLeafProto {
	formats := []llotypes.ReportFormat{
		llotypes.ReportFormatEVMPremiumLegacy,
		llotypes.ReportFormatEVMABIEncodeUnpacked,
		llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		llotypes.ReportFormatEVMStreamlined,
	}
	leaves := make([]*LLOMerkleLeafProto, n)
	for i := range leaves {
		leaves[i] = &LLOMerkleLeafProto{
			ChannelID:    uint32(i + 1), //nolint:gosec // test
			ReportFormat: uint32(formats[i%len(formats)]),
			Report:       []byte(fmt.Sprintf("report %d", i)),
		}
	}
	return leaves
}

func leafHashes(leaves []*LLOMerkleLeafProto) []MerkleHash {
	hashes := make([]MerkleHash, len(leaves))
	for i, leaf := range leaves {
		hashes[i] = MerkleLeafHash(evmMerkle, leaf)
	}
	return hashes
}

func Test_MerkleFamily(t *testing.T) {
	for _, rf := range []llotypes.ReportFormat{
		llotypes.ReportFormatEVMPremiumLegacy,
		llotypes.ReportFormatEVMABIEncodeUnpacked,
		llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
		llotypes.ReportFormatEVMStreamlined,
	} {
		family, ok := MerkleFamily(rf)
		assert.True(t, ok, rf.String())
		assert.Equal(t, evmMerkle, family, rf.String())
	}
	for _, rf := range []llotypes.ReportFormat{
		llotypes.ReportFormatJSON,
		llotypes.ReportFormatRetirement,
		llotypes.ReportFormatCapabilityTrigger,
		llotypes.ReportFormatHistoryBackfill,
		llotypes.ReportFormatEVMMerkle,
	} {
		_, ok := MerkleFamily(rf)
		assert.False(t, ok, rf.String())
	}
}

func Test_VerifySigningScheme(t *testing.T) {
	require.NoError(t, VerifySigningScheme(llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON}))
	require.NoError(t, VerifySigningScheme(llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatEVMStreamlined, SigningScheme: llotypes.SigningSchemeMerkle}))
	require.EqualError(t, VerifySigningScheme(llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatJSON, SigningScheme: llotypes.SigningSchemeMerkle}),
		`signing scheme "merkle" is not supported for report format json`)
	require.EqualError(t, VerifySigningScheme(llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatEVMMerkle}),
		`report format evm_merkle is a signing family, not a channel format`)
	require.EqualError(t, VerifySigningScheme(llotypes.ChannelDefinition{ReportFormat: llotypes.ReportFormatEVMStreamlined, SigningScheme: "both"}),
		`unknown signing scheme "both"`)
}

func Test_MerkleRoot_Shape(t *testing.T) {
	l := leafHashes(testLeaves(5))
	node := merkleNodeHash

	cases := []struct {
		n    int
		want MerkleHash
	}{
		{1, l[0]},
		{2, node(l[0], l[1])},
		// The odd leaf is promoted, not paired with a copy of itself.
		{3, node(node(l[0], l[1]), l[2])},
		{4, node(node(l[0], l[1]), node(l[2], l[3]))},
		{5, node(node(node(l[0], l[1]), node(l[2], l[3])), l[4])},
	}
	for _, c := range cases {
		root, err := MerkleRoot(l[:c.n])
		require.NoError(t, err)
		assert.Equal(t, c.want, root, "n=%d", c.n)
	}

	_, err := MerkleRoot(nil)
	require.Error(t, err)
}

func Test_MerkleLeafHash_DomainSeparation(t *testing.T) {
	leaf := testLeaves(1)[0]
	h := MerkleLeafHash(evmMerkle, leaf)

	other := proto.Clone(leaf).(*LLOMerkleLeafProto)
	other.ChannelID++
	assert.NotEqual(t, h, MerkleLeafHash(evmMerkle, other), "channel ID")

	other = proto.Clone(leaf).(*LLOMerkleLeafProto)
	other.ReportFormat = uint32(llotypes.ReportFormatEVMStreamlined)
	assert.NotEqual(t, h, MerkleLeafHash(evmMerkle, other), "report format")

	assert.NotEqual(t, h, MerkleLeafHash(evmMerkle+1, leaf), "family")

	// A leaf hash never equals a node hash over the same bytes.
	assert.NotEqual(t, keccak([]byte{merkleNodePrefix}, h[:], h[:]), MerkleLeafHash(evmMerkle, leaf))
}

func Test_MerkleProof_EveryLeaf(t *testing.T) {
	for n := 1; n <= 33; n++ {
		hashes := leafHashes(testLeaves(n))
		tree, err := newMerkleTree(hashes)
		require.NoError(t, err)
		root := tree.root()
		count := uint32(n) //nolint:gosec // test

		for i := range n {
			index := uint32(i) //nolint:gosec // test
			proof := tree.proof(i)

			got, err := MerkleRootFromProof(hashes[i], index, count, proof)
			require.NoError(t, err, "n=%d i=%d", n, i)
			require.Equal(t, root, got, "n=%d i=%d", n, i)

			// A different leaf does not reach the root.
			other := hashes[(i+1)%n]
			if other != hashes[i] {
				got, err = MerkleRootFromProof(other, index, count, proof)
				if err == nil {
					assert.NotEqual(t, root, got, "changed leaf n=%d i=%d", n, i)
				}
			}
			// Neither does the same leaf at another index or in a tree of
			// another size.
			if n > 1 {
				got, err = MerkleRootFromProof(hashes[i], (index+1)%count, count, proof)
				if err == nil {
					assert.NotEqual(t, root, got, "changed index n=%d i=%d", n, i)
				}
			}
			// A proof can reach the same root under another leaf count, which
			// is why the count is part of the signed message.
			got, err = MerkleRootFromProof(hashes[i], index, count+1, proof)
			if err == nil {
				signed := MerkleSigningMessage(ocr2types.ConfigDigest{}, 1, evmMerkle, count, root)
				assert.NotEqual(t, signed, MerkleSigningMessage(ocr2types.ConfigDigest{}, 1, evmMerkle, count+1, got), "changed leaf count n=%d i=%d", n, i)
			}
			// A padded or truncated proof is rejected.
			_, err = MerkleRootFromProof(hashes[i], index, count, append(proof, root[:]))
			require.Error(t, err)
			if len(proof) > 0 {
				_, err = MerkleRootFromProof(hashes[i], index, count, proof[:len(proof)-1])
				require.Error(t, err)
			}
		}
	}
}

func Test_MerkleRootFromProof_Errors(t *testing.T) {
	leaf := MerkleHash{1}
	_, err := MerkleRootFromProof(leaf, 0, 0, nil)
	require.Error(t, err)
	_, err = MerkleRootFromProof(leaf, 2, 2, nil)
	require.Error(t, err)
	_, err = MerkleRootFromProof(leaf, 0, 2, [][]byte{{1, 2, 3}})
	require.EqualError(t, err, "merkle proof hash has 3 bytes, expected 32")
}

// The golden vectors are the shared reference for the verifier contract and
// the Data Streams server. Changing them breaks every delivery verifier. They
// were checked against an independent keccak256 implementation of the scheme
// described in merkle.go.
func Test_Merkle_GoldenVectors(t *testing.T) {
	bundle := &LLOMerkleBundleProto{
		Family: uint32(evmMerkle),
		Leaves: []*LLOMerkleLeafProto{
			{ChannelID: 1, ReportFormat: uint32(llotypes.ReportFormatEVMPremiumLegacy), Report: []byte{0x01}},
			{ChannelID: 2, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpacked), Report: []byte{0x02, 0x02}},
			{ChannelID: 7, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpackedExpr), Report: []byte{0x07}},
			{ChannelID: 9, ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined), Report: []byte{}},
			{ChannelID: 10, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpacked), Report: []byte("ten")},
		},
	}
	digest := ocr2types.ConfigDigest{0x00, 0x09, 0xaa}
	seqNr := uint64(42)

	leaf0 := MerkleLeafHash(evmMerkle, bundle.Leaves[0])
	assert.Equal(t, "c77f76bdb77a1e6a7145ea23d2f9309047f3020187de3013c01cee82bff6f057", hex.EncodeToString(leaf0[:]))

	root, err := MerkleBundleRoot(bundle)
	require.NoError(t, err)
	assert.Equal(t, "cee789eea2b2295270c2507936405a5dc9e3af1fcfc3c8c9cb605bae8e57b4e9", hex.EncodeToString(root[:]))

	msg := MerkleSigningMessage(digest, seqNr, evmMerkle, uint32(len(bundle.Leaves)), root)
	assert.Equal(t, "82a749ca4b50caa0ea66a307bb3cffbc933bca6be71832179c775be8dee4ebac", hex.EncodeToString(msg[:]))

	deliveries, err := ExpandMerkleBundle(bundle)
	require.NoError(t, err)
	proofs := make([][]string, len(deliveries))
	for i, d := range deliveries {
		for _, p := range d.Proof {
			proofs[i] = append(proofs[i], hex.EncodeToString(p))
		}
	}
	assert.Equal(t, [][]string{
		{"a9ca2f12f60482dba94c617c2c752d6f1770af1e796a3fe319e3a69bba71fc5a", "ba2504762e114a693f259e800f8e66387fb59e0a217d6caa40e6f5e54703ada2", "cb05384430fa7e800cea5134251e599c40b4f2b52a46a5bccc70a21f69483a0b"},
		{"c77f76bdb77a1e6a7145ea23d2f9309047f3020187de3013c01cee82bff6f057", "ba2504762e114a693f259e800f8e66387fb59e0a217d6caa40e6f5e54703ada2", "cb05384430fa7e800cea5134251e599c40b4f2b52a46a5bccc70a21f69483a0b"},
		{"ae0b62b9ce280ffba7c5c0c1cad41c77928a9a408988e550d857e6ffe04dad22", "b31bd790ac0b0f71e3db167a3d275efab380d4fbcaca3ec7646958b8f6e0bf19", "cb05384430fa7e800cea5134251e599c40b4f2b52a46a5bccc70a21f69483a0b"},
		{"4d1c23853ce24ad78df071013d6300555c3d36fa656a046b94c750b74661660a", "b31bd790ac0b0f71e3db167a3d275efab380d4fbcaca3ec7646958b8f6e0bf19", "cb05384430fa7e800cea5134251e599c40b4f2b52a46a5bccc70a21f69483a0b"},
		// The last leaf is promoted twice, so its only sibling is the root of
		// the first four.
		{"7e4ba02ae94d30d32ef66b329b325986ebadd188a13178fb2c0b9159268b9369"},
	}, proofs)
}

func Test_MerkleBundle_Codec(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		bundle := &LLOMerkleBundleProto{Family: uint32(evmMerkle), Leaves: testLeaves(4)}
		b, err := EncodeMerkleBundle(bundle)
		require.NoError(t, err)
		decoded, err := DecodeMerkleBundle(b)
		require.NoError(t, err)
		assert.True(t, proto.Equal(bundle, decoded))
	})

	encode := func(bundle *LLOMerkleBundleProto) []byte {
		b, err := EncodeMerkleBundle(bundle)
		require.NoError(t, err)
		return b
	}
	cases := []struct {
		name   string
		bundle *LLOMerkleBundleProto
		err    string
	}{
		{"unknown family", &LLOMerkleBundleProto{Family: uint32(llotypes.ReportFormatJSON), Leaves: testLeaves(1)}, "merkle bundle has unknown family json"},
		{"no leaves", &LLOMerkleBundleProto{Family: uint32(evmMerkle)}, fmt.Sprintf("merkle bundle has 0 leaves, expected between 1 and %d", MaxMerkleBundleLeaves)},
		{"non member leaf", &LLOMerkleBundleProto{Family: uint32(evmMerkle), Leaves: []*LLOMerkleLeafProto{{ChannelID: 1, ReportFormat: uint32(llotypes.ReportFormatJSON)}}}, "merkle bundle leaf 0: report format json is not a member of family evm_merkle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeMerkleBundle(encode(c.bundle))
			require.EqualError(t, err, c.err)
		})
	}

	t.Run("too many leaves", func(t *testing.T) {
		_, err := DecodeMerkleBundle(encode(&LLOMerkleBundleProto{Family: uint32(evmMerkle), Leaves: testLeaves(MaxMerkleBundleLeaves + 1)}))
		require.EqualError(t, err, fmt.Sprintf("merkle bundle has %d leaves, expected between 1 and %d", MaxMerkleBundleLeaves+1, MaxMerkleBundleLeaves))
	})

	t.Run("garbage", func(t *testing.T) {
		_, err := DecodeMerkleBundle([]byte{0xff, 0xff})
		require.Error(t, err)
	})
}

func Test_MerkleDelivery(t *testing.T) {
	bundle := &LLOMerkleBundleProto{Family: uint32(evmMerkle), Leaves: testLeaves(7)}
	root, err := MerkleBundleRoot(bundle)
	require.NoError(t, err)

	deliveries, err := ExpandMerkleBundle(bundle)
	require.NoError(t, err)
	require.Len(t, deliveries, 7)

	for i, d := range deliveries {
		assert.Equal(t, uint32(i), d.LeafIndex) //nolint:gosec // test
		assert.Equal(t, uint32(7), d.LeafCount)
		assert.True(t, proto.Equal(bundle.Leaves[i], d.Leaf))

		b, err := EncodeMerkleDelivery(d)
		require.NoError(t, err)
		decoded, got, err := DecodeMerkleDelivery(b)
		require.NoError(t, err)
		assert.Equal(t, root, got)
		assert.True(t, proto.Equal(d, decoded))
	}

	tamper := func(f func(d *LLOMerkleDeliveryProto)) (MerkleHash, error) {
		d := proto.Clone(deliveries[3]).(*LLOMerkleDeliveryProto)
		f(d)
		b, err := EncodeMerkleDelivery(d)
		require.NoError(t, err)
		_, got, err := DecodeMerkleDelivery(b)
		return got, err
	}
	for name, f := range map[string]func(d *LLOMerkleDeliveryProto){
		"report":  func(d *LLOMerkleDeliveryProto) { d.Leaf.Report = append(d.Leaf.Report, 0) },
		"channel": func(d *LLOMerkleDeliveryProto) { d.Leaf.ChannelID++ },
		"format":  func(d *LLOMerkleDeliveryProto) { d.Leaf.ReportFormat = uint32(llotypes.ReportFormatEVMPremiumLegacy) },
		"index":   func(d *LLOMerkleDeliveryProto) { d.LeafIndex = 2 },
		"proof":   func(d *LLOMerkleDeliveryProto) { d.Proof[0][0] ^= 1 },
		// A changed leaf count can still reach the root. The signed message
		// binds it, see Test_MerkleProof_EveryLeaf.
		"leaf count": func(d *LLOMerkleDeliveryProto) { d.LeafCount = 9 },
	} {
		got, err := tamper(f)
		if err == nil {
			assert.NotEqual(t, root, got, name)
		}
	}

	for name, c := range map[string]struct {
		f   func(d *LLOMerkleDeliveryProto)
		err string
	}{
		"family":      {func(d *LLOMerkleDeliveryProto) { d.Family = uint32(llotypes.ReportFormatJSON) }, "merkle delivery has unknown family json"},
		"non member":  {func(d *LLOMerkleDeliveryProto) { d.Leaf.ReportFormat = uint32(llotypes.ReportFormatJSON) }, "merkle delivery: report format json is not a member of family evm_merkle"},
		"no leaf":     {func(d *LLOMerkleDeliveryProto) { d.Leaf = nil }, "merkle delivery: nil leaf"},
		"index range": {func(d *LLOMerkleDeliveryProto) { d.LeafIndex = 7 }, "merkle delivery: leaf index 7 out of range for 7 leaves"},
	} {
		_, err := tamper(c.f)
		require.EqualError(t, err, c.err, name)
	}
}

func Test_SplitMerkleBundle(t *testing.T) {
	leaves := testLeaves(50)
	leafSize := len(mustEncodeBundle(t, &LLOMerkleBundleProto{Leaves: leaves[:1]}))
	familySize := len(mustEncodeBundle(t, &LLOMerkleBundleProto{Family: uint32(evmMerkle)}))

	t.Run("fits in one bundle", func(t *testing.T) {
		bundles, oversized, err := SplitMerkleBundle(evmMerkle, leaves, 1<<20)
		require.NoError(t, err)
		assert.Empty(t, oversized)
		require.Len(t, bundles, 1)
		assert.Equal(t, mustEncodeBundle(t, &LLOMerkleBundleProto{Family: uint32(evmMerkle), Leaves: leaves}), bundles[0])
	})

	t.Run("splits by size, in order", func(t *testing.T) {
		maxBytes := familySize + 7*leafSize
		bundles, oversized, err := SplitMerkleBundle(evmMerkle, leaves, maxBytes)
		require.NoError(t, err)
		assert.Empty(t, oversized)
		require.Greater(t, len(bundles), 1)

		var all []*LLOMerkleLeafProto
		for i, b := range bundles {
			assert.LessOrEqual(t, len(b), maxBytes)
			decoded, err := DecodeMerkleBundle(b)
			require.NoError(t, err)
			all = append(all, decoded.Leaves...)
			if i < len(bundles)-1 {
				next := leaves[len(all)]
				withNext := mustEncodeBundle(t, &LLOMerkleBundleProto{Family: uint32(evmMerkle), Leaves: append(decoded.Leaves, next)})
				assert.Greater(t, len(withNext), maxBytes, "bundle %d is not full", i)
			}
		}
		require.Len(t, all, len(leaves))
		for i := range leaves {
			assert.True(t, proto.Equal(leaves[i], all[i]))
		}
	})

	t.Run("oversized leaf is left out", func(t *testing.T) {
		big := &LLOMerkleLeafProto{ChannelID: 99, ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined), Report: make([]byte, 200)}
		input := []*LLOMerkleLeafProto{leaves[0], big, leaves[1]}
		bundles, oversized, err := SplitMerkleBundle(evmMerkle, input, 100)
		require.NoError(t, err)
		assert.Equal(t, []int{1}, oversized)
		require.Len(t, bundles, 1)
		decoded, err := DecodeMerkleBundle(bundles[0])
		require.NoError(t, err)
		require.Len(t, decoded.Leaves, 2)
	})

	t.Run("no leaves", func(t *testing.T) {
		bundles, oversized, err := SplitMerkleBundle(evmMerkle, nil, 100)
		require.NoError(t, err)
		assert.Empty(t, bundles)
		assert.Empty(t, oversized)
	})
}

func mustEncodeBundle(t *testing.T, bundle *LLOMerkleBundleProto) []byte {
	t.Helper()
	b, err := EncodeMerkleBundle(bundle)
	require.NoError(t, err)
	return b
}

func FuzzMerkleDelivery(f *testing.F) {
	f.Add(uint16(1), uint16(0), []byte("a"), uint16(0))
	f.Add(uint16(7), uint16(6), []byte("seven"), uint16(3))
	f.Add(uint16(64), uint16(31), []byte{}, uint16(100))
	f.Fuzz(func(t *testing.T, n, index uint16, report []byte, flip uint16) {
		count := int(n%200) + 1
		i := int(index) % count
		leaves := testLeaves(count)
		leaves[i].Report = report
		bundle := &LLOMerkleBundleProto{Family: uint32(evmMerkle), Leaves: leaves}

		root, err := MerkleBundleRoot(bundle)
		require.NoError(t, err)
		deliveries, err := ExpandMerkleBundle(bundle)
		require.NoError(t, err)
		b, err := EncodeMerkleDelivery(deliveries[i])
		require.NoError(t, err)
		_, got, err := DecodeMerkleDelivery(b)
		require.NoError(t, err)
		require.Equal(t, root, got)

		// Flipping any bit of the delivery either fails to decode or no
		// longer reaches the root. The one exception is the leaf count, which
		// the root does not bind but the signed message does.
		bit := int(flip) % (len(b) * 8)
		tampered := append([]byte(nil), b...)
		tampered[bit/8] ^= 1 << (bit % 8)
		d, got, err := DecodeMerkleDelivery(tampered)
		if err == nil && got == root && d.LeafCount == deliveries[i].LeafCount {
			require.True(t, proto.Equal(deliveries[i], d), "a changed delivery reached the root")
		}
	})
}
