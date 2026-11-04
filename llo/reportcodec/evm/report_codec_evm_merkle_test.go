package evm

import (
	"crypto/ecdsa"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/libocr/commontypes"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Test_ReportCodecEVMMerkle_Pack checks a packed delivery the way an onchain
// verifier would: rebuild the leaf from the payload, hash it up to the root
// with the proof, and recover the signers from the signed message.
func Test_ReportCodecEVMMerkle_Pack(t *testing.T) {
	digest := ocr2types.ConfigDigest{0x00, 0x09, 0xaa}
	seqNr := uint64(42)
	bundle := &protocol.LLOMerkleBundleProto{
		Family: uint32(llotypes.ReportFormatEVMMerkle),
		Leaves: []*protocol.LLOMerkleLeafProto{
			{ChannelID: 1, ReportFormat: uint32(llotypes.ReportFormatEVMPremiumLegacy), Report: []byte{0x01}},
			{ChannelID: 2, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpacked), Report: []byte{0x02, 0x02}},
			{ChannelID: 7, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpackedExpr), Report: []byte{0x07}},
			{ChannelID: 9, ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined), Report: []byte{}},
			{ChannelID: 10, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpacked), Report: []byte("ten")},
		},
	}
	root, err := protocol.MerkleBundleRoot(bundle)
	require.NoError(t, err)
	msg := protocol.MerkleSigningMessage(digest, seqNr, llotypes.ReportFormatEVMMerkle, uint32(len(bundle.Leaves)), root)

	keys := make([]*ecdsa.PrivateKey, 2)
	sigs := make([]ocr2types.AttributedOnchainSignature, len(keys))
	for i := range keys {
		keys[i], err = crypto.GenerateKey()
		require.NoError(t, err)
		sigs[i].Signature, err = crypto.Sign(msg[:], keys[i])
		require.NoError(t, err)
		sigs[i].Signer = commontypes.OracleID(i) //nolint:gosec // test
	}

	deliveries, err := protocol.ExpandMerkleBundle(bundle)
	require.NoError(t, err)
	for i, d := range deliveries {
		report, err := protocol.EncodeMerkleDelivery(d)
		require.NoError(t, err)
		payload, err := ReportCodecEVMMerkle{}.Pack(digest, seqNr, report, sigs)
		require.NoError(t, err)

		values, err := MerklePayloadTypes.Unpack(payload)
		require.NoError(t, err)
		require.Len(t, values, 10)
		reportCtx := values[0].([3][32]byte)
		leaf := &protocol.LLOMerkleLeafProto{ChannelID: values[7].(uint32), ReportFormat: values[8].(uint32), Report: values[1].([]byte)}
		rs := values[2].([][32]byte)
		ss := values[3].([][32]byte)
		vs := values[4].([32]byte)
		leafCount := values[5].(uint32)
		leafIndex := values[6].(uint32)
		proof := values[9].([][32]byte)

		gotDigest := reportCtx[0]
		require.Equal(t, []byte(MerkleReportContextTag), reportCtx[1][:len(MerkleReportContextTag)])
		gotSeqNr := binary.BigEndian.Uint64(reportCtx[1][24:])
		assert.Equal(t, [32]byte{}, reportCtx[2])

		assert.Equal(t, [32]byte(digest), gotDigest)
		assert.Equal(t, seqNr, gotSeqNr)
		assert.Equal(t, uint32(i), leafIndex) //nolint:gosec // test
		assert.Equal(t, bundle.Leaves[i].Report, leaf.Report)

		proofBytes := make([][]byte, len(proof))
		for j := range proof {
			proofBytes[j] = proof[j][:]
		}
		gotRoot, err := protocol.MerkleRootFromProof(protocol.MerkleLeafHash(llotypes.ReportFormatEVMMerkle, leaf), leafIndex, leafCount, proofBytes)
		require.NoError(t, err)
		require.Equal(t, root, gotRoot)

		gotMsg := protocol.MerkleSigningMessage(ocr2types.ConfigDigest(gotDigest), gotSeqNr, llotypes.ReportFormatEVMMerkle, leafCount, gotRoot)
		require.Len(t, rs, len(keys))
		for j := range rs {
			sig := append(append(append([]byte{}, rs[j][:]...), ss[j][:]...), vs[j])
			pub, err := crypto.SigToPub(gotMsg[:], sig)
			require.NoError(t, err)
			assert.Equal(t, crypto.PubkeyToAddress(keys[j].PublicKey), crypto.PubkeyToAddress(*pub))
		}
	}

	// The fee manager and verifier proxy read a Merkle payload as a legacy
	// one, and a verifier without Merkle support rejects it.
	t.Run("decodes as a legacy payload and fails legacy verification", func(t *testing.T) {
		report, err := protocol.EncodeMerkleDelivery(deliveries[1])
		require.NoError(t, err)
		payload, err := ReportCodecEVMMerkle{}.Pack(digest, seqNr, report, sigs)
		require.NoError(t, err)

		values, err := PayloadTypes.Unpack(payload)
		require.NoError(t, err)
		reportCtx := values[0].([3][32]byte)
		reportData := values[1].([]byte)
		assert.Equal(t, [32]byte(digest), [32]byte(payload[:32]))
		assert.Equal(t, bundle.Leaves[1].Report, reportData)

		// A legacy report context has 27 zero bytes before epoch and round.
		assert.NotEqual(t, make([]byte, 27), reportCtx[1][:27])

		rs, ss, vs := values[2].([][32]byte), values[3].([][32]byte), values[4].([32]byte)
		legacyHash := crypto.Keccak256(crypto.Keccak256(reportData), reportCtx[0][:], reportCtx[1][:], reportCtx[2][:])
		for j := range rs {
			sig := append(append(append([]byte{}, rs[j][:]...), ss[j][:]...), vs[j])
			pub, err := crypto.SigToPub(legacyHash, sig)
			require.NoError(t, err)
			assert.NotEqual(t, crypto.PubkeyToAddress(keys[j].PublicKey), crypto.PubkeyToAddress(*pub))
		}
	})

	t.Run("rejects an invalid delivery", func(t *testing.T) {
		_, err := ReportCodecEVMMerkle{}.Pack(digest, seqNr, []byte{0xff}, sigs)
		require.Error(t, err)
	})
}
