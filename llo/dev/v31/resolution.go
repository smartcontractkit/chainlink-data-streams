package llo

import (
	"github.com/goccy/go-json"

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
// Unlike the v30 equivalent (which reads a node-local, incrementally mutated
// OptsCache and is therefore non-deterministic on a cache miss), this parses the resolution directly from
// the channel definition's opts, which are part of the replicated state — so the
// result is deterministic across oracles.
//
//   - ReportFormatEVMPremiumLegacy always uses seconds.
//   - ReportFormatEVMABIEncodeUnpacked uses the TimeResolution opt, which
//     defaults to seconds (the zero value) when unset; malformed opts are
//     treated as non-seconds.
//   - all other formats use full (nanosecond) resolution.
func isSecondsResolution(cd llotypes.ChannelDefinition) bool {
	switch cd.ReportFormat {
	case llotypes.ReportFormatEVMPremiumLegacy:
		return true
	case llotypes.ReportFormatEVMABIEncodeUnpacked:
		var o timeResolutionOpts // TimeResolution zero value == ResolutionSeconds
		if len(cd.Opts) > 0 {
			if err := json.Unmarshal(cd.Opts, &o); err != nil {
				return false
			}
		}
		return o.TimeResolution == protocol.ResolutionSeconds
	default:
		return false
	}
}

// secondsOverlap reports whether validAfterNs and obsTsNs collapse to the same
// (or an inverted) second, which would make a seconds-resolution report invalid.
func secondsOverlap(validAfterNs, obsTsNs uint64) bool {
	return validAfterNs/1e9 >= obsTsNs/1e9
}
