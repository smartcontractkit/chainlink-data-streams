package llo

import (
	"context"
	"time"

	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// DSOpts is the v31 data-source options passed to DataSource.Observe. Unlike
// v30's DSOpts it carries no OutcomeContext or OutcomeCodec (v31 has neither):
// state lives in the KeyValueState, not a threaded outcome.
type DSOpts interface {
	VerboseLogging() bool
	SeqNr() uint64
	ConfigDigest() ocrtypes.ConfigDigest
	ObservationTimestamp() time.Time
}

type dsOpts struct {
	verboseLogging       bool
	seqNr                uint64
	configDigest         ocrtypes.ConfigDigest
	observationTimestamp time.Time
}

func (o *dsOpts) VerboseLogging() bool                { return o.verboseLogging }
func (o *dsOpts) SeqNr() uint64                       { return o.seqNr }
func (o *dsOpts) ConfigDigest() ocrtypes.ConfigDigest { return o.configDigest }
func (o *dsOpts) ObservationTimestamp() time.Time     { return o.observationTimestamp }

// DataSource observes stream values. For each known streamID, Observe should
// set the observed value in the passed StreamValues. Unknown/failed streams
// should be left unset.
type DataSource interface {
	Observe(ctx context.Context, streamValues llocommon.StreamValues, opts DSOpts) error
}

// ShouldRetireCache reads asynchronously from the onchain ConfigurationStore
// whether this protocol instance should retire.
type ShouldRetireCache interface {
	ShouldRetire(digest ocrtypes.ConfigDigest) (bool, error)
}
