package llo

import (
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// timeResolutionOpts reads TimeResolution from a channel's opts.
type timeResolutionOpts struct {
	TimeResolution protocol.TimeResolution `json:"TimeResolution"`
}

// reportTimeResolution returns the resolution a channel's report format
// truncates report timestamps to. Such channels must not emit a report whose
// validAfter and observation timestamp fall in the same tick, otherwise the
// encoded report would be not yet valid (ValidFrom after Timestamp).
//
// It reads opts through the round OptsCache. That cache is sealed to the same
// channel generation as the definitions, so it holds every channel opts and
// the result is deterministic across oracles.
//
//   - ReportFormatEVMPremiumLegacy always uses seconds.
//   - ReportFormatEVMABIEncodeUnpacked and ReportFormatEVMABIEncodeUnpackedExpr
//     use the TimeResolution opt, which defaults to seconds (the zero value)
//     when unset. ok is false when the opts cannot be read.
//   - all other formats use full (nanosecond) resolution.
func reportTimeResolution(cd llotypes.ChannelDefinition, channelID llotypes.ChannelID, optsCache *protocol.OptsCache) (res protocol.TimeResolution, ok bool) {
	switch cd.ReportFormat {
	case llotypes.ReportFormatEVMPremiumLegacy:
		return protocol.ResolutionSeconds, true
	case llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.ReportFormatEVMABIEncodeUnpackedExpr:
		o, err := protocol.GetOpts[timeResolutionOpts](optsCache, channelID)
		if err != nil {
			return 0, false
		}
		return o.TimeResolution, true
	default:
		return protocol.ResolutionNanoseconds, true
	}
}

// tickOverlap reports whether validAfterNs and obsTsNs collapse to the same
// (or an inverted) tick at res. It truncates with protocol.ConvertTimestamp,
// the same conversion the codecs apply when encoding.
func tickOverlap(validAfterNs, obsTsNs uint64, res protocol.TimeResolution) bool {
	return protocol.ConvertTimestamp(validAfterNs, res) >= protocol.ConvertTimestamp(obsTsNs, res)
}
