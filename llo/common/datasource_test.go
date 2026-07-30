package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

func Test_NewDSOpts(t *testing.T) {
	cd := ocrtypes.ConfigDigest{1, 2, 3}
	ts := time.Unix(1737936858, 0)
	opts := NewDSOpts(true, 42, cd, ts, LifeCycleStageProduction)

	assert.True(t, opts.VerboseLogging())
	assert.Equal(t, uint64(42), opts.SeqNr())
	assert.Equal(t, cd, opts.ConfigDigest())
	assert.Equal(t, ts, opts.ObservationTimestamp())
	assert.Equal(t, LifeCycleStageProduction, opts.LifeCycleStage())

	staging := NewDSOpts(false, 0, ocrtypes.ConfigDigest{}, time.Time{}, LifeCycleStageStaging)
	assert.False(t, staging.VerboseLogging())
	assert.Equal(t, llotypes.LifeCycleStage(LifeCycleStageStaging), staging.LifeCycleStage())
}
