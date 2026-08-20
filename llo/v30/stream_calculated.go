package llo

import (
	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol/calculated"
)

// The calculated-stream expression engine lives in llo/protocol/calculated and
// is shared with the v31 plugin. These are thin wrappers over that engine,
// adapting the v30 Outcome/Plugin shape.

// ProcessCalculatedStreams evaluates calculated-stream expressions for the
// outcome's EVMABIEncodeUnpackedExpr channels, appending the calculated streams
// to their channel definitions and writing the evaluated values into the
// outcome's StreamAggregates.
//
// v3.0 commits ChannelDefinitions as part of its outcome, so it keeps the
// deprecated appending variant. v3.1 derives the streams it reports instead,
// via protocol.EffectiveStreams.
func (p *Plugin) ProcessCalculatedStreams(outcome *Outcome) {
	// nil HistoryReader: v30 has no replicated key-value state, so there is
	// nowhere for stream history to live. Expressions using History fail closed
	// rather than evaluating against an empty window.
	calculated.ProcessCalculatedStreamsWithDefinitionAppend(p.Logger, outcome.ChannelDefinitions, outcome.StreamAggregates, outcome.ObservationTimestampNanoseconds, p.OptsCache, nil)
}

// ProcessCalculatedStreamsDryRun validates an expression against synthetic inputs.
func (p *Plugin) ProcessCalculatedStreamsDryRun(expression string) error {
	return calculated.ProcessCalculatedStreamsDryRun(expression)
}
