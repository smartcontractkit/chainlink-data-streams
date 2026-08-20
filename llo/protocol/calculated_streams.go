package protocol

import (
	"encoding/json"
	"errors"
	"fmt"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// CalculatedStreamOpts is the shape of the channel opts that declare calculated
// (expression) streams. It is the single decode of that shape: expression
// evaluation, channel definition verification and stream derivation all read it,
// so they cannot drift apart.
//
// It lives here rather than in llo/protocol/calculated because that package
// depends on this one, and this package needs the shape in order to derive a
// channel's effective streams.
type CalculatedStreamOpts struct {
	ABI []CalculatedStreamABI `json:"abi"`
}

// CalculatedStreamABI is one declared calculated stream: the expression that
// produces it and the stream ID it is published under.
type CalculatedStreamABI struct {
	Type               string            `json:"type"`
	Expression         string            `json:"expression"`
	ExpressionStreamID llotypes.StreamID `json:"expressionStreamID"`
}

// DecodeCalculatedStreamOpts decodes a channel definition's calculated stream
// opts, preferring the (node-local) decode cache and falling back to decoding
// the definition's raw opts on a cache miss. The fallback keeps the result
// identical across oracles even when the cache has not been populated (e.g.
// after a restart, or in stages that never reset it).
//
// Returns an error if the opts cannot be decoded or declare no expressions.
// The error does not name the channel: every caller already has the channel ID
// in hand and wraps with it, and naming it here produced it twice.
func DecodeCalculatedStreamOpts(optsCache *OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) (CalculatedStreamOpts, error) {
	var o CalculatedStreamOpts
	var err error
	if optsCache != nil {
		o, err = GetOpts[CalculatedStreamOpts](optsCache, cid)
	}
	if optsCache == nil || err != nil {
		o = CalculatedStreamOpts{}
		if uerr := json.Unmarshal(cd.Opts, &o); uerr != nil {
			return o, fmt.Errorf("failed to decode calculated stream opts: %w", uerr)
		}
	}
	if len(o.ABI) == 0 {
		return o, errors.New("no expressions found in channel definition")
	}
	return o, nil
}

// HasCalculatedStreams reports whether a channel definition's report format
// declares calculated streams. It is the definition's format, not its stream
// list, that answers this: calculated streams are derived from the opts and are
// not required to be present on the definition.
func HasCalculatedStreams(cd llotypes.ChannelDefinition) bool {
	return cd.ReportFormat == llotypes.ReportFormatEVMABIEncodeUnpackedExpr
}

// EffectiveStreams returns the streams a channel actually reports: the streams
// its definition observes, followed by one calculated stream per expression its
// opts declare, in declaration order.
//
// This is a pure function of (definition, opts), which is the point. Calculated
// streams are derived, never stored: a definition is exactly what was voted on,
// and every node derives the same effective list from it without the derivation
// having to be replicated. Callers that need to know which streams a report
// carries — report value assembly above all — must go through here rather than
// reading cd.Streams directly.
//
// The trailing position of the calculated streams is a contract, not an
// accident: ReportCodecEVMABIEncodeUnpackedExpr encodes the last len(opts.ABI)
// report values as its payload.
//
// Inline llotypes.AggregatorCalculated entries on the definition are dropped
// before the derived ones are appended. Definitions written by older code
// carried the calculated streams inline, so filtering makes the result identical
// whether or not the stored definition was mutated, and makes the function
// idempotent under repeated application.
func EffectiveStreams(optsCache *OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) ([]llotypes.Stream, error) {
	if !HasCalculatedStreams(cd) {
		return cd.Streams, nil
	}

	o, err := DecodeCalculatedStreamOpts(optsCache, cd, cid)
	if err != nil {
		return nil, fmt.Errorf("cannot derive effective streams for channel %d: %w", cid, err)
	}

	streams := make([]llotypes.Stream, 0, len(cd.Streams)+len(o.ABI))
	for _, strm := range cd.Streams {
		if strm.Aggregator == llotypes.AggregatorCalculated {
			continue
		}
		streams = append(streams, strm)
	}
	for i, abi := range o.ABI {
		if abi.ExpressionStreamID == 0 {
			return nil, fmt.Errorf("cannot derive effective streams for channel %d: expression stream ID is 0, abi index: %d", cid, i)
		}
		streams = append(streams, llotypes.Stream{
			StreamID:   abi.ExpressionStreamID,
			Aggregator: llotypes.AggregatorCalculated,
		})
	}
	return streams, nil
}

// CalculatedStreamIDs returns the calculated stream IDs a channel's opts
// declare, in declaration order. It is the source of truth for which calculated
// streams a channel is expected to produce.
//
// Returns an error if the opts cannot be resolved, declare no expressions, or
// declare a zero expression stream ID.
func CalculatedStreamIDs(optsCache *OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) ([]llotypes.StreamID, error) {
	o, err := DecodeCalculatedStreamOpts(optsCache, cd, cid)
	if err != nil {
		return nil, err
	}
	ids := make([]llotypes.StreamID, 0, len(o.ABI))
	for i, abi := range o.ABI {
		if abi.ExpressionStreamID == 0 {
			return ids, fmt.Errorf("expression stream ID is 0, abi index: %d", i)
		}
		ids = append(ids, abi.ExpressionStreamID)
	}
	return ids, nil
}
