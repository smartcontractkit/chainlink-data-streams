package datasource

import (
	"context"
	"time"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// DSOpts is the per-round data-source options passed to DataSource.Observe.
// It is shared across all LLO plugin versions (v30, v31, ...): it carries only
// the version-agnostic round context the data source and telemetry paths need.
//
// Lifecycle is exposed directly via LifeCycleStage() rather than by threading a
// previous outcome: v30 decodes it from the previous outcome and v31 reads it
// from the KeyValueState, but the data source only ever needs the resulting
// stage (to gate pipeline observations on the Production instance).
type DSOpts interface {
	VerboseLogging() bool
	SeqNr() uint64
	ConfigDigest() ocrtypes.ConfigDigest
	ObservationTimestamp() time.Time
	LifeCycleStage() llotypes.LifeCycleStage
}

type dsOpts struct {
	verboseLogging       bool
	seqNr                uint64
	configDigest         ocrtypes.ConfigDigest
	observationTimestamp time.Time
	lifeCycleStage       llotypes.LifeCycleStage
}

// NewDSOpts builds the shared DSOpts value passed to DataSource.Observe.
func NewDSOpts(
	verboseLogging bool,
	seqNr uint64,
	configDigest ocrtypes.ConfigDigest,
	observationTimestamp time.Time,
	lifeCycleStage llotypes.LifeCycleStage,
) DSOpts {
	return &dsOpts{
		verboseLogging:       verboseLogging,
		seqNr:                seqNr,
		configDigest:         configDigest,
		observationTimestamp: observationTimestamp,
		lifeCycleStage:       lifeCycleStage,
	}
}

func (o *dsOpts) VerboseLogging() bool                    { return o.verboseLogging }
func (o *dsOpts) SeqNr() uint64                           { return o.seqNr }
func (o *dsOpts) ConfigDigest() ocrtypes.ConfigDigest     { return o.configDigest }
func (o *dsOpts) ObservationTimestamp() time.Time         { return o.observationTimestamp }
func (o *dsOpts) LifeCycleStage() llotypes.LifeCycleStage { return o.lifeCycleStage }

// DataSource observes stream values. For each known streamID, Observe should
// set the observed value in the passed protocol.StreamValues. Unknown/failed streams
// should be left unset. Shared across all LLO plugin versions.
type DataSource interface {
	Observe(ctx context.Context, streamValues protocol.StreamValues, opts DSOpts) error
}
