package transmitter

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

type countingSubTransmitter struct {
	services.Service
	calls atomic.Int32
}

func (c *countingSubTransmitter) Transmit(context.Context, types.ConfigDigest, uint64, ocr3types.ReportWithInfo[llotypes.ReportInfo], []types.AttributedOnchainSignature) error {
	c.calls.Add(1)
	return nil
}

func (c *countingSubTransmitter) FromAccount(context.Context) (types.Account, error) {
	return "", nil
}

func Test_transmitter_StagingMarkerOnlyNotifies(t *testing.T) {
	ctx := tests.Context(t)
	sub := &countingSubTransmitter{}
	tr := &transmitter{
		lggr:            logger.Test(t),
		subTransmitters: []Transmitter{sub},
		onTransmit:      &onTransmit{lggr: logger.Test(t)},
	}
	type notification struct {
		digest types.ConfigDigest
		seqNr  uint64
	}
	notified := make(chan notification, 4)
	tr.OnTransmit(func(digest types.ConfigDigest, seqNr uint64) { notified <- notification{digest, seqNr} })

	jsonReport := func(stage llotypes.LifeCycleStage, payload []byte) ocr3types.ReportWithInfo[llotypes.ReportInfo] {
		return ocr3types.ReportWithInfo[llotypes.ReportInfo]{
			Report: payload,
			Info:   llotypes.ReportInfo{LifeCycleStage: stage, ReportFormat: llotypes.ReportFormatJSON},
		}
	}
	marker, err := protocol.EncodeStagingMarker(types.ConfigDigest{2}, 7)
	require.NoError(t, err)

	t.Run("staging marker notifies and is not transmitted", func(t *testing.T) {
		require.NoError(t, tr.Transmit(ctx, types.ConfigDigest{2}, 7, jsonReport(protocol.LifeCycleStageStaging, marker), nil))
		require.Equal(t, notification{types.ConfigDigest{2}, 7}, <-notified)
		require.Zero(t, sub.calls.Load())
	})

	t.Run("staging specimen notifies and is transmitted", func(t *testing.T) {
		require.NoError(t, tr.Transmit(ctx, types.ConfigDigest{2}, 8, jsonReport(protocol.LifeCycleStageStaging, []byte(`{"ConfigDigest":"02","Specimen":true}`)), nil))
		require.Equal(t, notification{types.ConfigDigest{2}, 8}, <-notified)
		require.Equal(t, int32(1), sub.calls.Load())
	})

	t.Run("production report notifies and is transmitted", func(t *testing.T) {
		require.NoError(t, tr.Transmit(ctx, types.ConfigDigest{1}, 9, jsonReport(protocol.LifeCycleStageProduction, []byte(`{}`)), nil))
		require.Equal(t, notification{types.ConfigDigest{1}, 9}, <-notified)
		require.Equal(t, int32(2), sub.calls.Load())
	})
}
