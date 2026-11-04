package dataengine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec/evm"
	"github.com/smartcontractkit/chainlink-data-streams/rpc"
)

func Test_server_transmit_EVMMerkle(t *testing.T) {
	var got *rpc.TransmitRequest
	s := &server{
		evmMerklePacker: evm.ReportCodecEVMMerkle{},
		c: &MockGRPCClient{TransmitF: func(_ context.Context, in *rpc.TransmitRequest) (*rpc.TransmitResponse, error) {
			got = in
			return &rpc.TransmitResponse{}, nil
		}},
	}

	bundle := &protocol.LLOMerkleBundleProto{
		Family: uint32(llotypes.ReportFormatEVMMerkle),
		Leaves: []*protocol.LLOMerkleLeafProto{
			{ChannelID: 1, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpacked), Report: []byte("one")},
			{ChannelID: 2, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpackedExpr), Report: []byte("two")},
		},
	}
	deliveries, err := protocol.ExpandMerkleBundle(bundle)
	require.NoError(t, err)
	delivery, err := protocol.EncodeMerkleDelivery(deliveries[1])
	require.NoError(t, err)

	transmission := &Transmission{
		ConfigDigest: types.ConfigDigest{1},
		SeqNr:        7,
		Report: ocr3types.ReportWithInfo[llotypes.ReportInfo]{
			Report: delivery,
			Info:   llotypes.ReportInfo{LifeCycleStage: protocol.LifeCycleStageProduction, ReportFormat: llotypes.ReportFormatEVMMerkle},
		},
		Sigs: []types.AttributedOnchainSignature{{Signature: make([]byte, 65), Signer: 1}},
	}
	_, _, err = s.transmit(t.Context(), transmission)
	require.NoError(t, err)

	require.NotNil(t, got)
	assert.Equal(t, uint32(llotypes.ReportFormatEVMMerkle), got.ReportFormat)
	want, err := evm.ReportCodecEVMMerkle{}.Pack(transmission.ConfigDigest, transmission.SeqNr, delivery, transmission.Sigs)
	require.NoError(t, err)
	assert.Equal(t, want, got.Payload)
}

func Test_transmitter_TransmitBatch(t *testing.T) {
	mt := &transmitter{
		lggr:     logger.Sugared(logger.Test(t)),
		servers:  map[string]*server{"a": {}, "b": {}},
		commitCh: make(chan []*Transmission, 1),
	}

	reports := make([]ocr3types.ReportWithInfo[llotypes.ReportInfo], 3)
	for i := range reports {
		reports[i] = ocr3types.ReportWithInfo[llotypes.ReportInfo]{
			Report: []byte{byte(i)},
			Info:   llotypes.ReportInfo{ReportFormat: llotypes.ReportFormatEVMMerkle},
		}
	}

	require.EqualError(t, mt.TransmitBatch(t.Context(), types.ConfigDigest{1}, 7, reports, nil), "transmitter is not started")

	require.NoError(t, mt.StartOnce("LLOMercuryTransmitter", func() error { return nil }))
	require.NoError(t, mt.TransmitBatch(t.Context(), types.ConfigDigest{1}, 7, reports, nil))

	// One send carries every report for every server.
	require.Len(t, mt.commitCh, 1)
	batch := <-mt.commitCh
	require.Len(t, batch, len(reports)*len(mt.servers))
	perServer := map[string]int{}
	for _, tr := range batch {
		perServer[tr.ServerURL]++
		assert.Equal(t, uint64(7), tr.SeqNr)
	}
	assert.Equal(t, map[string]int{"a": 3, "b": 3}, perServer)

	// A full commit channel fails the batch once the context is done.
	mt.commitCh <- nil
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorContains(t, mt.TransmitBatch(ctx, types.ConfigDigest{1}, 7, reports, nil), "failed to add transmission batch to commit channel")
}
