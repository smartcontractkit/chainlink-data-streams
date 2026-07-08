package common

import (
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"
)

// Protocol instances start in either the staging or production stage. They
// may later be retired and "hand over" their work to another protocol instance
// that will move from the staging to the production stage.
//
// These lifecycle constants and the retirement handover types are shared
// across all LLO plugin versions.
const (
	LifeCycleStageStaging    llotypes.LifeCycleStage = "staging"
	LifeCycleStageProduction llotypes.LifeCycleStage = "production"
	LifeCycleStageRetired    llotypes.LifeCycleStage = "retired"
)

type RetirementReport struct {
	// Retirement reports are not guaranteed to be compatible across different
	// protocol versions
	ProtocolVersion uint32
	// Carries validity time stamps between protocol instances to ensure there
	// are no gaps
	ValidAfterNanoseconds map[llotypes.ChannelID]uint64
}

// The predecessor protocol instance stores its attested retirement report in
// this cache (locally, offchain), so it can be fetched by the successor
// protocol instance.
//
// PredecessorRetirementReportCache is populated by the old protocol instance
// writing to it and the new protocol instance reading from it.
//
// The sketch envisions it being implemented as a single object that is shared
// between different protocol instances.
type PredecessorRetirementReportCache interface {
	// AttestedRetirementReport returns the attested retirement report for the
	// given config digest from the local cache.
	//
	// This should return nil and not error in the case of a missing attested
	// retirement report.
	AttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest) ([]byte, error)
	// CheckAttestedRetirementReport verifies that an attested retirement
	// report, which may have come from another node, is valid (signed) with
	// signers corresponding to the given config digest
	CheckAttestedRetirementReport(predecessorConfigDigest ocr2types.ConfigDigest, attestedRetirementReport []byte) (RetirementReport, error)
}
