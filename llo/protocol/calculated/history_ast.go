package calculated

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// HistoryFunctionName is the DSL form that declares and reads a window of a
// stream's past agreed values:
//
//	History(s10001, 600)
//
// It is a compile-time form, not a runtime function. The AST is rewritten so
// each call becomes a plain identifier bound to the loaded window, for two
// reasons:
//
//  1. The requested depth must be known before evaluation, because it is what
//     tells the plugin how much history to keep. A depth discovered at run time
//     is discovered a round too late.
//  2. A stream identifier passed as an argument would resolve through the
//     environment to a scalar, so the function body would receive a single
//     number rather than a reference to the stream.
//
// Both arguments must therefore be literal: a bare stream identifier and an
// integer.
const HistoryFunctionName = "History"

// twapFunctionName is the DSL name TWAP is registered under, shared with the
// static analysis that validates its configuration.
const twapFunctionName = "TWAP"

// Field selects which part of a stored stream value a window projects. One
// stored window serves every field, so History(s1, 10), History(s1_bid, 10) and
// History(s1_ask, 10) share a single series in state and differ only here.
//
// NOTE: Field lives in this file rather than with Series because the AST pass is
// what parses field suffixes and it must not depend on the evaluation types.
type Field uint8

const (
	FieldValue Field = iota
	FieldBid
	FieldAsk
	FieldBenchmark
)

// suffix returns the stream identifier suffix that selects this field.
func (f Field) suffix() string {
	switch f {
	case FieldBid:
		return "_bid"
	case FieldAsk:
		return "_ask"
	case FieldBenchmark:
		return "_benchmark"
	default:
		return ""
	}
}

func (f Field) String() string {
	if s := f.suffix(); s != "" {
		return s[1:]
	}
	return "value"
}

func fieldFromSuffix(suffix string) (Field, bool) {
	switch suffix {
	case "":
		return FieldValue, true
	case "bid":
		return FieldBid, true
	case "ask":
		return FieldAsk, true
	case "benchmark":
		return FieldBenchmark, true
	default:
		return FieldValue, false
	}
}

// HistoryRef is one History call recovered from an expression: which stream and
// field it reads, and how deep. It is both the read and the declaration — the
// plugin derives the depth it must persist per stream from these.
type HistoryRef struct {
	StreamID llotypes.StreamID
	Field    Field
	Count    uint32
}

// envName is the identifier a History call is rewritten to. The name is
// generated from the recovered reference so the declaration and the binding
// cannot drift apart.
func (r HistoryRef) envName() string {
	return fmt.Sprintf("s%d%s__h%d", r.StreamID, r.Field.suffix(), r.Count)
}

func (r HistoryRef) String() string {
	return fmt.Sprintf("History(s%d%s, %d)", r.StreamID, r.Field.suffix(), r.Count)
}

var (
	// historyStreamArg matches the first argument of a History call. It is
	// anchored, unlike streamMatch, because here the whole argument must be a
	// stream identifier and nothing else.
	historyStreamArg = regexp.MustCompile(`^s(\d+)(?:_(bid|ask|benchmark|timestamp))?$`)

	// reservedHistoryName matches the identifier namespace this pass generates.
	// Expressions may not use it directly: allowing that would let a source
	// identifier impersonate a window binding that was never declared, and so
	// never persisted.
	reservedHistoryName = regexp.MustCompile(`__h\d+$`)

	// rangeAcceptingFunctions are the functions a window may be passed to.
	// Anything else — arithmetic, comparisons — takes scalars, so passing a
	// window to one is a mistake worth catching at compile time rather than at
	// evaluation.
	//
	// Some of these are implemented in a later phase; naming them here is
	// deliberate, since an unimplemented function fails loudly at compile time
	// with "unknown name" rather than silently accepting a window.
	rangeAcceptingFunctions = map[string]bool{
		"Avg":       true,
		"Sum":       true,
		"Min":       true,
		"Max":       true,
		"Count":     true,
		"First":     true,
		"Last":      true,
		"Median":    true,
		"Variance":  true,
		"Stddev":    true,
		"Delta":     true,
		"PctChange": true,
		"Spread":    true,
		"SMA":       true,
		"WMA":       true,
		"EMA":       true,
		"TWAP":      true,
	}
)

// ErrHistoryExpression is returned for any expression that misuses History. It
// is a static error: the expression is rejected at configuration time and at
// compile time, never accepted and left to fail during evaluation.
var ErrHistoryExpression = errors.New("invalid History expression")

// historyPatcher rewrites History calls into window identifiers, recording what
// it found. It satisfies ast.Visitor so it can be used both standalone, to
// analyze an expression without compiling it, and as an expr.Patch option during
// compilation. Using one implementation for both means the depth the plugin
// persists and the identifier the program reads can never disagree.
//
// A patcher is single-use and not safe for concurrent use: it accumulates state
// for one expression.
type historyPatcher struct {
	refs []HistoryRef
	errs []error

	// generated holds the identifier nodes this pass created, so position
	// validation can recognise them by identity rather than by name. Identity
	// matters because the same window may legitimately appear more than once in
	// an expression, and each occurrence must be checked on its own.
	generated map[ast.Node]bool
	// approved holds the generated nodes found in a valid position.
	approved map[ast.Node]bool
	// refByNode maps each generated identifier back to what it was generated
	// from, so a consuming call can be checked against the depth it will get.
	refByNode map[ast.Node]HistoryRef

	fanOut uint64
}

func newHistoryPatcher() *historyPatcher {
	return &historyPatcher{
		generated: map[ast.Node]bool{},
		approved:  map[ast.Node]bool{},
		refByNode: map[ast.Node]HistoryRef{},
	}
}

// Visit implements ast.Visitor.
//
// ast.Walk is post-order, so a node's children are visited before the node
// itself. Two consequences the logic here depends on:
//
//   - By the time a call is visited, any History calls among its arguments have
//     already been rewritten into identifiers. Position checking therefore looks
//     for generated identifiers, not for nested calls.
//   - Nodes this pass creates are never visited, so every identifier reaching
//     Visit came from the source. That is what makes the reserved-namespace
//     check meaningful.
func (p *historyPatcher) Visit(node *ast.Node) {
	switch n := (*node).(type) {
	case *ast.IdentifierNode:
		if reservedHistoryName.MatchString(n.Value) {
			p.errorf("identifier %q uses the reserved history namespace (__h<N>); use %s(s<streamID>, <N>) instead", n.Value, HistoryFunctionName)
		}

	case *ast.CallNode:
		callee, ok := n.Callee.(*ast.IdentifierNode)
		if !ok {
			return
		}
		if callee.Value == HistoryFunctionName {
			p.rewrite(node, n)
			return
		}
		if rangeAcceptingFunctions[callee.Value] {
			for _, arg := range n.Arguments {
				if p.generated[arg] {
					p.approved[arg] = true
				}
			}
			if callee.Value == twapFunctionName {
				p.checkTWAP(n)
			}
		}
	}
}

// rewrite validates one History call and replaces it with its window
// identifier. On any validation failure the node is left untouched so the error
// is reported rather than a bad binding being introduced.
func (p *historyPatcher) rewrite(node *ast.Node, call *ast.CallNode) {
	if len(call.Arguments) != 2 {
		p.errorf("%s takes exactly 2 arguments (stream identifier, depth), got %d", HistoryFunctionName, len(call.Arguments))
		return
	}

	streamArg, ok := call.Arguments[0].(*ast.IdentifierNode)
	if !ok {
		p.errorf("%s: first argument must be a stream identifier such as s10001, got %s", HistoryFunctionName, describeNode(call.Arguments[0]))
		return
	}
	match := historyStreamArg.FindStringSubmatch(streamArg.Value)
	if match == nil {
		p.errorf("%s: first argument %q is not a stream identifier", HistoryFunctionName, streamArg.Value)
		return
	}
	if match[2] == "timestamp" {
		p.errorf("%s: _timestamp is not available as a window; timestamps travel inside the window itself", HistoryFunctionName)
		return
	}
	field, ok := fieldFromSuffix(match[2])
	if !ok {
		p.errorf("%s: unsupported stream field %q", HistoryFunctionName, match[2])
		return
	}
	streamID, err := strconv.ParseUint(match[1], 10, 32)
	if err != nil {
		p.errorf("%s: stream ID %q is out of range: %s", HistoryFunctionName, match[1], err)
		return
	}

	depthArg, ok := call.Arguments[1].(*ast.IntegerNode)
	if !ok {
		// Anything computed is rejected: the depth has to be known before
		// evaluation, so it cannot depend on a value.
		p.errorf("%s: depth must be an integer literal, got %s", HistoryFunctionName, describeNode(call.Arguments[1]))
		return
	}
	if depthArg.Value < 1 {
		p.errorf("%s: depth must be at least 1, got %d", HistoryFunctionName, depthArg.Value)
		return
	}
	if depthArg.Value > protocol.MaxHistoryRecordsPerPair {
		p.errorf("%s: depth %d exceeds the maximum of %d", HistoryFunctionName, depthArg.Value, protocol.MaxHistoryRecordsPerPair)
		return
	}

	p.fanOut += uint64(depthArg.Value)
	if p.fanOut > protocol.MaxHistoryRecordsPerExpression {
		p.errorf("total history depth %d across the expression exceeds the maximum of %d", p.fanOut, protocol.MaxHistoryRecordsPerExpression)
		return
	}

	ref := HistoryRef{
		StreamID: llotypes.StreamID(streamID),
		Field:    field,
		Count:    uint32(depthArg.Value),
	}
	p.refs = append(p.refs, ref)

	generated := &ast.IdentifierNode{Value: ref.envName()}
	ast.Patch(node, generated)
	p.generated[*node] = true
	p.refByNode[*node] = ref
}

// checkTWAP validates a TWAP call against the depth of the window it reads.
//
// This is the static half of TWAP validation: whether a configuration can ever be
// satisfied is a property of the expression, so it belongs here rather than at
// evaluation time, where the same condition would surface as a per-round
// rejection and look like a data problem instead of a deployment mistake.
//
// Only literal configuration can be checked. A configuration built at runtime is
// left to the runtime validation in functions_twap.go, which is stricter but
// later.
func (p *historyPatcher) checkTWAP(call *ast.CallNode) {
	if len(call.Arguments) != 2 {
		p.errorf("%s takes exactly 2 arguments (history window, configuration), got %d", twapFunctionName, len(call.Arguments))
		return
	}
	ref, ok := p.refByNode[call.Arguments[0]]
	if !ok {
		// Not reading a window at all; the position rule reports that.
		return
	}
	config, ok := call.Arguments[1].(*ast.MapNode)
	if !ok {
		return // not a literal configuration
	}

	minSamples, found := twapConfigLiteral(config, "minSamples")
	if !found {
		return
	}
	// Compared as int64: minSamples is a literal and can be any integer the
	// parser accepted, so narrowing it to the width of ref.Count would let a
	// value above 2^32 wrap into a small one and pass. The runtime validation
	// still rejects it, but the diagnostic this check exists to give would be
	// lost.
	if minSamples < 1 {
		p.errorf("%s requires minSamples to be at least 1, got %d", twapFunctionName, minSamples)
		return
	}
	if minSamples > int64(ref.Count) {
		p.errorf("%s requires at least %d observations but %s only keeps %d records; increase the history depth or lower minSamples",
			twapFunctionName, minSamples, ref, ref.Count)
	}
}

// twapConfigLiteral reads an integer-literal value out of a configuration map
// literal, reporting whether it was present and literal.
func twapConfigLiteral(config *ast.MapNode, key string) (int64, bool) {
	for _, pair := range config.Pairs {
		kv, ok := pair.(*ast.PairNode)
		if !ok {
			continue
		}
		name, ok := kv.Key.(*ast.StringNode)
		if !ok || name.Value != key {
			continue
		}
		value, ok := kv.Value.(*ast.IntegerNode)
		if !ok {
			return 0, false
		}
		return int64(value.Value), true
	}
	return 0, false
}

// err reports every problem found, including windows left in a position that
// cannot consume them.
//
// The position rule is what turns a confusing evaluation-time failure into a
// configuration-time one: Add(History(s101, 10), 2) is meaningless, and saying
// so at compile time is far better than letting it reach a node and fail inside
// scalar conversion.
func (p *historyPatcher) err() error {
	errs := p.errs
	for node := range p.generated {
		if p.approved[node] {
			continue
		}
		name := "window"
		if id, ok := node.(*ast.IdentifierNode); ok {
			name = id.Value
		}
		errs = append(errs, fmt.Errorf("%s must be passed directly to one of %v; %s cannot be used as a scalar",
			HistoryFunctionName, sortedRangeAcceptingFunctions(), name))
	}
	if len(errs) == 0 {
		return nil
	}
	// Sorted so the same expression always produces the same message: these
	// errors reach configuration tooling, and unstable ordering there is noise.
	msgs := make([]string, 0, len(errs))
	for _, err := range errs {
		msgs = append(msgs, err.Error())
	}
	sort.Strings(msgs)
	joined := make([]error, 0, len(msgs))
	for _, msg := range msgs {
		joined = append(joined, errors.New(msg))
	}
	return fmt.Errorf("%w: %w", ErrHistoryExpression, errors.Join(joined...))
}

func (p *historyPatcher) errorf(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

// sortedRefs returns the recovered references deduplicated and in a
// deterministic order, so callers deriving persisted state from them agree
// across nodes.
func (p *historyPatcher) sortedRefs() []HistoryRef {
	if len(p.refs) == 0 {
		return nil
	}
	seen := make(map[HistoryRef]bool, len(p.refs))
	refs := make([]HistoryRef, 0, len(p.refs))
	for _, ref := range p.refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].StreamID != refs[j].StreamID {
			return refs[i].StreamID < refs[j].StreamID
		}
		if refs[i].Field != refs[j].Field {
			return refs[i].Field < refs[j].Field
		}
		return refs[i].Count < refs[j].Count
	})
	return refs
}

// analyzeHistoryExpression parses an expression and recovers its History
// references without compiling it. It is a pure function of the expression
// string, which is what lets every node derive the same required depths from
// replicated channel definitions.
//
// An expression with no History calls returns no references and no error.
func analyzeHistoryExpression(expression string) ([]HistoryRef, error) {
	tree, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse expression: %s", ErrHistoryExpression, err)
	}

	p := newHistoryPatcher()
	ast.Walk(&tree.Node, p)
	if err := p.err(); err != nil {
		return nil, err
	}
	return p.sortedRefs(), nil
}

func describeNode(node ast.Node) string {
	switch n := node.(type) {
	case *ast.IdentifierNode:
		return fmt.Sprintf("identifier %q", n.Value)
	case *ast.CallNode:
		if callee, ok := n.Callee.(*ast.IdentifierNode); ok {
			return fmt.Sprintf("call to %s", callee.Value)
		}
		return "call"
	case *ast.StringNode:
		return fmt.Sprintf("string %q", n.Value)
	case *ast.FloatNode:
		return fmt.Sprintf("float %v", n.Value)
	case *ast.BinaryNode:
		return fmt.Sprintf("expression %q", n.String())
	case nil:
		return "nothing"
	default:
		return fmt.Sprintf("%s", node.String())
	}
}

func sortedRangeAcceptingFunctions() []string {
	names := make([]string, 0, len(rangeAcceptingFunctions))
	for name := range rangeAcceptingFunctions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
