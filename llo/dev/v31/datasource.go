package llo

import (
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-data-streams/llo/datasource"
)

// DSOpts and DataSource are the shared, version-agnostic data-source types.
// Kept as aliases here so existing llov31.DSOpts / llov31.DataSource references
// keep working. Lifecycle is carried directly via DSOpts.LifeCycleStage()
// (read from the KeyValueState at the Observe call site).
type DSOpts = datasource.DSOpts
type DataSource = datasource.DataSource

// ShouldRetireCache reads asynchronously from the onchain ConfigurationStore
// whether this protocol instance should retire.
type ShouldRetireCache interface {
	ShouldRetire(digest ocrtypes.ConfigDigest) (bool, error)
}
