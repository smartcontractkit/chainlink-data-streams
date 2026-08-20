// Package calculated evaluates the expression language used by
// EVMABIEncodeUnpackedExpr channels to derive new stream values from observed
// ones.
//
// It is shared by every plugin version, so anything here that changes an output
// changes it for already-deployed DONs.
//
// # Expressions
//
// An expression is an expr-lang expression evaluated against an environment of
// stream values and functions, and must evaluate to a decimal. Builtins are
// disabled, so only the functions registered in defaultEnv are callable.
//
// Streams are named by identifier: s10001 for a scalar stream, and
// s10001_bid / s10001_benchmark / s10001_ask for a quote stream. A quote has no
// bare value — s10001 alone is not bound for a quote stream. s10001_timestamp is
// bound for timestamped streams. Only streams the channel declares are in scope.
//
//	Div(Add(s1, s2), s3)
//
// # Stream history
//
// History(<stream identifier>, <depth>) reads the last <depth> agreed values of a
// stream:
//
//	Avg(History(s10001, 10))
//	EMA(History(s10001, 50), 20)
//	TWAP(History(s10001, 600), {window: Duration("5m"), minSamples: 240,
//	                            maxHeadGap: 30, maxInteriorGap: 10, maxTailGap: 30})
//
// The call is both the declaration of how much history to persist and the read of
// it. There is no separate configuration: the depth kept for a stream is the
// deepest any live channel asks for.
//
// It is resolved at compile time, not called at run time — the AST is rewritten
// so each call becomes an identifier bound to the loaded window (history_ast.go).
// Two consequences: both arguments must be literal (a bare stream identifier and
// an integer), and the depth is known before evaluation, which is what lets the
// plugin know how much to keep.
//
// History requires replicated state, so it works only on protocol versions that
// have it. On v30 an expression using History fails closed: no value, and the
// channel does not report.
//
// # Functions
//
// Scalar functions, unchanged by history: Add, Sub, Mul, Div, Pow, Sqrt, Ln, Log,
// Abs, Round, Ceil, Floor, Duration, IsZero, IsNegative, IsPositive, and the
// comparisons EQ/Equal, GT/GreaterThan, GTE/GreaterThanOrEqual, LT/LessThan,
// LTE/LessThanOrEqual.
//
// Accepting either a history window or a list of scalars: Avg, Sum, Min, Max. The
// two forms cannot be mixed — Avg(History(s1, 10), s2) is ambiguous about whether
// the scalar is another sample or a weight, so it is rejected.
//
// Window-only:
//
//	Count      number of values
//	First      oldest value
//	Last       newest value (the value this round agreed on)
//	Median     middle value; mean of the two middles for an even length
//	Variance   population variance
//	Stddev     population standard deviation
//	Delta      newest minus oldest
//	PctChange  (newest - oldest) / oldest, as a fraction: 0.05 is a 5% rise
//	Spread     maximum minus minimum
//	SMA(w, n)  simple mean of the newest n
//	WMA(w, n)  linearly weighted, newest weighted n and the oldest of the n weighted 1
//	EMA(w, n)  seeded with the mean of the oldest n, then alpha = 2/(n+1) newest-ward
//	TWAP(w, c) time-weighted average price over c.window, filling gaps by type
//
// A window may only be passed directly to one of these. Add(History(s1, 10), 2) is
// rejected when the expression is validated, not left to fail during evaluation.
//
// # Warmup
//
// A window is readable only once it holds at least the requested depth. Until
// then the expression is not evaluated, no value is written, and the channel is
// not reportable — so its coverage watermark does not advance and no gap is
// falsely claimed.
//
// The operational consequence: adding a History call to a live channel, or
// raising its depth, stops that channel reporting until the window fills. Deploy
// the change as a NEW channel, wait for its history to be satisfied, then retire
// the old one. Lowering a depth takes effect the next round with no gap.
//
// # Limits
//
// Depth per (stream, aggregator) pair, the number of such pairs, the per-round
// byte budget and the per-record size are all bounded by constants in
// llo/protocol/limits.go. They are hardcoded because they determine persisted
// state and so must not vary per node.
//
// A pair denied history because a cap was reached gets none at all, and channels
// reading it do not report. There is no silently shortened window.
//
// # Determinism
//
// Expression results become consensus values, so identical inputs must give
// bit-identical output on every node. See decimalmath.go: no float64 anywhere in
// the calculation, no reliance on decimal.DivisionPrecision (a mutable global),
// fixed rounding at every step of an iterative calculation, and a lock around
// shopspring/decimal's transcendental functions, which are not concurrency-safe.
//
// Div and Avg are pinned to the precision they have always effectively used (16);
// functions added with stream history use 18. Changing the former would move the
// trailing digits of every existing calculated stream.
//
// # Validation
//
// ValidateExpression and ValidateChannelExpressions apply every static rule
// without evaluating anything, and are what a report codec's Verify should call
// so an unusable definition cannot reach consensus.
// ProcessCalculatedStreamsDryRun goes further, evaluating against synthesized
// inputs, and is for offline configuration tooling.
package calculated
