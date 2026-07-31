package datasource

import (
	"testing"
	"time"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/stretchr/testify/assert"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

func Test_NewDSOpts(t *testing.T) {
	cd := ocrtypes.ConfigDigest{1, 2, 3}
	ts := time.Unix(1737936858, 0)
	opts := NewDSOpts(true, 42, cd, ts, protocol.LifeCycleStageProduction)

	assert.True(t, opts.VerboseLogging())
	assert.Equal(t, uint64(42), opts.SeqNr())
	assert.Equal(t, cd, opts.ConfigDigest())
	assert.Equal(t, ts, opts.ObservationTimestamp())
	assert.Equal(t, protocol.LifeCycleStageProduction, opts.LifeCycleStage())

	staging := NewDSOpts(false, 0, ocrtypes.ConfigDigest{}, time.Time{}, protocol.LifeCycleStageStaging)
	assert.False(t, staging.VerboseLogging())
	assert.Equal(t, llotypes.LifeCycleStage(protocol.LifeCycleStageStaging), staging.LifeCycleStage())
}
