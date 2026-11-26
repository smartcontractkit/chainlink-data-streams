package keyring

import (
	crand "crypto/rand"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/keystore/corekeys"
	"github.com/smartcontractkit/chainlink-common/keystore/corekeys/ocr2key"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

var _ Key = &mockKey{}

type mockKey struct {
	format          llotypes.ReportFormat
	verify          bool
	maxSignatureLen int
	sig             []byte
}

func (m *mockKey) Sign(reportCtx types.ReportContext, report types.Report) ([]byte, error) {
	return m.sig, nil
}

func (m *mockKey) Verify(publicKey types.OnchainPublicKey, reportCtx types.ReportContext, report types.Report, signature []byte) bool {
	return m.verify
}

func (m *mockKey) Sign3(digest types.ConfigDigest, seqNr uint64, r types.Report) (signature []byte, err error) {
	return m.sig, nil
}

func (m *mockKey) Verify3(publicKey types.OnchainPublicKey, cd types.ConfigDigest, seqNr uint64, r types.Report, signature []byte) bool {
	return m.verify
}
func (m *mockKey) SignBlob(b []byte) (sig []byte, err error) { return m.sig, nil }
func (m *mockKey) VerifyBlob(publicKey types.OnchainPublicKey, b, sig []byte) bool {
	return m.verify
}

func (m *mockKey) PublicKey() types.OnchainPublicKey {
	b := make([]byte, m.maxSignatureLen)
	for i := range m.maxSignatureLen {
		b[i] = byte(255)
	}
	return types.OnchainPublicKey(b)
}

func (m *mockKey) MaxSignatureLength() int {
	return m.maxSignatureLen
}

func Test_Keyring(t *testing.T) {
	t.Parallel()
	lggr := logger.Test(t)

	ks := map[llotypes.ReportFormat]Key{
		llotypes.ReportFormatEVMPremiumLegacy: &mockKey{format: llotypes.ReportFormatEVMPremiumLegacy, maxSignatureLen: 1, sig: []byte("sig-1")},
		llotypes.ReportFormatJSON:             &mockKey{format: llotypes.ReportFormatJSON, maxSignatureLen: 2, sig: []byte("sig-2")},
		llotypes.ReportFormatEVMStreamlined:   &mockKey{format: llotypes.ReportFormatEVMStreamlined, maxSignatureLen: 6, sig: []byte("sig-6")},
	}

	kr := NewOnchainKeyring(lggr, ks, 2)

	cases := []struct {
		format llotypes.ReportFormat
	}{
		{
			llotypes.ReportFormatEVMPremiumLegacy,
		},
		{
			llotypes.ReportFormatJSON,
		},
		{
			llotypes.ReportFormatEVMStreamlined,
		},
	}

	cd, err := types.BytesToConfigDigest(mustRandBytes(32))
	require.NoError(t, err)
	seqNr := rand.Uint64N(math.MaxUint32 << 8)
	t.Run("Sign+Verify", func(t *testing.T) {
		t.Parallel()
		for _, tc := range cases {
			t.Run(tc.format.String(), func(t *testing.T) {
				t.Parallel()
				ksLocal := map[llotypes.ReportFormat]Key{
					llotypes.ReportFormatEVMPremiumLegacy: &mockKey{format: llotypes.ReportFormatEVMPremiumLegacy, maxSignatureLen: 1, sig: []byte("sig-1")},
					llotypes.ReportFormatJSON:             &mockKey{format: llotypes.ReportFormatJSON, maxSignatureLen: 2, sig: []byte("sig-2")},
					llotypes.ReportFormatEVMStreamlined:   &mockKey{format: llotypes.ReportFormatEVMStreamlined, maxSignatureLen: 6, sig: []byte("sig-6")},
				}
				krLocal := NewOnchainKeyring(lggr, ksLocal, 2)
				k := ksLocal[tc.format]

				sig, err := krLocal.Sign(cd, seqNr, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Info: llotypes.ReportInfo{ReportFormat: tc.format}})
				require.NoError(t, err)

				assert.Equal(t, fmt.Appendf(nil, "sig-%d", tc.format), sig)

				assert.False(t, krLocal.Verify(nil, cd, seqNr, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Info: llotypes.ReportInfo{ReportFormat: tc.format}}, sig))

				k.(*mockKey).verify = true
			})
		}
	})

	t.Run("MaxSignatureLength", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 6+2+1, kr.MaxSignatureLength())
	})
	t.Run("PublicKey", func(t *testing.T) {
		t.Parallel()
		b := make([]byte, 6+2+1)
		for i := range b {
			b[i] = byte(255)
		}
		assert.Equal(t, types.OnchainPublicKey(b), kr.PublicKey())
	})
}

func Test_Keyring_EVMStreamlined(t *testing.T) {
	t.Parallel()
	kb := ocr2key.MustNewInsecure(crand.Reader, corekeys.EVM)
	kr := NewOnchainKeyring(logger.Test(t), map[llotypes.ReportFormat]Key{llotypes.ReportFormatEVMStreamlined: kb}, 2)

	cd, err := types.BytesToConfigDigest(mustRandBytes(32))
	require.NoError(t, err)
	report := ocr3types.ReportWithInfo[llotypes.ReportInfo]{
		Report: mustRandBytes(64),
		Info:   llotypes.ReportInfo{ReportFormat: llotypes.ReportFormatEVMStreamlined},
	}

	sig, err := kr.Sign(cd, 42, report)
	require.NoError(t, err)
	require.True(t, kr.Verify(kb.PublicKey(), cd, 42, report, sig))

	// An onchain verifier recovers the signer from keccak256(digest ‖ report).
	h := gethcrypto.Keccak256(append(cd[:], report.Report...))
	recovered, err := gethcrypto.SigToPub(h, sig)
	require.NoError(t, err)
	assert.Equal(t, common.BytesToAddress(kb.PublicKey()), gethcrypto.PubkeyToAddress(*recovered))

	report.Report = mustRandBytes(64)
	assert.False(t, kr.Verify(kb.PublicKey(), cd, 42, report, sig))
}

func mustRandBytes(n int) (b []byte) {
	b = make([]byte, n)
	_, err := crand.Read(b)
	if err != nil {
		panic(err)
	}
	return b
}

func Test_Keyring_EVMMerkle(t *testing.T) {
	t.Parallel()
	lggr := logger.Test(t)
	newKeyring := func() (OnchainKeyring, ocr2key.KeyBundle) {
		kb := ocr2key.MustNewInsecure(crand.Reader, corekeys.EVM)
		return NewOnchainKeyring(lggr, map[llotypes.ReportFormat]Key{llotypes.ReportFormatEVMMerkle: kb}, 2), kb
	}
	signer, signerKey := newKeyring()
	verifier, _ := newKeyring()

	bundle := &protocol.LLOMerkleBundleProto{
		Family: uint32(llotypes.ReportFormatEVMMerkle),
		Leaves: []*protocol.LLOMerkleLeafProto{
			{ChannelID: 1, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpacked), Report: []byte("one")},
			{ChannelID: 2, ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined), Report: []byte("two")},
			{ChannelID: 3, ReportFormat: uint32(llotypes.ReportFormatEVMPremiumLegacy), Report: []byte("three")},
		},
	}
	encoded, err := protocol.EncodeMerkleBundle(bundle)
	require.NoError(t, err)
	report := ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: encoded, Info: llotypes.ReportInfo{ReportFormat: llotypes.ReportFormatEVMMerkle}}
	cd, err := types.BytesToConfigDigest(mustRandBytes(32))
	require.NoError(t, err)
	seqNr := uint64(42)

	sig, err := signer.Sign(cd, seqNr, report)
	require.NoError(t, err)
	pub := signerKey.PublicKey()

	t.Run("verifies", func(t *testing.T) {
		require.True(t, verifier.Verify(pub, cd, seqNr, report, sig))
		// The second check is served from the root cache.
		require.True(t, verifier.Verify(pub, cd, seqNr, report, sig))
	})

	t.Run("signs the merkle message an onchain verifier recovers", func(t *testing.T) {
		root, err := protocol.MerkleBundleRoot(bundle)
		require.NoError(t, err)
		msg := protocol.MerkleSigningMessage(cd, seqNr, llotypes.ReportFormatEVMMerkle, uint32(len(bundle.Leaves)), root)
		recovered, err := gethcrypto.SigToPub(msg[:], sig)
		require.NoError(t, err)
		assert.Equal(t, common.BytesToAddress(pub), gethcrypto.PubkeyToAddress(*recovered))
	})

	t.Run("rejects another round or configuration", func(t *testing.T) {
		assert.False(t, verifier.Verify(pub, cd, seqNr+1, report, sig))
		otherCD := cd
		otherCD[0] ^= 1
		assert.False(t, verifier.Verify(pub, otherCD, seqNr, report, sig))
	})

	t.Run("rejects a bundle whose leaves differ from the signed ones", func(t *testing.T) {
		changed := proto.Clone(bundle).(*protocol.LLOMerkleBundleProto)
		changed.Leaves[1].Report = []byte("TWO")
		b, err := protocol.EncodeMerkleBundle(changed)
		require.NoError(t, err)
		assert.False(t, verifier.Verify(pub, cd, seqNr, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: b, Info: report.Info}, sig))

		dropped := proto.Clone(bundle).(*protocol.LLOMerkleBundleProto)
		dropped.Leaves = dropped.Leaves[:2]
		b, err = protocol.EncodeMerkleBundle(dropped)
		require.NoError(t, err)
		assert.False(t, verifier.Verify(pub, cd, seqNr, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: b, Info: report.Info}, sig))
	})

	t.Run("rejects an invalid bundle", func(t *testing.T) {
		invalid := ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: []byte{0xff}, Info: report.Info}
		_, err := signer.Sign(cd, seqNr, invalid)
		require.ErrorContains(t, err, "Sign failed; invalid merkle bundle")
		assert.False(t, verifier.Verify(pub, cd, seqNr, invalid, sig))
	})

	t.Run("rejects a bundle under another report format", func(t *testing.T) {
		other := ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: encoded, Info: llotypes.ReportInfo{ReportFormat: llotypes.ReportFormatEVMStreamlined}}
		_, err := signer.(*onchainKeyring).merkleSigningMessage(cd, seqNr, other)
		require.EqualError(t, err, "bundle family evm_merkle does not match report format evm_streamlined")
	})
}
