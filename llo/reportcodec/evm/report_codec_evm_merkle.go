package evm

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// MerklePayloadTypes is the ABI of the payload an EVM Merkle report is
// verified with. It starts with the same five fields as PayloadTypes, so the
// verifier proxy routes it by reportContext[0], the config digest, and the fee
// manager reads the report from it unchanged. The Merkle fields follow.
//
// An EVMStreamlined leaf starts with its feed ID but carries no fee fields. A
// fee manager that decodes fees from the report, unlike NoOpFeeManager, must
// skip it.
//
// reportContext[1] holds MerkleReportContextTag in its first bytes and seqNr in
// its last 8. A legacy report context has 27 zero bytes there, so a verifier
// tells the two payloads apart by the tag. reportContext[2] is zero.
//
// The verifier rebuilds the leaf hash from channelId, reportFormat and report,
// hashes it up to the root with proof, and checks the signatures over
// protocol.MerkleSigningMessage(configDigest, seqNr, ReportFormatEVMMerkle,
// leafCount, root). A verifier without Merkle support recovers the wrong
// signers from such a payload and rejects it.
var MerklePayloadTypes = getMerklePayloadTypes()

// MerkleReportContextTag marks reportContext[1] of a Merkle report payload.
const MerkleReportContextTag = protocol.MerkleSigningDomain

func getMerklePayloadTypes() abi.Arguments {
	mustNewType := func(t string) abi.Type {
		result, err := abi.NewType(t, "", []abi.ArgumentMarshaling{})
		if err != nil {
			panic(fmt.Sprintf("Unexpected error during abi.NewType: %s", err))
		}
		return result
	}
	return append(slices.Clone(PayloadTypes), abi.Arguments([]abi.Argument{
		{Name: "leafCount", Type: mustNewType("uint32")},
		{Name: "leafIndex", Type: mustNewType("uint32")},
		{Name: "channelId", Type: mustNewType("uint32")},
		{Name: "reportFormat", Type: mustNewType("uint32")},
		{Name: "proof", Type: mustNewType("bytes32[]")},
	})...)
}

// MerkleReportContext returns the report context of a Merkle report payload.
func MerkleReportContext(digest ocr2types.ConfigDigest, seqNr uint64) (rc [3][32]byte) {
	rc[0] = [32]byte(digest)
	copy(rc[1][:], MerkleReportContextTag)
	binary.BigEndian.PutUint64(rc[1][24:], seqNr)
	return rc
}

// ReportCodecEVMMerkle packs ReportFormatEVMMerkle reports for EVM
// verifiers. It is a packer only: no channel uses the format, so there is
// nothing to encode.
type ReportCodecEVMMerkle struct{}

// Pack assembles a report, as expanded from an attested bundle, and the root
// signatures into the payload for verifying on chain.
func (ReportCodecEVMMerkle) Pack(digest ocr2types.ConfigDigest, seqNr uint64, encoded ocr2types.Report, sigs []ocr2types.AttributedOnchainSignature) ([]byte, error) {
	report, _, err := protocol.DecodeMerkleReport(encoded)
	if err != nil {
		return nil, err
	}
	if family := llotypes.ReportFormat(report.Family); family != llotypes.ReportFormatEVMMerkle {
		return nil, fmt.Errorf("expected a report of family %s, got %s", llotypes.ReportFormatEVMMerkle, family)
	}
	proof := make([][32]byte, len(report.Proof))
	for i, p := range report.Proof {
		proof[i] = [32]byte(p)
	}
	rs, ss, vs, err := splitSignatures(sigs)
	if err != nil {
		return nil, err
	}
	payload, err := MerklePayloadTypes.Pack(
		MerkleReportContext(digest, seqNr),
		report.Leaf.Report,
		rs,
		ss,
		vs,
		report.LeafCount,
		report.LeafIndex,
		report.Leaf.ChannelID,
		report.Leaf.ReportFormat,
		proof,
	)
	if err != nil {
		return nil, fmt.Errorf("abi.Pack failed; %w", err)
	}
	return payload, nil
}
