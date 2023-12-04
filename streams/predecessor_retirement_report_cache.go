package llo

import (
	relayllo "github.com/smartcontractkit/chainlink-common/pkg/reportingplugins/llo"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
)

var _ relayllo.PredecessorRetirementReportCache = &predecessorRetirementReportCache{}

type predecessorRetirementReportCache struct{}

func NewPredecessorRetirementReportCache() relayllo.PredecessorRetirementReportCache {
	return newPredecessorRetirementReportCache()
}

func newPredecessorRetirementReportCache() *predecessorRetirementReportCache {
	return &predecessorRetirementReportCache{}
}

func (c *predecessorRetirementReportCache) AttestedRetirementReport(predecessorConfigDigest types.ConfigDigest) ([]byte, error) {
	panic("TODO")
}

func (c *predecessorRetirementReportCache) CheckAttestedRetirementReport(predecessorConfigDigest types.ConfigDigest, attestedRetirementReport []byte) (relayllo.RetirementReport, error) {
	panic("TODO")
}
