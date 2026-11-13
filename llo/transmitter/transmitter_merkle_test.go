package transmitter

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

type recordingSubTransmitter struct {
	services.Service
	mu      sync.Mutex
	reports []ocr3types.ReportWithInfo[llotypes.ReportInfo]
	sigs    [][]types.AttributedOnchainSignature
}

func (r *recordingSubTransmitter) Transmit(_ context.Context, _ types.ConfigDigest, _ uint64, report ocr3types.ReportWithInfo[llotypes.ReportInfo], sigs []types.AttributedOnchainSignature) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, report)
	r.sigs = append(r.sigs, sigs)
	return nil
}

func (r *recordingSubTransmitter) FromAccount(context.Context) (types.Account, error) {
	return "", nil
}

// batchingSubTransmitter takes a bundle's reports in one call.
type batchingSubTransmitter struct {
	recordingSubTransmitter
	batches [][]ocr3types.ReportWithInfo[llotypes.ReportInfo]
}

func (b *batchingSubTransmitter) TransmitBatch(_ context.Context, _ types.ConfigDigest, _ uint64, reports []ocr3types.ReportWithInfo[llotypes.ReportInfo], _ []types.AttributedOnchainSignature) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches = append(b.batches, reports)
	return nil
}

// failingSubTransmitter fails its first transmission.
type failingSubTransmitter struct {
	recordingSubTransmitter
}

func (f *failingSubTransmitter) Transmit(_ context.Context, _ types.ConfigDigest, _ uint64, report ocr3types.ReportWithInfo[llotypes.ReportInfo], _ []types.AttributedOnchainSignature) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, report)
	if len(f.reports) == 1 {
		return errors.New("first fails")
	}
	return nil
}

func Test_transmitter_ExpandsMerkleBundle(t *testing.T) {
	ctx := t.Context()
	subs := []*recordingSubTransmitter{{}, {}}
	tr := &transmitter{
		lggr:            logger.Test(t),
		subTransmitters: []Transmitter{subs[0], subs[1]},
		onTransmit:      &onTransmit{lggr: logger.Test(t)},
	}

	bundle := &protocol.LLOMerkleBundleProto{
		Family: uint32(llotypes.ReportFormatEVMMerkle),
		Leaves: []*protocol.LLOMerkleLeafProto{
			{ChannelID: 1, ReportFormat: uint32(llotypes.ReportFormatEVMABIEncodeUnpacked), Report: []byte("one")},
			{ChannelID: 2, ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined), Report: []byte("two")},
			{ChannelID: 5, ReportFormat: uint32(llotypes.ReportFormatEVMPremiumLegacy), Report: []byte("five")},
		},
	}
	encoded, err := protocol.EncodeMerkleBundle(bundle)
	require.NoError(t, err)
	root, err := protocol.MerkleBundleRoot(bundle)
	require.NoError(t, err)
	info := llotypes.ReportInfo{LifeCycleStage: protocol.LifeCycleStageProduction, ReportFormat: llotypes.ReportFormatEVMMerkle}
	sigs := []types.AttributedOnchainSignature{{Signature: []byte{1}, Signer: 0}, {Signature: []byte{2}, Signer: 3}}

	require.NoError(t, tr.Transmit(ctx, types.ConfigDigest{1}, 9, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: encoded, Info: info}, sigs))

	for _, sub := range subs {
		require.Len(t, sub.reports, len(bundle.Leaves))
		for i, r := range sub.reports {
			assert.Equal(t, info, r.Info)
			assert.Equal(t, sigs, sub.sigs[i])
			report, got, err := protocol.DecodeMerkleReport(r.Report)
			require.NoError(t, err)
			assert.Equal(t, root, got, "report %d does not reach the signed root", i)
			assert.Equal(t, uint32(i), report.LeafIndex) //nolint:gosec // test
			assert.Equal(t, bundle.Leaves[i].ChannelID, report.Leaf.ChannelID)
		}
	}

	t.Run("a batching sub transmitter gets every report in one call", func(t *testing.T) {
		sub := &batchingSubTransmitter{}
		tr := &transmitter{lggr: logger.Test(t), subTransmitters: []Transmitter{sub}, onTransmit: &onTransmit{lggr: logger.Test(t)}}
		require.NoError(t, tr.Transmit(ctx, types.ConfigDigest{1}, 9, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: encoded, Info: info}, sigs))
		require.Len(t, sub.batches, 1)
		assert.Len(t, sub.batches[0], len(bundle.Leaves))
		assert.Empty(t, sub.reports)
	})

	t.Run("a failed report does not drop the rest", func(t *testing.T) {
		sub := &failingSubTransmitter{}
		tr := &transmitter{lggr: logger.Test(t), subTransmitters: []Transmitter{sub}, onTransmit: &onTransmit{lggr: logger.Test(t)}}
		err := tr.Transmit(ctx, types.ConfigDigest{1}, 9, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: encoded, Info: info}, sigs)
		require.EqualError(t, err, "first fails")
		assert.Len(t, sub.reports, len(bundle.Leaves))
	})

	t.Run("an invalid bundle is not transmitted", func(t *testing.T) {
		sub := &recordingSubTransmitter{}
		tr := &transmitter{lggr: logger.Test(t), subTransmitters: []Transmitter{sub}, onTransmit: &onTransmit{lggr: logger.Test(t)}}
		err := tr.Transmit(ctx, types.ConfigDigest{1}, 9, ocr3types.ReportWithInfo[llotypes.ReportInfo]{Report: []byte{0xff}, Info: info}, sigs)
		require.ErrorContains(t, err, "failed to expand evm_merkle bundle")
		assert.Empty(t, sub.reports)
	})
}
