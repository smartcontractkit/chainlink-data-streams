package streams

import (
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
)

var _ PredecessorRetirementReportCache = &predecessorRetirementReportCache{}

type predecessorRetirementReportCache struct{}

func NewPredecessorRetirementReportCache() PredecessorRetirementReportCache {
	return newPredecessorRetirementReportCache()
}

func newPredecessorRetirementReportCache() *predecessorRetirementReportCache {
	return &predecessorRetirementReportCache{}
}

func (c *predecessorRetirementReportCache) AttestedRetirementReport(predecessorConfigDigest types.ConfigDigest) ([]byte, error) {
	panic("TODO")
}

func (c *predecessorRetirementReportCache) CheckAttestedRetirementReport(predecessorConfigDigest types.ConfigDigest, attestedRetirementReport []byte) (RetirementReport, error) {
	panic("TODO")
}
