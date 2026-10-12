package calculated

import (
	"container/list"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/expr-lang/expr/conf"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// maxAnalysisCacheEntries bounds the expression analysis cache. Channel
// definitions can churn, so the cache must not grow with the number of distinct
// expressions ever seen; a few hundred live expressions is already far more than
// any real configuration.
const maxAnalysisCacheEntries = 1024

// parseExpression is the single parse of the DSL: it applies the expression
// size and node limits, which bound parse work and the recursion of the AST
// walks that follow it. Both limits are consensus-relevant, so they are passed
// explicitly rather than left to expr-lang's own defaults.
func parseExpression(expression string) (*parser.Tree, error) {
	if len(expression) > protocol.MaxExpressionBytes {
		return nil, fmt.Errorf("expression of %d bytes exceeds the maximum of %d", len(expression), protocol.MaxExpressionBytes)
	}
	config := conf.CreateNew()
	config.MaxNodes = protocol.MaxExpressionNodes
	return parser.ParseWithConfig(expression, config)
}

// analysisCache memoizes expression analysis, keyed by the raw expression
// string.
//
// Analysis parses the expression and walks its AST, which previously happened
// on every round for every expression. It is a pure function of the expression
// string, so the result is identical on every node and safe to memoize
// node-locally — the same reasoning that makes the opts cache safe. Nothing
// about consensus depends on whether a given node hits the cache.
//
// Both hits and failures are cached: a rejected expression stays rejected until
// its text changes, and re-deriving the same error every round is wasted work.
type analysisCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front is most recently used
	max     int
}

type analysisEntry struct {
	expression string
	refs       []HistoryRef
	twapCalls  int
	names      []string
	err        error

	// program is the compiled expression, filled in on first successful
	// compilation. Compilation is deferred because it needs an environment,
	// while analysis does not.
	//
	// A program is safe to share across channels even though it was compiled
	// against one channel's environment: identifier reads compile to dynamic
	// map fetches, so the program resolves names from whatever environment it
	// is run with. Only compile failures are environment-dependent, and those
	// are deliberately not cached.
	program *vm.Program
}

func newAnalysisCache(max int) *analysisCache {
	return &analysisCache{
		entries: make(map[string]*list.Element, max),
		order:   list.New(),
		max:     max,
	}
}

// historyAnalysisCache is the process-wide analysis cache.
var historyAnalysisCache = newAnalysisCache(maxAnalysisCacheEntries)

// AnalyzeExpressionHistory returns the History references an expression
// declares: which stream and field each reads, and how deep.
//
// This is the declaration side of the DSL. Callers use it to derive how much
// history to persist per stream, so it must stay a pure function of the
// expression string. An expression using no History returns no references and
// no error; an expression misusing History returns ErrHistoryExpression and no
// references, and must not be evaluated.
//
// The returned slice is deduplicated, ordered by (streamID, field, depth), and
// shared with the cache: callers must not modify it.
func AnalyzeExpressionHistory(expression string) ([]HistoryRef, error) {
	return historyAnalysisCache.analyze(expression)
}

// ChannelTWAPCalls counts the TWAP calls across every expression a channel's
// opts declare. It is what admission charges the channel against
// protocol.MaxTotalTWAPCalls, and like ValidateExpression it is a pure function
// of the definition.
//
// Only expressions that analyze cleanly are counted: one that does not is never
// evaluated, so it does no TWAP work. Rejecting such expressions is
// ValidateChannelExpressions' job. Every declared expression is counted, not
// just those within MaxExpressionsPerChannel, so that a committed definition
// predating that limit is still charged for what it evaluates.
func ChannelTWAPCalls(optsCache *protocol.OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) int {
	o, err := getCalculatedStreamOpts(optsCache, cd, cid)
	if err != nil {
		return 0
	}
	calls := 0
	for _, abi := range o.ABI {
		if abi.Expression == "" {
			continue
		}
		entry := historyAnalysisCache.entry(abi.Expression)
		if entry.err == nil {
			calls += entry.twapCalls
		}
	}
	return calls
}

// ValidateExpression reports whether an expression is statically well formed.
//
// It is the check to run before a channel definition reaches consensus: it
// parses, rewrites History calls, and applies every static rule (argument
// shapes, depth caps, per-expression fan-out, window positions, reserved names,
// TWAP configuration satisfiability). It does not evaluate, so it needs no
// stream values and no persisted state, and it is a pure function of the
// expression string.
//
// A statically invalid expression can never produce a value, so accepting one
// into a channel definition means accepting a channel that will never report.
func ValidateExpression(expression string) error {
	if expression == "" {
		return fmt.Errorf("%w: expression is empty", ErrHistoryExpression)
	}
	_, err := AnalyzeExpressionHistory(expression)
	return err
}

// ValidateChannelExpressions validates every expression a channel's opts declare.
// Errors are joined so one pass reports all of them.
func ValidateChannelExpressions(optsCache *protocol.OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) error {
	expressions, err := Expressions(optsCache, cd, cid)
	if err != nil {
		return err
	}

	// The aggregator a History call resolves to comes from the channel's
	// streams, so an ambiguous channel cannot be validated — or evaluated.
	aggByStream, err := AggregatorByStream(cd)
	if err != nil {
		return fmt.Errorf("channel %d: %w", cid, err)
	}

	var errs []error
	for _, expression := range expressions {
		if expression == "" {
			errs = append(errs, fmt.Errorf("%w: expression is empty", ErrHistoryExpression))
			continue
		}
		entry := historyAnalysisCache.entry(expression)
		if entry.err != nil {
			errs = append(errs, fmt.Errorf("expression %q: %w", expression, entry.err))
			continue
		}
		// Every name the expression reads must be bound at evaluation. A name
		// that is not compiles against no channel environment, so the channel
		// would be installed and fail to compile every round.
		for _, name := range entry.names {
			if !channelBindsName(name, aggByStream) {
				errs = append(errs, fmt.Errorf("expression %q: %w: identifier %q is neither a function nor a stream the channel observes",
					expression, ErrUnknownIdentifier, name))
			}
		}
		refs := entry.refs
		// A History call may only read a stream the channel observes. The DSL
		// names a stream, and the channel definition is what says which
		// aggregation of it is meant, so a reference to a stream that is not
		// there cannot be resolved. Both consumers already refuse it — history
		// requirements skip the reference, and evaluation fails on it — so
		// admitting such a definition installs a channel that reserves no
		// history and never reports. Reporting it here is the whole point of
		// validating the definition.
		for _, ref := range refs {
			if _, ok := aggByStream[ref.StreamID]; !ok {
				errs = append(errs, fmt.Errorf("expression %q: %w: %s references stream %d, which the channel does not observe",
					expression, ErrHistoryExpression, HistoryFunctionName, ref.StreamID))
			}
		}
	}
	return errors.Join(errs...)
}

// ErrUnknownIdentifier is returned for an expression that reads a name a
// channel's evaluation environment does not bind.
var ErrUnknownIdentifier = errors.New("unknown identifier")

// channelBindsName reports whether evaluation binds name for a channel whose
// observed streams are aggByStream. It cannot check a stream identifier's
// field: s1_bid is only bound when s1 observes a quote, and the value type is
// not part of the definition.
func channelBindsName(name string, aggByStream map[llotypes.StreamID]llotypes.Aggregator) bool {
	if _, ok := defaultEnv[name]; ok || name == observationsTimestampName {
		return true
	}
	match := streamIdentifier.FindStringSubmatch(name)
	if match == nil {
		return false
	}
	id, err := strconv.ParseUint(match[1], 10, 32)
	if err != nil {
		return false
	}
	_, ok := aggByStream[llotypes.StreamID(id)]
	return ok
}

func (c *analysisCache) analyze(expression string) ([]HistoryRef, error) {
	entry := c.entry(expression)
	return entry.refs, entry.err
}

func (c *analysisCache) entry(expression string) *analysisEntry {
	if entry, ok := c.get(expression); ok {
		return entry
	}

	refs, twapCalls, names, err := analyzeHistoryExpression(expression)
	return c.put(&analysisEntry{expression: expression, refs: refs, twapCalls: twapCalls, names: names, err: err})
}

func (c *analysisCache) get(expression string) (*analysisEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[expression]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*analysisEntry), true
}

// put inserts entry unless one already exists for its expression, and returns
// the cached entry. Keep the existing one: analysis is pure, so both are
// equivalent, and the existing one may already carry a compiled program.
func (c *analysisCache) put(entry *analysisEntry) *analysisEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	if element, ok := c.entries[entry.expression]; ok {
		c.order.MoveToFront(element)
		return element.Value.(*analysisEntry)
	}

	c.entries[entry.expression] = c.order.PushFront(entry)
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*analysisEntry).expression)
	}
	return entry
}

// program returns the cached compiled program for an expression, or nil if it
// has not been compiled yet.
func (c *analysisCache) program(expression string) *vm.Program {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[expression]
	if !ok {
		return nil
	}
	c.order.MoveToFront(element)
	return element.Value.(*analysisEntry).program
}

// storeProgram attaches a compiled program to an existing entry. It is a no-op
// if the entry was evicted in between, in which case the next round simply
// compiles again.
func (c *analysisCache) storeProgram(expression string, program *vm.Program) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[expression]
	if !ok {
		return
	}
	element.Value.(*analysisEntry).program = program
	c.order.MoveToFront(element)
}

func (c *analysisCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
