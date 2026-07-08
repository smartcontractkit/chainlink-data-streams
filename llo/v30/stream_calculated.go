package llo

import (
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
)

// The calculated-stream expression engine lives in llo/common and is shared with
// the v31 plugin. These are thin wrappers over that engine, adapting the v30
// Outcome/Plugin shape.

// ProcessCalculatedStreams evaluates calculated-stream expressions for the
// outcome's EVMABIEncodeUnpackedExpr channels, appending the calculated streams
// to their channel definitions and writing the evaluated values into the
// outcome's StreamAggregates.
func (p *Plugin) ProcessCalculatedStreams(outcome *Outcome) {
	llocommon.ProcessCalculatedStreams(p.Logger, outcome.ChannelDefinitions, outcome.StreamAggregates, outcome.ObservationTimestampNanoseconds, p.OptsCache)
}

// ProcessCalculatedStreamsDryRun validates an expression against synthetic inputs.
func (p *Plugin) ProcessCalculatedStreamsDryRun(expression string) error {
	return llocommon.ProcessCalculatedStreamsDryRun(expression)
}
