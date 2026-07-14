package retirement

import (
	"context"

	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

type NullRetirementReportCache struct{}

func (n *NullRetirementReportCache) StoreAttestedRetirementReport(ctx context.Context, cd ocr2types.ConfigDigest, retirementReport []byte, sigs []types.AttributedOnchainSignature) error {
	return nil
}
func (n *NullRetirementReportCache) StoreConfig(ctx context.Context, cd ocr2types.ConfigDigest, signers [][]byte, f uint8) error {
	return nil
}
func (n *NullRetirementReportCache) AttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest) ([]byte, error) {
	return nil, nil
}
func (n *NullRetirementReportCache) CheckAttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest, attestedRetirementReport []byte) (llocommon.RetirementReport, error) {
	return llocommon.RetirementReport{}, nil
}
