package protocol

import (
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// timeResolutionOpts reads TimeResolution from a channel's opts.
type timeResolutionOpts struct {
	TimeResolution TimeResolution `json:"TimeResolution"`
}

// ReportTimeResolution returns the resolution a channel's report codec
// truncates ValidAfterNanoseconds and ObservationTimestampNanoseconds to when
// it encodes a report. It is the single mapping from report format to codec
// truncation: the state transition cannot consult the node-local codecs, so
// the mapping is kept here and tied to each codec's Encode by test.
//
// Every format is listed on purpose. A format without an entry returns an
// error rather than a guess, so a codec added without a mapping is loud: the
// plugin treats its channels as unreportable until the format is classified.
//
// Opts are read through optsCache when it is non-nil and decoded from the
// definition's raw opts otherwise, so the result is the same on every oracle.
func ReportTimeResolution(optsCache *OptsCache, cd llotypes.ChannelDefinition, channelID llotypes.ChannelID) (TimeResolution, error) {
	switch cd.ReportFormat {
	case llotypes.ReportFormatEVMPremiumLegacy:
		return ResolutionSeconds, nil
	case llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.ReportFormatEVMABIEncodeUnpackedExpr:
		// The zero value is seconds, which is the codec default when unset.
		o, err := GetOptsOrDecode[timeResolutionOpts](optsCache, cd, channelID)
		if err != nil {
			return 0, fmt.Errorf("channel opts: %w", err)
		}
		return o.TimeResolution, nil
	case llotypes.ReportFormatJSON, llotypes.ReportFormatEVMStreamlined:
		return ResolutionNanoseconds, nil
	case llotypes.ReportFormatHistoryBackfill:
		return 0, fmt.Errorf("report format %s is encoded with the target channel's codec", cd.ReportFormat)
	case llotypes.ReportFormatRetirement, llotypes.ReportFormatCapabilityTrigger:
		return 0, fmt.Errorf("report format %s has no report codec", cd.ReportFormat)
	default:
		return 0, fmt.Errorf("report format %s has no time resolution mapping", cd.ReportFormat)
	}
}
