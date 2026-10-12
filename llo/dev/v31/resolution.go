package llo

import (
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// timeResolutionOpts reads TimeResolution from a channel's opts.
type timeResolutionOpts struct {
	TimeResolution protocol.TimeResolution `json:"TimeResolution"`
}

// isSecondsResolution reports whether a channel's report format encodes report
// timestamps at second-level resolution. Such channels must not emit two reports
// whose validAfter and observation timestamp fall in the same second, otherwise
// the reports would overlap (or be not-yet-valid) once truncated to seconds.
//
// It reads opts through the round OptsCache. That cache is sealed to the same
// channel generation as the definitions, so it holds every channel opts and
// the result is deterministic across oracles.
//
//   - ReportFormatEVMPremiumLegacy always uses seconds.
//   - ReportFormatEVMABIEncodeUnpacked and ReportFormatEVMABIEncodeUnpackedExpr
//     use the TimeResolution opt, which defaults to seconds (the zero value)
//     when unset; malformed opts are treated as non-seconds.
//   - all other formats use full (nanosecond) resolution.
func isSecondsResolution(cd llotypes.ChannelDefinition, channelID llotypes.ChannelID, optsCache *protocol.OptsCache) bool {
	switch cd.ReportFormat {
	case llotypes.ReportFormatEVMPremiumLegacy:
		return true
	case llotypes.ReportFormatEVMABIEncodeUnpacked, llotypes.ReportFormatEVMABIEncodeUnpackedExpr:
		o, err := protocol.GetOpts[timeResolutionOpts](optsCache, channelID)
		return err == nil && o.TimeResolution == protocol.ResolutionSeconds
	default:
		return false
	}
}

// secondsOverlap reports whether validAfterNs and obsTsNs collapse to the same
// (or an inverted) second, which would make a seconds-resolution report invalid.
func secondsOverlap(validAfterNs, obsTsNs uint64) bool {
	return validAfterNs/1e9 >= obsTsNs/1e9
}
