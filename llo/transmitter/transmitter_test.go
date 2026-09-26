package transmitter

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ocr3types "github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

type stubSubTransmitter struct {
	logger.Logger
	called atomic.Int32
}

func (s *stubSubTransmitter) Start(context.Context) error { return nil }
func (s *stubSubTransmitter) Close() error                 { return nil }
func (s *stubSubTransmitter) Ready() error                 { return nil }
func (s *stubSubTransmitter) HealthReport() map[string]error {
	return map[string]error{s.Name(): nil}
}
func (s *stubSubTransmitter) FromAccount(context.Context) (ocr2types.Account, error) {
	return "", nil
}
func (s *stubSubTransmitter) Transmit(
	_ context.Context,
	_ ocr2types.ConfigDigest,
	_ uint64,
	_ ocr3types.ReportWithInfo[llotypes.ReportInfo],
	_ []ocr2types.AttributedOnchainSignature,
) error {
	s.called.Add(1)
	return nil
}

func TestTransmitter_SkipsSpecimen(t *testing.T) {
	lggr := logger.Test(t)

	stub := &stubSubTransmitter{Logger: lggr}
	tr := &transmitter{
		lggr:           lggr,
		subTransmitters: []Transmitter{stub},
		onTransmit:      &onTransmit{},
	}

	mkReport := func(stage llotypes.LifeCycleStage) ocr3types.ReportWithInfo[llotypes.ReportInfo] {
		return ocr3types.ReportWithInfo[llotypes.ReportInfo]{
			Report: []byte("report-bytes"),
			Info:   llotypes.ReportInfo{LifeCycleStage: stage},
		}
	}

	t.Run("specimen report not transmitted", func(t *testing.T) {
		stub.called.Store(0)
		err := tr.Transmit(context.Background(), ocr2types.ConfigDigest{}, 1,
			mkReport(protocol.LifeCycleStageStaging), nil)
		require.NoError(t, err)
		assert.Equal(t, int32(0), stub.called.Load(), "sub-transmitter must not be called for specimen reports")
	})

	t.Run("production report is transmitted", func(t *testing.T) {
		stub.called.Store(0)
		err := tr.Transmit(context.Background(), ocr2types.ConfigDigest{}, 2,
			mkReport(protocol.LifeCycleStageProduction), nil)
		require.NoError(t, err)
		assert.Equal(t, int32(1), stub.called.Load(), "sub-transmitter must be called for production reports")
	})
}
