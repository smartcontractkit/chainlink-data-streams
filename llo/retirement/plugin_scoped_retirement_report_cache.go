package retirement

import (
	"fmt"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
	retirement "github.com/smartcontractkit/chainlink-data-streams/llo/reportcodecs/retirement"
)

type RetirementReportVerifier interface {
	Verify(key types.OnchainPublicKey, digest types.ConfigDigest, seqNr uint64, r ocr3types.ReportWithInfo[llotypes.ReportInfo], signature []byte) bool
}

// PluginScopedRetirementReportCache is a wrapper around RetirementReportCache
// that implements CheckAttestedRetirementReport
//
// This is necessary because while config digest keys are globally unique,
// different plugins may implement different signing/verification strategies
var _ llocommon.PredecessorRetirementReportCache = &pluginScopedRetirementReportCache{}

type pluginScopedRetirementReportCache struct {
	rrc      RetirementReportCacheReader
	verifier RetirementReportVerifier
	codec    llocommon.RetirementReportCodec
}

func NewPluginScopedRetirementReportCache(rrc RetirementReportCacheReader, verifier RetirementReportVerifier, codec llocommon.RetirementReportCodec) llocommon.PredecessorRetirementReportCache {
	return &pluginScopedRetirementReportCache{
		rrc:      rrc,
		verifier: verifier,
		codec:    codec,
	}
}

func (pr *pluginScopedRetirementReportCache) CheckAttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest, serializedAttestedRetirementReport []byte) (llocommon.RetirementReport, error) {
	config, exists := pr.rrc.Config(predecessorConfigDigest)
	if !exists {
		return llocommon.RetirementReport{}, fmt.Errorf("Verify failed; predecessor config not found for config digest %x", predecessorConfigDigest[:])
	}

	var arr retirement.AttestedRetirementReport
	if err := proto.Unmarshal(serializedAttestedRetirementReport, &arr); err != nil {
		return llocommon.RetirementReport{}, fmt.Errorf("Verify failed; failed to unmarshal protobuf: %w", err)
	}

	validSigs := 0
	seenSigners := make(map[uint32]struct{}, len(config.Signers))
	for _, sig := range arr.Sigs {
		// #nosec G115
		if sig.Signer >= uint32(len(config.Signers)) {
			return llocommon.RetirementReport{}, fmt.Errorf("Verify failed; attested report signer index out of bounds (got: %d, max: %d)", sig.Signer, len(config.Signers)-1)
		}

		// ensure we have unique signatures
		if _, seen := seenSigners[sig.Signer]; seen {
			return llocommon.RetirementReport{}, fmt.Errorf("Verify failed; duplicate signature from signer index %d", sig.Signer)
		}

		seenSigners[sig.Signer] = struct{}{}
		signer := config.Signers[sig.Signer]
		valid := pr.verifier.Verify(types.OnchainPublicKey(signer), predecessorConfigDigest, arr.SeqNr, ocr3types.ReportWithInfo[llotypes.ReportInfo]{
			Report: arr.RetirementReport,
			Info:   llotypes.ReportInfo{ReportFormat: llotypes.ReportFormatRetirement},
		}, sig.Signature)
		if !valid {
			continue
		}
		validSigs++
	}
	if validSigs <= int(config.F) {
		return llocommon.RetirementReport{}, fmt.Errorf("Verify failed; not enough valid signatures (got: %d, need: %d)", validSigs, config.F+1)
	}
	decoded, err := pr.codec.Decode(arr.RetirementReport)
	if err != nil {
		return llocommon.RetirementReport{}, fmt.Errorf("Verify failed; failed to decode retirement report: %w", err)
	}
	return decoded, nil
}

func (pr *pluginScopedRetirementReportCache) AttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest) ([]byte, error) {
	arr, exists := pr.rrc.AttestedRetirementReport(predecessorConfigDigest)
	if !exists {
		return nil, nil
	}
	return arr, nil
}
