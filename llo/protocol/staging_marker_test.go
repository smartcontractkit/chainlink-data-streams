package protocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

func Test_StagingMarker(t *testing.T) {
	staging := llotypes.ReportInfo{LifeCycleStage: LifeCycleStageStaging, ReportFormat: llotypes.ReportFormatJSON}

	marker, err := EncodeStagingMarker(ocrtypes.ConfigDigest{1, 2, 3}, 42)
	require.NoError(t, err)
	require.True(t, IsStagingMarker(staging, marker))

	var decoded stagingMarker
	require.NoError(t, json.Unmarshal(marker, &decoded))
	require.Equal(t, ocrtypes.ConfigDigest{1, 2, 3}, decoded.StagingMarker.ConfigDigest)
	require.Equal(t, uint64(42), decoded.StagingMarker.SeqNr)

	t.Run("requires the staging stage and JSON format", func(t *testing.T) {
		require.False(t, IsStagingMarker(llotypes.ReportInfo{LifeCycleStage: LifeCycleStageProduction, ReportFormat: llotypes.ReportFormatJSON}, marker))
		require.False(t, IsStagingMarker(llotypes.ReportInfo{LifeCycleStage: LifeCycleStageStaging, ReportFormat: llotypes.ReportFormatEVMPremiumLegacy}, marker))
	})

	t.Run("a JSON channel report is not a marker", func(t *testing.T) {
		// JSON channel reports start with their first field, ConfigDigest.
		report := []byte(`{"ConfigDigest":"0102","SeqNr":42,"ChannelID":0,"Specimen":true}`)
		require.False(t, IsStagingMarker(staging, report))
	})
}
