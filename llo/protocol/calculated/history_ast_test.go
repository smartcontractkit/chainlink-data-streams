package calculated

import (
	"fmt"
	"strings"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

func TestHistoryRef_EnvName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		ref  HistoryRef
		want string
	}{
		{HistoryRef{StreamID: 10001, Field: FieldValue, Count: 10}, "s10001__h10"},
		{HistoryRef{StreamID: 10001, Field: FieldBid, Count: 300}, "s10001_bid__h300"},
		{HistoryRef{StreamID: 1, Field: FieldAsk, Count: 1}, "s1_ask__h1"},
		{HistoryRef{StreamID: 7, Field: FieldBenchmark, Count: 1024}, "s7_benchmark__h1024"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.ref.envName())
			// Every generated name must land in the reserved namespace, or the
			// spoofing check would not cover it.
			assert.Regexp(t, reservedHistoryName, tc.ref.envName())
		})
	}
}

func TestAnalyzeHistoryExpression_Accepts(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		expression string
		want       []HistoryRef
	}{
		"no history at all": {
			expression: "Add(s1, s2)",
			want:       nil,
		},
		"single window": {
			expression: "Avg(History(s10001, 10))",
			want:       []HistoryRef{{StreamID: 10001, Field: FieldValue, Count: 10}},
		},
		"bid field": {
			expression: "Avg(History(s10001_bid, 300))",
			want:       []HistoryRef{{StreamID: 10001, Field: FieldBid, Count: 300}},
		},
		"ask field": {
			expression: "Avg(History(s10001_ask, 5))",
			want:       []HistoryRef{{StreamID: 10001, Field: FieldAsk, Count: 5}},
		},
		"benchmark field": {
			expression: "Avg(History(s10001_benchmark, 5))",
			want:       []HistoryRef{{StreamID: 10001, Field: FieldBenchmark, Count: 5}},
		},
		"mixed with scalars": {
			expression: "Div(Avg(History(s1, 10)), s2)",
			want:       []HistoryRef{{StreamID: 1, Field: FieldValue, Count: 10}},
		},
		"deduplicated and sorted": {
			expression: "Add(Avg(History(s7, 10)), Add(Avg(History(s1, 20)), Avg(History(s7, 10))))",
			want: []HistoryRef{
				{StreamID: 1, Field: FieldValue, Count: 20},
				{StreamID: 7, Field: FieldValue, Count: 10},
			},
		},
		"same stream different depths": {
			expression: "Add(Avg(History(s1, 10)), Avg(History(s1, 20)))",
			want: []HistoryRef{
				{StreamID: 1, Field: FieldValue, Count: 10},
				{StreamID: 1, Field: FieldValue, Count: 20},
			},
		},
		"same stream different fields": {
			expression: "Add(Avg(History(s1_ask, 10)), Avg(History(s1_bid, 10)))",
			want: []HistoryRef{
				{StreamID: 1, Field: FieldBid, Count: 10},
				{StreamID: 1, Field: FieldAsk, Count: 10},
			},
		},
		"depth at the cap": {
			expression: fmt.Sprintf("Avg(History(s1, %d))", protocol.MaxHistoryRecordsPerPair),
			want:       []HistoryRef{{StreamID: 1, Field: FieldValue, Count: protocol.MaxHistoryRecordsPerPair}},
		},
		"minimum depth": {
			expression: "Avg(History(s1, 1))",
			want:       []HistoryRef{{StreamID: 1, Field: FieldValue, Count: 1}},
		},
		// The case a text-level rewriter gets wrong: only the parser knows this
		// is a string, not a call.
		"history inside a string literal is not a call": {
			expression: `Duration('History(s1, 10)')`,
			want:       nil,
		},
		"scalar stream sharing the ranged stream id": {
			expression: "Div(Avg(History(s1, 10)), s1)",
			want:       []HistoryRef{{StreamID: 1, Field: FieldValue, Count: 10}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refs, err := analyzeHistoryExpression(tc.expression)
			require.NoError(t, err)
			assert.Equal(t, tc.want, refs)
		})
	}
}

func TestAnalyzeHistoryExpression_Rejects(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		expression string
		wantErr    string
	}{
		"depth is an identifier": {
			expression: "Avg(History(s1, n))",
			wantErr:    "depth must be an integer literal",
		},
		"depth is computed": {
			expression: "Avg(History(s1, 10 + 2))",
			wantErr:    "depth must be an integer literal",
		},
		"depth is a float": {
			expression: "Avg(History(s1, 10.5))",
			wantErr:    "depth must be an integer literal",
		},
		"depth is a string": {
			expression: `Avg(History(s1, '10'))`,
			wantErr:    "depth must be an integer literal",
		},
		"depth is zero": {
			expression: "Avg(History(s1, 0))",
			wantErr:    "depth must be at least 1",
		},
		"depth is negative": {
			// Parsed as a unary minus over an integer, so it is not an integer
			// literal node at all.
			expression: "Avg(History(s1, -5))",
			wantErr:    "depth must be an integer literal",
		},
		"depth over the cap": {
			expression: fmt.Sprintf("Avg(History(s1, %d))", protocol.MaxHistoryRecordsPerPair+1),
			wantErr:    "exceeds the maximum",
		},
		"stream argument is a call": {
			expression: "Avg(History(Avg(s1, s2), 10))",
			wantErr:    "first argument must be a stream identifier",
		},
		"stream argument is an expression": {
			expression: "Avg(History(s1 + s2, 10))",
			wantErr:    "first argument must be a stream identifier",
		},
		"stream argument is a string": {
			expression: `Avg(History('s1', 10))`,
			wantErr:    "first argument must be a stream identifier",
		},
		"stream argument is not a stream": {
			expression: "Avg(History(notAStream, 10))",
			wantErr:    "is not a stream identifier",
		},
		"timestamp field is not rangeable": {
			expression: "Avg(History(s1_timestamp, 10))",
			wantErr:    "_timestamp is not available as a window",
		},
		"too few arguments": {
			expression: "Avg(History(s1))",
			wantErr:    "takes exactly 2 arguments",
		},
		"too many arguments": {
			expression: "Avg(History(s1, 10, 20))",
			wantErr:    "takes exactly 2 arguments",
		},
		"no arguments": {
			expression: "Avg(History())",
			wantErr:    "takes exactly 2 arguments",
		},
		// The inner call is rewritten first (ast.Walk is post-order), so the
		// outer call sees a window identifier where it requires a stream.
		"nested history": {
			expression: "Avg(History(History(s1, 10), 10))",
			wantErr:    "is not a stream identifier",
		},
		"scalar position": {
			expression: "Add(History(s1, 10), 2)",
			wantErr:    "must be passed directly to one of",
		},
		"bare top level history": {
			expression: "History(s1, 10)",
			wantErr:    "must be passed directly to one of",
		},
		"window in an arithmetic operator": {
			expression: "History(s1, 10) + 2",
			wantErr:    "must be passed directly to one of",
		},
		"window in a comparison": {
			expression: "GT(History(s1, 10), 2)",
			wantErr:    "must be passed directly to one of",
		},
		"reserved namespace spoofing": {
			expression: "Avg(s1__h10)",
			wantErr:    "reserved history namespace",
		},
		"reserved namespace spoofing with field": {
			expression: "Add(s1_bid__h300, 1)",
			wantErr:    "reserved history namespace",
		},
		"parse error": {
			expression: "Avg(History(s1, 10)",
			wantErr:    "failed to parse expression",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refs, err := analyzeHistoryExpression(tc.expression)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrHistoryExpression)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, refs, "a rejected expression must not yield references")
		})
	}
}

// TestAnalyzeHistoryExpression_FanOut covers the per-expression bound: each
// depth is legal on its own, but their sum is work done every round.
func TestAnalyzeHistoryExpression_FanOut(t *testing.T) {
	t.Parallel()

	perCall := protocol.MaxHistoryRecordsPerPair
	calls := protocol.MaxHistoryRecordsPerExpression / perCall

	within := make([]string, 0, calls)
	for i := range calls {
		within = append(within, fmt.Sprintf("Avg(History(s%d, %d))", i+1, perCall))
	}
	refs, err := analyzeHistoryExpression(strings.Join(within, " + "))
	require.NoError(t, err)
	assert.Len(t, refs, calls)

	over := append(within, fmt.Sprintf("Avg(History(s%d, 1))", calls+1))
	_, err = analyzeHistoryExpression(strings.Join(over, " + "))
	require.ErrorIs(t, err, ErrHistoryExpression)
	assert.Contains(t, err.Error(), "total history depth")
}

// TestAnalyzeHistoryExpression_ReportsEveryProblem checks that a single pass
// surfaces all the problems rather than one at a time, since these errors are
// read by configuration tooling.
func TestAnalyzeHistoryExpression_ReportsEveryProblem(t *testing.T) {
	t.Parallel()

	_, err := analyzeHistoryExpression("Add(History(s1, 0), History(s2, x))")
	require.ErrorIs(t, err, ErrHistoryExpression)
	assert.Contains(t, err.Error(), "depth must be at least 1")
	assert.Contains(t, err.Error(), "depth must be an integer literal")
}

// TestAnalyzeHistoryExpression_Deterministic guards the property the plugin
// depends on: analysis is a pure function of the expression string, so every
// node derives the same required depths.
func TestAnalyzeHistoryExpression_Deterministic(t *testing.T) {
	t.Parallel()

	expression := "Add(Avg(History(s7_bid, 10)), Add(Avg(History(s1, 300)), Avg(History(s7, 10))))"
	first, err := analyzeHistoryExpression(expression)
	require.NoError(t, err)
	for range 20 {
		again, err := analyzeHistoryExpression(expression)
		require.NoError(t, err)
		require.Equal(t, first, again)
	}

	errFirst := func() string {
		_, err := analyzeHistoryExpression("Add(History(s1, 0), History(s2, x))")
		return err.Error()
	}()
	for range 20 {
		_, err := analyzeHistoryExpression("Add(History(s1, 0), History(s2, x))")
		require.Equal(t, errFirst, err.Error(), "error text must be stable")
	}
}

// TestHistoryPatcher_RewritesTree checks the tree the compiler will actually see.
func TestHistoryPatcher_RewritesTree(t *testing.T) {
	t.Parallel()

	tree, err := parser.Parse("Avg(History(s10001_bid, 300))")
	require.NoError(t, err)

	p := newHistoryPatcher()
	ast.Walk(&tree.Node, p)
	require.NoError(t, p.err())

	call, ok := tree.Node.(*ast.CallNode)
	require.True(t, ok)
	require.Len(t, call.Arguments, 1)
	// The History call is gone: what remains is a plain identifier the compiler
	// resolves from the environment.
	id, ok := call.Arguments[0].(*ast.IdentifierNode)
	require.True(t, ok, "expected the History call to be replaced by an identifier, got %T", call.Arguments[0])
	assert.Equal(t, "s10001_bid__h300", id.Value)
}

// TestHistoryPatcher_AsCompileOption exercises the patcher through the same
// expr.Patch hook the evaluator will use, so the analysis path and the
// compilation path cannot diverge.
func TestHistoryPatcher_AsCompileOption(t *testing.T) {
	t.Parallel()

	env := NewEnv(uint64(1750169759775700000))
	defer env.release()
	// Phase 3 binds a real window here; for compilation only the name has to
	// resolve.
	env["s10001__h10"] = struct{}{}

	p := newHistoryPatcher()
	program, err := expr.Compile(
		"Avg(History(s10001, 10))",
		expr.Env(env),
		expr.DisableAllBuiltins(),
		expr.Patch(p),
	)
	require.NoError(t, err)
	require.NoError(t, p.err())
	require.NotNil(t, program)
	assert.Equal(t, []HistoryRef{{StreamID: 10001, Field: FieldValue, Count: 10}}, p.sortedRefs())
}

// TestHistoryPatcher_UnboundWindowFailsAtEvaluation pins where an unbound window
// is caught.
//
// With a map environment, expr resolves unknown identifiers dynamically, so a
// missing window binding does NOT fail compilation — only an unknown *callee*
// does. The window therefore reads as nil and the failure surfaces when a
// function tries to use it. That is still fail-closed (an error, so no
// aggregate is produced), but callers must not rely on compilation to detect
// insufficient or unbound history.
func TestHistoryPatcher_UnboundWindowFailsAtEvaluation(t *testing.T) {
	t.Parallel()

	env := NewEnv(uint64(1750169759775700000))
	defer env.release()

	p := newHistoryPatcher()
	program, err := expr.Compile(
		"Avg(History(s10001, 10))",
		expr.Env(env),
		expr.DisableAllBuiltins(),
		expr.Patch(p),
	)
	require.NoError(t, err, "an unbound window does not fail compilation")
	require.NoError(t, p.err())

	_, err = expr.Run(program, env)
	require.Error(t, err, "an unbound window must fail evaluation")
}

// TestHistoryStubFailsClosed covers the patch-bypass guard: History resolving at
// evaluation time means compilation did not rewrite it, and that must fail
// loudly rather than produce a value.
//
// evalDecimal always applies the patcher, so a bypass can only come from code
// that compiles without expr.Patch — which is what this constructs.
func TestHistoryStubFailsClosed(t *testing.T) {
	t.Parallel()

	require.Contains(t, defaultEnv, HistoryFunctionName, "History must be registered so a bypass cannot hit an undefined identifier")

	env := NewEnv(uint64(1750169759775700000))
	defer env.release()
	env["s1"] = decimal.NewFromInt(1)

	program, err := expr.Compile("History(s1, 10)", expr.Env(map[string]any(env)), expr.DisableAllBuiltins())
	require.NoError(t, err, "the stub keeps History a known name")

	_, err = expr.Run(program, map[string]any(env))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was not resolved at compile time")
}

// TestEvalDecimalPatchesHistory confirms the normal evaluation path rewrites
// History rather than calling the stub, so the stub really is unreachable in
// practice.
func TestEvalDecimalPatchesHistory(t *testing.T) {
	t.Parallel()

	env := NewEnv(uint64(1750169759775700000))
	defer env.release()

	// With the window unbound, the rewrite still happens: the call reaches
	// Count with a nil window rather than reaching the History stub. This is
	// also why bindHistory has to gate evaluation — an unbound or too-short
	// window is not a compile error, it is a nil that arrives at a function.
	_, err := evalDecimal("Count(History(s1, 10))", env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Count expects a history window")
	assert.NotContains(t, err.Error(), "was not resolved at compile time")
}

// TestHistoryPatcher_SingleUse documents that a patcher accumulates state for one
// expression and must not be reused.
func TestHistoryPatcher_SingleUse(t *testing.T) {
	t.Parallel()

	p := newHistoryPatcher()
	for _, expression := range []string{"Avg(History(s1, 10))", "Avg(History(s2, 20))"} {
		tree, err := parser.Parse(expression)
		require.NoError(t, err)
		ast.Walk(&tree.Node, p)
	}
	// Both are recorded, which is why a fresh patcher is required per
	// expression.
	assert.Len(t, p.sortedRefs(), 2)
}

// FuzzAnalyzeHistoryExpression checks that arbitrary expression text cannot
// panic the analysis pass, and that anything it accepts respects the depth
// bounds. Expressions come from channel definitions, which are replicated state:
// a panic here would take the node down, and an out-of-bounds depth accepted
// here would become persisted state.
func FuzzAnalyzeHistoryExpression(f *testing.F) {
	for _, seed := range []string{
		"",
		"s1",
		"Avg(History(s1, 10))",
		"History(s1, 10)",
		"Avg(History(s1_bid, 1024))",
		"Avg(History(s1, 0))",
		"Avg(History(s1, x))",
		"Avg(History(History(s1, 2), 2))",
		"Avg(s1__h10)",
		`Duration('History(s1, 10)')`,
		"Avg(History(s99999999999999999999, 10))",
		"((((",
		"History",
		"History()",
		"Avg(History(s1, 10)) + Avg(History(s2, 20))",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, expression string) {
		refs, err := analyzeHistoryExpression(expression)
		if err != nil {
			require.Nil(t, refs, "a rejected expression must not yield references")
			return
		}
		seen := map[HistoryRef]bool{}
		for _, ref := range refs {
			require.GreaterOrEqual(t, ref.Count, uint32(1))
			require.LessOrEqual(t, ref.Count, uint32(protocol.MaxHistoryRecordsPerPair))
			require.Regexp(t, reservedHistoryName, ref.envName())
			require.False(t, seen[ref], "references must be deduplicated")
			seen[ref] = true
		}
	})
}

func TestFieldFromSuffix(t *testing.T) {
	t.Parallel()

	for suffix, want := range map[string]Field{
		"":          FieldValue,
		"bid":       FieldBid,
		"ask":       FieldAsk,
		"benchmark": FieldBenchmark,
	} {
		got, ok := fieldFromSuffix(suffix)
		require.True(t, ok, "suffix %q", suffix)
		assert.Equal(t, want, got)
	}

	_, ok := fieldFromSuffix("timestamp")
	assert.False(t, ok, "timestamp must not map to a field")
}

// TestRangeAcceptingFunctionsAreRegistered keeps the static-analysis list and the
// environment in agreement. A name in one and not the other means an expression
// either fails to compile or is rejected as misusing a window.
func TestRangeAcceptingFunctionsAreRegistered(t *testing.T) {
	t.Parallel()

	for name := range rangeAcceptingFunctions {
		assert.Contains(t, defaultEnv, name, "%s accepts a window but is not registered", name)
	}
}
