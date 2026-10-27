package calculated

import (
	"maps"
	"slices"
	"sync"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// ExpressionPins holds the expressions one plugin instance evaluates in the
// process-wide analysis cache, exempt from eviction, so that their compiled
// programs survive however many other expressions admission and peer
// validation analyze.
//
// Holds are counted per expression, so instances sharing the process do not
// unpin each other. Call Release when the instance closes.
//
// A nil *ExpressionPins is usable and pins nothing.
type ExpressionPins struct {
	cache *analysisCache

	mu   sync.Mutex
	held map[string]struct{}
}

func NewExpressionPins() *ExpressionPins {
	return &ExpressionPins{cache: historyAnalysisCache}
}

// Set pins the expressions channelDefinitions evaluate and releases those they
// no longer do. Call it with the definitions about to be evaluated. It is cheap
// when they declare the same expressions as the previous call.
func (p *ExpressionPins) Set(optsCache *protocol.OptsCache, channelDefinitions llotypes.ChannelDefinitions) {
	if p == nil {
		return
	}
	next := evaluatedExpressions(optsCache, channelDefinitions)

	p.mu.Lock()
	defer p.mu.Unlock()
	if maps.Equal(next, p.held) {
		return
	}
	var added, removed []string
	for expression := range next {
		if _, ok := p.held[expression]; !ok {
			added = append(added, expression)
		}
	}
	for expression := range p.held {
		if _, ok := next[expression]; !ok {
			removed = append(removed, expression)
		}
	}
	p.cache.pin(added)
	p.cache.unpin(removed)
	p.held = next
}

// Release drops every hold.
func (p *ExpressionPins) Release() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache.unpin(slices.Collect(maps.Keys(p.held)))
	p.held = nil
}

// evaluatedExpressions returns the distinct expressions evaluation runs for
// channelDefinitions, selecting channels as prepareCalculatedStreams does.
func evaluatedExpressions(optsCache *protocol.OptsCache, channelDefinitions llotypes.ChannelDefinitions) map[string]struct{} {
	expressions := make(map[string]struct{})
	for cid, cd := range channelDefinitions {
		if cd.Tombstone || cd.ReportFormat != llotypes.ReportFormatEVMABIEncodeUnpackedExpr {
			continue
		}
		o, err := getCalculatedStreamOpts(optsCache, cd, cid)
		if err != nil {
			continue
		}
		for _, abi := range o.ABI {
			if abi.Expression != "" {
				expressions[abi.Expression] = struct{}{}
			}
		}
	}
	return expressions
}
