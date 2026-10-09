package protocol

import (
	"bytes"
	"encoding/json"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// A staging marker is the last report of a staging round. It is never
// transmitted: the transmitter drops it after notifying the transmit listeners,
// which flushes the telemetry buffered for that round. A staging instance has
// no other report guaranteed to reach the transmitter.
//
// It travels as a JSON report, which the keyring already signs, with its own
// shape so it can never be mistaken for a channel report.

type stagingMarker struct {
	StagingMarker stagingMarkerBody `json:"stagingMarker"`
}

type stagingMarkerBody struct {
	ConfigDigest ocrtypes.ConfigDigest `json:"configDigest"`
	SeqNr        uint64                `json:"seqNr"`
}

// stagingMarkerPrefix is how every encoded staging marker starts. JSON channel
// reports start with their first field, ConfigDigest.
var stagingMarkerPrefix = []byte(`{"stagingMarker":`)

// EncodeStagingMarker encodes the staging marker of seqNr.
func EncodeStagingMarker(configDigest ocrtypes.ConfigDigest, seqNr uint64) ([]byte, error) {
	return json.Marshal(stagingMarker{stagingMarkerBody{ConfigDigest: configDigest, SeqNr: seqNr}})
}

// IsStagingMarker reports whether report is a staging marker.
func IsStagingMarker(info llotypes.ReportInfo, report []byte) bool {
	return info.LifeCycleStage == LifeCycleStageStaging &&
		info.ReportFormat == llotypes.ReportFormatJSON &&
		bytes.HasPrefix(report, stagingMarkerPrefix)
}
