package llo

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/reportcodec"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"
)

// leafCodec encodes a report as its format and channel ID, or as size bytes
// when size is set, so a test can tell reports apart and control their size.
type leafCodec struct{ size int }

func (c leafCodec) Encode(r protocol.Report, cd llotypes.ChannelDefinition, _ *protocol.OptsCache) ([]byte, error) {
	if c.size > 0 {
		b := make([]byte, c.size)
		b[0] = byte(r.ChannelID)
		return b, nil
	}
	return fmt.Appendf(nil, "%s:%d", cd.ReportFormat, r.ChannelID), nil
}

func (leafCodec) Verify(llotypes.ChannelDefinition) error { return nil }

func merklePlugin(t *testing.T, codec leafCodec) *Plugin {
	p := testPlugin(t)
	p.ReportCodecs = map[llotypes.ReportFormat]protocol.ReportCodec{
		llotypes.ReportFormatJSON:                     reportcodec.JSONReportCodec{},
		llotypes.ReportFormatEVMABIEncodeUnpacked:     codec,
		llotypes.ReportFormatEVMStreamlined:           codec,
		llotypes.ReportFormatEVMPremiumLegacy:         codec,
		llotypes.ReportFormatEVMABIEncodeUnpackedExpr: codec,
	}
	return p
}

func merkleChannel(format llotypes.ReportFormat, scheme llotypes.SigningScheme) llotypes.ChannelDefinition {
	return llotypes.ChannelDefinition{
		ReportFormat:  format,
		Streams:       []llotypes.Stream{{StreamID: 100, Aggregator: llotypes.AggregatorMedian}},
		SigningScheme: scheme,
	}
}

// merklePrecursor is a round in which every channel of defs is reportable.
func merklePrecursor(t *testing.T, stage llotypes.LifeCycleStage, defs llotypes.ChannelDefinitions) []byte {
	t.Helper()
	ids := make([]llotypes.ChannelID, 0, len(defs))
	validAfter := map[llotypes.ChannelID]uint64{}
	for id := range defs {
		ids = append(ids, id)
		validAfter[id] = 1_000_000_000
	}
	sortChannelIDs(ids)
	prec, err := encodePrecursor(precursor{
		LifeCycleStage:                  stage,
		ObservationTimestampNanoseconds: 2_000_000_000,
		ChannelDefinitions:              defs,
		ChannelStateSeqNr:               2,
		ValidAfterNanoseconds:           validAfter,
		StreamAggregates: protocol.StreamAggregates{
			100: {llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(1))},
		},
		ReportableChannelIDs: ids,
	})
	require.NoError(t, err)
	return prec
}

func bundleOf(t *testing.T, rwi ocr3types.ReportPlus[llotypes.ReportInfo]) *protocol.LLOMerkleBundleProto {
	t.Helper()
	require.Equal(t, llotypes.ReportFormatEVMMerkle, rwi.ReportWithInfo.Info.ReportFormat)
	bundle, err := protocol.DecodeMerkleBundle(rwi.ReportWithInfo.Report)
	require.NoError(t, err)
	return bundle
}

func Test_Reports_MerkleBundle(t *testing.T) {
	ctx := t.Context()
	defs := llotypes.ChannelDefinitions{
		1: merkleChannel(llotypes.ReportFormatJSON, llotypes.SigningSchemeDefault),
		2: merkleChannel(llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.SigningSchemeMerkle),
		3: merkleChannel(llotypes.ReportFormatEVMPremiumLegacy, llotypes.SigningSchemeMerkle),
		4: merkleChannel(llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.SigningSchemeDefault),
		5: merkleChannel(llotypes.ReportFormatEVMPremiumLegacy, llotypes.SigningSchemeMerkle),
	}

	t.Run("merkle channels are bundled after the per report channels", func(t *testing.T) {
		p := merklePlugin(t, leafCodec{})
		rwis, err := p.Reports(ctx, 3, merklePrecursor(t, protocol.LifeCycleStageProduction, defs))
		require.NoError(t, err)
		require.Len(t, rwis, 3)

		assert.Equal(t, llotypes.ReportFormatJSON, rwis[0].ReportWithInfo.Info.ReportFormat)
		assert.Equal(t, llotypes.ReportFormat(llotypes.ReportFormatEVMABIEncodeUnpacked), rwis[1].ReportWithInfo.Info.ReportFormat)
		assert.Equal(t, "evm_abi_encode_unpacked:4", string(rwis[1].ReportWithInfo.Report))

		assert.Equal(t, protocol.LifeCycleStageProduction, rwis[2].ReportWithInfo.Info.LifeCycleStage)
		bundle := bundleOf(t, rwis[2])
		require.Len(t, bundle.Leaves, 3)
		for i, want := range []struct {
			id     llotypes.ChannelID
			format llotypes.ReportFormat
		}{
			{2, llotypes.ReportFormatEVMABIEncodeUnpacked},
			{3, llotypes.ReportFormatEVMPremiumLegacy},
			{5, llotypes.ReportFormatEVMPremiumLegacy},
		} {
			leaf := bundle.Leaves[i]
			assert.Equal(t, want.id, leaf.ChannelID)
			assert.Equal(t, uint32(want.format), leaf.ReportFormat)
			assert.Equal(t, fmt.Sprintf("%s:%d", want.format, want.id), string(leaf.Report))
		}

		// Every oracle builds the same bundle bytes from the same precursor.
		again, err := merklePlugin(t, leafCodec{}).Reports(ctx, 3, merklePrecursor(t, protocol.LifeCycleStageProduction, defs))
		require.NoError(t, err)
		assert.Equal(t, rwis[2].ReportWithInfo.Report, again[2].ReportWithInfo.Report)
	})

	t.Run("the staging marker stays last", func(t *testing.T) {
		p := merklePlugin(t, leafCodec{})
		rwis, err := p.Reports(ctx, 3, merklePrecursor(t, protocol.LifeCycleStageStaging, defs))
		require.NoError(t, err)
		require.Len(t, rwis, 4)
		assert.Equal(t, protocol.LifeCycleStageStaging, rwis[2].ReportWithInfo.Info.LifeCycleStage)
		bundleOf(t, rwis[2])
		assert.True(t, protocol.IsStagingMarker(rwis[3].ReportWithInfo.Info, rwis[3].ReportWithInfo.Report))
	})

	t.Run("an unknown scheme is not emitted", func(t *testing.T) {
		p := merklePlugin(t, leafCodec{})
		rwis, err := p.Reports(ctx, 3, merklePrecursor(t, protocol.LifeCycleStageProduction, llotypes.ChannelDefinitions{
			1: merkleChannel(llotypes.ReportFormatEVMABIEncodeUnpacked, "both"),
		}))
		require.NoError(t, err)
		assert.Empty(t, rwis)
	})
}

func Test_Reports_MerkleBundleSplitsBySize(t *testing.T) {
	// Three reports of 40% of the report size limit fit two to a bundle.
	p := merklePlugin(t, leafCodec{size: MaxReportBytes * 4 / 10})
	defs := llotypes.ChannelDefinitions{
		1: merkleChannel(llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.SigningSchemeMerkle),
		2: merkleChannel(llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.SigningSchemeMerkle),
		3: merkleChannel(llotypes.ReportFormatEVMPremiumLegacy, llotypes.SigningSchemeMerkle),
	}
	rwis, err := p.Reports(t.Context(), 3, merklePrecursor(t, protocol.LifeCycleStageProduction, defs))
	require.NoError(t, err)
	require.Len(t, rwis, 2)

	var ids []llotypes.ChannelID
	for _, rwi := range rwis {
		assert.LessOrEqual(t, len(rwi.ReportWithInfo.Report), MaxReportBytes)
		for _, leaf := range bundleOf(t, rwi).Leaves {
			ids = append(ids, leaf.ChannelID)
		}
	}
	assert.Equal(t, []llotypes.ChannelID{1, 2, 3}, ids)
}

func Test_Reports_MerkleOversizedLeafHasNoTelemetry(t *testing.T) {
	p := merklePlugin(t, leafCodec{size: MaxReportBytes})
	rtCh := make(chan *protocol.LLOReportTelemetry, 8)
	p.ReportTelemetryCh = rtCh
	// Emitters with N=4, F=1 for seqNr 3 are {3, 0}.
	p.OracleID = 0
	defs := llotypes.ChannelDefinitions{
		1: merkleChannel(llotypes.ReportFormatJSON, llotypes.SigningSchemeDefault),
		2: merkleChannel(llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.SigningSchemeMerkle),
	}
	rwis, err := p.Reports(t.Context(), 3, merklePrecursor(t, protocol.LifeCycleStageProduction, defs))
	require.NoError(t, err)
	require.Len(t, rwis, 1)
	assert.Equal(t, llotypes.ReportFormatJSON, rwis[0].ReportWithInfo.Info.ReportFormat)

	require.Len(t, rtCh, 1)
	assert.Equal(t, uint32(1), (<-rtCh).ChannelId)
}
