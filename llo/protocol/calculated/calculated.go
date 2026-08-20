package calculated

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/goccy/go-json"
	"github.com/shopspring/decimal"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// defaultEnv is the single source of truth for the bindings every expression
// environment must carry. The pool, the reserved-name set and release() are all
// derived from it, so a function cannot be registered in one place and missed in
// another.
//
// Add new functions here and nowhere else.
var defaultEnv = map[string]any{
	"EQ":                 Equal,
	"Equal":              Equal,
	"GT":                 GreaterThan,
	"GreaterThan":        GreaterThan,
	"GTE":                GreaterThanOrEqual,
	"GreaterThanOrEqual": GreaterThanOrEqual,
	"LT":                 LessThan,
	"LessThan":           LessThan,
	"LTE":                LessThanOrEqual,
	"LessThanOrEqual":    LessThanOrEqual,
	"Abs":                Abs,
	"Mul":                Mul,
	"Div":                Div,
	"Add":                Add,
	"Sum":                Sum,
	"Sub":                Sub,
	"Pow":                Pow,
	"Sqrt":               Sqrt,
	"Ln":                 Ln,
	"Log":                Log,
	"IsZero":             IsZero,
	"IsNegative":         IsNegative,
	"IsPositive":         IsPositive,
	"Round":              Round,
	"Max":                Max,
	"Min":                Min,
	"Ceil":               Ceil,
	"Floor":              Floor,
	"Avg":                Avg,
	"Duration":           ParseDuration,

	// History window functions. See history_ast.go for which of these a window
	// may be passed to; that list and these registrations must agree, or an
	// expression will either fail to compile or be rejected as misusing a
	// window.
	"Count":     Count,
	"First":     First,
	"Last":      Last,
	"Median":    Median,
	"Variance":  Variance,
	"Stddev":    Stddev,
	"Delta":     Delta,
	"PctChange": PctChange,
	"Spread":    Spread,
	"SMA":       SMA,
	"WMA":       WMA,
	"EMA":       EMA,
	// TWAP needs the round's observation timestamp to anchor its window, so
	// NewEnv rebinds it per round. This default only reports that it was called
	// against an environment NewEnv did not build.
	"TWAP": twapUnbound,
	// History is rewritten away at compile time (see history_ast.go). It is
	// registered only so that a call surviving to evaluation fails loudly
	// instead of resolving to an undefined identifier or, worse, to something
	// that quietly works on scalars. Reaching it means the AST pass was
	// bypassed.
	HistoryFunctionName: historyCallReached,
}

// historyCallReached is the runtime stub for History. See defaultEnv.
func historyCallReached(...any) (decimal.Decimal, error) {
	return decimal.Decimal{}, fmt.Errorf("%s was not resolved at compile time; this is a bug in expression compilation", HistoryFunctionName)
}

var (
	pool = sync.Pool{
		New: func() any {
			return newDefaultEnvironment()
		},
	}

	// keys is the set of names reserved by defaultEnv, derived so the two can
	// never disagree.
	keys = func() map[string]bool {
		k := make(map[string]bool, len(defaultEnv))
		for name := range defaultEnv {
			k[name] = true
		}
		return k
	}()
)

// newDefaultEnvironment returns a fresh environment carrying every default
// binding.
func newDefaultEnvironment() environment {
	env := make(environment, len(defaultEnv))
	for name, fn := range defaultEnv {
		env[name] = fn
	}
	return env
}

const (
	// precision defines the precision level for power calculations, representing the number of decimal places.
	// See PowerWithPrecision at https://github.com/shopspring/decimal/blob/master/decimal.go#L798.
	precision = 18
	// doublePrecision is used when we intend to further modify the result and we don't want to suffer from rounding
	// errors.
	doublePrecision = 2 * precision
)

type environment map[string]any

func (e environment) SetStreamValue(id llotypes.StreamID, value protocol.StreamValue) error {
	if value == nil {
		return fmt.Errorf("stream value is nil")
	}

	switch value.Type() {
	case protocol.LLOStreamValue_Decimal:
		e[fmt.Sprintf("s%d", id)] = value.(*protocol.Decimal).Decimal()
	case protocol.LLOStreamValue_Quote:
		quote := value.(*protocol.Quote)
		e[fmt.Sprintf("s%d_bid", id)] = quote.Bid
		e[fmt.Sprintf("s%d_benchmark", id)] = quote.Benchmark
		e[fmt.Sprintf("s%d_ask", id)] = quote.Ask
	case protocol.LLOStreamValue_TimestampedStreamValue:
		tsv := value.(*protocol.TimestampedStreamValue)
		e[fmt.Sprintf("s%d_timestamp", id)] = tsv.ObservedAtNanoseconds
		e.SetStreamValue(id, tsv.StreamValue)
	}

	return nil
}

// release returns an environment to the pool, stripped of everything the caller
// added (stream values, observations_timestamp) and with every default binding
// restored.
//
// Restoring rather than assuming is deliberate: release() cannot verify that e
// came from the pool, so a hand-built or partially populated map must be
// repaired here instead of being handed to the next NewEnv caller with functions
// missing. Expressions are compiled against whatever the environment holds, so a
// stripped environment does not misbehave subtly — it fails every expression
// that uses the absent function.
func (e environment) release() {
	for k := range e {
		if _, ok := defaultEnv[k]; !ok {
			delete(e, k)
		}
	}
	// Also repairs a default that the caller shadowed.
	for k, v := range defaultEnv {
		e[k] = v
	}
	pool.Put(e)
}

// NewEnv returns a new environment with the default functions
func NewEnv(observationTimestampNanoseconds uint64) environment {
	env := pool.Get().(environment)
	env["observations_timestamp"] = observationTimestampNanoseconds
	// TWAP's window is anchored on the round's consensus observation timestamp,
	// not on the data, so it is bound per round. release() restores the default
	// binding, which fails if called.
	env["TWAP"] = twapFunc(observationTimestampNanoseconds)
	return env
}

// Equal returns true if x and y are equal
func Equal(x, y any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return false, err
	}
	return ad.Equal(bd), nil
}

// Ceil returns the ceiling of x
func Ceil(x any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return ad.Ceil(), nil
}

// Floor returns the floor of x
func Floor(x any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return ad.Floor(), nil
}

// Avg returns the average of x elements, which may be a single history window or
// a list of scalars.
//
// NOTE: the division is pinned to legacyDivisionPrecision, which is what this
// function has always effectively used via decimal.DivisionPrecision. Raising it
// would change the trailing digits of every existing calculated stream.
func Avg(x ...any) (decimal.Decimal, error) {
	values, err := scalarsOrWindow("Avg", x)
	if err != nil {
		return decimal.Decimal{}, err
	}

	sum := decimal.Zero
	for _, v := range values {
		sum = sum.Add(v)
	}
	return divRoundByInt(sum, len(values), legacyDivisionPrecision)
}

// Max returns the maximum of x elements, which may be a single history window or
// a list of scalars.
func Max(x ...any) (decimal.Decimal, error) {
	values, err := scalarsOrWindow("Max", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return decimal.Max(values[0], values[1:]...), nil
}

// Min returns the minimum of x elements
func Min(x ...any) (decimal.Decimal, error) {
	values, err := scalarsOrWindow("Min", x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return decimal.Min(values[0], values[1:]...), nil
}

// GreaterThan returns true if x is greater than y
func GreaterThan(x, y any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return false, err
	}
	return ad.GreaterThan(bd), nil
}

// GreaterThanOrEqual returns true if x is greater than or equal to y
func GreaterThanOrEqual(x, y any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return false, err
	}
	return ad.GreaterThanOrEqual(bd), nil
}

// LessThan returns true if x is less than y
func LessThan(x, y any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return false, err
	}
	return ad.LessThan(bd), nil
}

// LessThanOrEqual returns true if x is less than or equal to y
func LessThanOrEqual(x, y any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return false, err
	}
	return ad.LessThanOrEqual(bd), nil
}

// Abs returns the absolute value of x
func Abs(x any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return ad.Abs(), nil
}

// Mul returns the product of x and y
func Mul(x, y any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return ad.Mul(bd), nil
}

// Div returns the quotient of x and y
func Div(x, y any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	// NOTE: pinned to legacyDivisionPrecision, the precision this has always
	// effectively used via the mutable decimal.DivisionPrecision global. Raising
	// it would change the trailing digits of every existing calculated stream.
	return divRound(ad, bd, legacyDivisionPrecision)
}

// Add returns the sum of x and y
func Add(x, y any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return ad.Add(bd), nil
}

// Sub returns the difference of x and y
func Sub(x, y any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return ad.Sub(bd), nil
}

// Pow returns x, raised to the power of y
func Pow(x, y any) (decimal.Decimal, error) {
	base, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	power, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	// We use double precision here in order to offset any float approximation errors.
	res, err := decimalPow(base, power, doublePrecision)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return res.Round(precision), nil
}

// Sqrt returns the square root of x. Returns error for negative values.
func Sqrt(x any) (decimal.Decimal, error) {
	n, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if n.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("negative number")
	}
	sqrtPow, _ := toDecimal(0.5)
	// We use double precision here in order to offset any float approximation errors.
	res, err := decimalPow(n, sqrtPow, doublePrecision)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return res.Round(precision), nil
}

// Ln returns the natural logarithm of x.
func Ln(x any) (decimal.Decimal, error) {
	n, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if n.IsZero() {
		return decimal.Decimal{}, fmt.Errorf("cannot represent natural logarithm of 0")
	}
	return decimalLn(n, precision)
}

// Log returns the logarithms of y with base x. This is equivalent to log_x(y).
//
// We use this formula:
//
//	             ln(y)
//	log_x(y)  =  ----
//	             ln(x)
func Log(x, y any) (decimal.Decimal, error) {
	log, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	lnLog, err := decimalLn(log, doublePrecision) // double precision, since we're going to divide them
	if err != nil {
		return decimal.Decimal{}, err
	}

	base, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	lnBase, err := decimalLn(base, doublePrecision) // double precision, since we're going to divide them
	if err != nil {
		return decimal.Decimal{}, err
	}

	return lnBase.DivRound(lnLog, precision), nil
}

// IsZero returns true if x is zero
func IsZero(x any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	return ad.IsZero(), nil
}

// IsNegative returns true if x is negative
func IsNegative(x any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	return ad.IsNegative(), nil
}

// IsPositive returns true if x is positive
func IsPositive(x any) (bool, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return false, err
	}
	return ad.IsPositive(), nil
}

// Round returns the rounded value of x to the given precision
func Round(x any, precision int) (decimal.Decimal, error) {
	if precision > math.MaxInt32 {
		return decimal.Decimal{}, fmt.Errorf("precision is too large")
	}
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return ad.Round(int32(precision)), nil
}

// Truncate truncates off digits from the number, without rounding.
func Truncate(x any, precision int) (decimal.Decimal, error) {
	if precision > math.MaxInt32 {
		return decimal.Decimal{}, fmt.Errorf("precision is too large")
	}
	n, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return n.Truncate(int32(precision)), nil
}

// ParseDuration parses a duration string into a time.ParseDuration
func ParseDuration(x string) (time.Duration, error) {
	return time.ParseDuration(x)
}

// toDecimal converts x to a decimal.Decimal
func toDecimal(x any) (decimal.Decimal, error) {
	switch v := x.(type) {
	case Series:
		// Static analysis rejects windows in scalar positions, so this is a
		// backstop for a bypassed analysis rather than an expected path.
		return decimal.Decimal{}, fmt.Errorf("%w (length %d)", ErrSeriesAsScalar, v.Len())
	case string:
		return decimal.NewFromString(v)
	case int:
		return decimal.NewFromInt(int64(v)), nil
	case int32:
		return decimal.NewFromInt32(v), nil
	case int64:
		return decimal.NewFromInt(v), nil
	case float32:
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return decimal.Decimal{}, fmt.Errorf("invalid float: NaN or Inf")
		}
		return decimal.NewFromFloat32(v), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return decimal.Decimal{}, fmt.Errorf("invalid float: NaN or Inf")
		}
		return decimal.NewFromFloat(v), nil
	case uint:
		return decimal.NewFromUint64(uint64(v)), nil
	case uint32:
		return decimal.NewFromUint64(uint64(v)), nil
	case uint64:
		return decimal.NewFromUint64(v), nil
	case *big.Int:
		return decimal.NewFromBigInt(v, 0), nil
	case decimal.Decimal:
		return v, nil
	case time.Duration:
		return decimal.NewFromInt(int64(v)), nil
	default:
		return decimal.Decimal{}, fmt.Errorf("unsupported type: %T", x)
	}
}

// evalDecimal evaluates the given expression and returns the result as a decimal.Decimal.
//
// History calls are resolved at compile time by rewriting them into window
// identifiers (see history_ast.go), which the caller must already have bound
// into env. Compiled programs are cached per expression string, so parsing,
// patching and compiling happen once per distinct expression per node rather
// than once per round.
func evalDecimal(stmt string, env map[string]any) (decimal.Decimal, error) {
	// NOTE: the env parameter is deliberately map[string]any rather than the
	// named environment type, and must stay that way. Patching an expression
	// discards the checker's type information, so identifier reads fall back to
	// a dynamic fetch that type-asserts the environment to exactly
	// map[string]any; passing the named type through would fail that assertion
	// at run time with "interface conversion: interface {} is
	// calculated.environment".
	program := historyAnalysisCache.program(stmt)
	if program == nil {
		var err error
		// compile with the environment for type checking, disable all builtins
		// to avoid unexpected behaviors, and patch History calls into the
		// window identifiers bound by the caller.
		program, err = expr.Compile(stmt, expr.Env(env), expr.DisableAllBuiltins(), expr.Patch(newHistoryPatcher()))
		if err != nil {
			// Not cached: unlike analysis, a compile failure can be caused by
			// the environment (a stream missing from this channel), so it is
			// not a property of the expression alone.
			return decimal.Decimal{}, fmt.Errorf("failed to compile expression: %w", err)
		}
		historyAnalysisCache.storeProgram(stmt, program)
	}

	r, err := expr.Run(program, env)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("failed to evaluate expression: %w", err)
	}

	d, ok := r.(decimal.Decimal)
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("expected decimal.Decimal, got %T", r)
	}

	return d, nil
}

// maxEvaluationWorkers bounds the goroutines used to evaluate channels
// concurrently. Evaluation is CPU bound and short, and the process has other
// work to do in the same round, so more workers than this buys contention
// rather than throughput.
const maxEvaluationWorkers = 8

// minParallelEvaluationWeight is the amount of evaluation work below which a
// round is evaluated inline. Dispatching goroutines costs more than evaluating a
// handful of scalar expressions — measurably so: a round of 32 scalar channels
// is ~20% slower when it is spread over workers than when it is not.
//
// Weight is measured in evaluated values (see evaluationWeight), and the
// threshold sits above a round of purely scalar channels and far below any round
// that reads history, which is where the work actually is.
const minParallelEvaluationWeight = 256

// ProcessCalculatedStreams evaluates expressions for each channel of the
// EVMABIEncodeUnpackedExpr format, appending the calculated streams to their
// channel definitions and writing the evaluated values into streamAggregates.
// It is version-agnostic: both the v30 and v31 plugins call it with their own
// outcome/precursor fields.
//
// Processing runs in three phases — prepare, evaluate, apply — so that only the
// pure part is parallelized:
//
//   - prepare is sequential. It touches everything that is shared or not
//     goroutine-safe: the opts cache, the stream aggregates it reads inputs
//     from, and the HistoryReader, whose one-read-per-pair memoization is
//     inherently stateful.
//   - evaluate is pure and runs concurrently. A compiled program plus an
//     environment is all it needs, and nothing it touches is shared with
//     another channel.
//   - apply is sequential and walks channels in ascending channel ID order, so
//     the aggregates written, the definitions mutated and the errors logged do
//     not depend on how the work was scheduled.
func ProcessCalculatedStreams(lggr logger.Logger, channelDefinitions llotypes.ChannelDefinitions, streamAggregates protocol.StreamAggregates, observationTimestampNanoseconds uint64, optsCache *protocol.OptsCache, history HistoryReader) {
	works := prepareCalculatedStreams(lggr, channelDefinitions, streamAggregates, observationTimestampNanoseconds, optsCache, history)
	if len(works) == 0 {
		return
	}
	results := evaluateCalculatedStreams(works)
	applyCalculatedStreams(lggr, channelDefinitions, streamAggregates, works, results)
}

// channelWork is one channel's fully materialized evaluation input: everything
// read out of shared state, so that evaluation itself reads nothing but this
// struct.
type channelWork struct {
	cid  llotypes.ChannelID
	cd   llotypes.ChannelDefinition
	opts calculatedStreamOpts
	env  environment

	// windows holds the history bound for each of the first len(windows)
	// expressions, in declaration order. It is shorter than opts.ABI when
	// binding failed, in which case err says why and expressions from
	// len(windows) on are not evaluated.
	windows [][]boundSeries
	err     error
}

// channelResult is one channel's evaluation output. values holds the value of
// each of the first len(values) expressions; err, if set, is why the expression
// at index len(values) could not be produced and why the ones after it were not
// attempted.
type channelResult struct {
	values []decimal.Decimal
	err    error
}

// prepareCalculatedStreams builds the evaluation input for every eligible
// channel, in ascending channel ID order.
//
// It is deliberately sequential: it is the only phase that reads the opts cache,
// the stream aggregates and the HistoryReader, and the reader's contract is one
// underlying state read per (streamID, aggregator) pair per round, which a
// memoizing implementation cannot honour from several goroutines at once.
func prepareCalculatedStreams(lggr logger.Logger, channelDefinitions llotypes.ChannelDefinitions, streamAggregates protocol.StreamAggregates, observationTimestampNanoseconds uint64, optsCache *protocol.OptsCache, history HistoryReader) []channelWork {
	cids := make([]llotypes.ChannelID, 0, len(channelDefinitions))
	for cid, cd := range channelDefinitions {
		if cd.Tombstone || cd.ReportFormat != llotypes.ReportFormatEVMABIEncodeUnpackedExpr {
			continue
		}
		cids = append(cids, cid)
	}
	if len(cids) == 0 {
		return nil
	}
	slices.Sort(cids)

	works := make([]channelWork, 0, len(cids))
	for _, cid := range cids {
		cd := channelDefinitions[cid]

		// aggByStream resolves the aggregator a History call refers to. The DSL
		// names only a stream, so the channel definition is what says which
		// aggregation of it the expression sees.
		aggByStream, err := AggregatorByStream(cd)
		if err != nil {
			// Ambiguous aggregation cannot be resolved deterministically, so
			// the channel is skipped rather than guessed at.
			lggr.Errorw("skipping channel with ambiguous stream aggregators", "channelID", cid, "error", err)
			continue
		}

		env := NewEnv(observationTimestampNanoseconds)
		for _, stream := range cd.Streams {
			if stream.Aggregator == llotypes.AggregatorCalculated {
				continue
			}

			lggr.Debugw("setting stream value", "channelID", cid, "streamID", stream.StreamID, "aggregator", stream.Aggregator)

			if err = env.SetStreamValue(stream.StreamID, streamAggregates[stream.StreamID][stream.Aggregator]); err != nil {
				lggr.Errorw("failed to set stream value", "channelID", cid, "error", err, "streamID", stream.StreamID, "aggregator", stream.Aggregator)
				env.release()
				break
			}
		}

		if err != nil {
			continue
		}

		copt, getErr := getCalculatedStreamOpts(optsCache, cd, cid)
		if getErr != nil {
			lggr.Errorw("failed to resolve calculated stream opts", "channelID", cid, "error", getErr)
			env.release()
			continue
		}

		if len(cd.Streams) == 0 {
			lggr.Errorw("no streams found in channel definition", "channelID", cid)
			env.release()
			continue
		}

		work := channelWork{cid: cid, cd: cd, opts: copt, env: env}
		work.windows, work.err = bindChannelHistory(&copt, cid, history, aggByStream)
		works = append(works, work)
	}
	return works
}

// evaluateCalculatedStreams evaluates every prepared channel. This is the only
// phase that runs concurrently, and it is pure: each channel reads its own
// environment and windows and writes its own result slot, so no synchronization
// beyond the wait is needed and the results do not depend on scheduling.
//
// Small batches run inline — spawning goroutines costs more than evaluating a
// handful of expressions.
func evaluateCalculatedStreams(works []channelWork) []channelResult {
	results := make([]channelResult, len(works))

	workers := min(len(works), runtime.GOMAXPROCS(0), maxEvaluationWorkers)
	if evaluationWeight(works) < minParallelEvaluationWeight {
		workers = 1
	}
	if workers <= 1 {
		for i := range works {
			results[i] = evalChannel(&works[i])
		}
		return results
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(works) {
					return
				}
				results[i] = evalChannel(&works[i])
			}
		}()
	}
	wg.Wait()
	return results
}

// evaluationWeight estimates how much evaluation a round holds, in values that
// will be visited: one per expression, plus one per record in each window it
// reads. It is the cheapest predictor of cost that distinguishes the two cases
// that matter — a round of scalar arithmetic from a round of window functions —
// and it is derived from prepared work, so it costs nothing to compute.
func evaluationWeight(works []channelWork) int {
	weight := 0
	for i := range works {
		for _, window := range works[i].windows {
			weight++
			for _, bound := range window {
				weight += bound.series.Len()
			}
		}
	}
	return weight
}

// applyCalculatedStreams commits the evaluated values, in ascending channel ID
// order so that the outcome does not depend on the order the work completed in.
//
// The duplicate-aggregate check lives here rather than in evaluation because it
// is the one check whose answer depends on what earlier channels wrote.
func applyCalculatedStreams(lggr logger.Logger, channelDefinitions llotypes.ChannelDefinitions, streamAggregates protocol.StreamAggregates, works []channelWork, results []channelResult) {
	for i := range works {
		work := &works[i]
		result := &results[i]

		// channel definitions are inherited from the previous outcome,
		// so we only update the channel definition streams if we haven't done it before
		cd := work.cd
		if cd.Streams[len(cd.Streams)-1].StreamID != work.opts.ABI[len(work.opts.ABI)-1].ExpressionStreamID {
			for _, abi := range work.opts.ABI {
				cd.Streams = append(cd.Streams, llotypes.Stream{
					StreamID:   abi.ExpressionStreamID,
					Aggregator: llotypes.AggregatorCalculated,
				})
			}
			channelDefinitions[work.cid] = cd
		}

		err := result.err
		for j, value := range result.values {
			abi := work.opts.ABI[j]
			if len(streamAggregates[abi.ExpressionStreamID]) > 0 {
				err = fmt.Errorf(
					"calculated stream aggregate ID already exists, channelID: %d, expressionStreamID: %d, expression: %s",
					work.cid, abi.ExpressionStreamID, abi.Expression)
				break
			}
			// update the aggregates with the new stream value if expression was successfully evaluated
			streamAggregates[abi.ExpressionStreamID] = map[llotypes.Aggregator]protocol.StreamValue{
				llotypes.AggregatorCalculated: protocol.ToDecimal(value),
			}
		}

		if err != nil {
			if errors.Is(err, ErrInsufficientHistory) {
				// Expected while history warms up, after a depth increase, or
				// after corrupt state was discarded. No aggregate is written,
				// so the channel is not reportable this round.
				lggr.Infow("insufficient stream history; channel not reportable this round", "channelID", work.cid, "error", err)
			} else {
				lggr.Errorw("failed to process expression", "channelID", work.cid, "error", err)
			}
		}
		work.env.release()
	}
}

// AggregatorByStream indexes a channel's non-calculated streams by stream ID.
// It is the mapping from what a History call names (a stream) to what history is
// keyed by (a stream and an aggregator), and is exported so the plugin derives
// its persisted requirements from exactly the same mapping evaluation uses.
//
// A stream appearing twice under different aggregators makes any History call
// naming it ambiguous — the DSL has no way to say which aggregation is meant —
// so this is rejected rather than resolved arbitrarily. Rejecting is
// deterministic; picking one would differ between nodes if map iteration order
// ever leaked in.
func AggregatorByStream(cd llotypes.ChannelDefinition) (map[llotypes.StreamID]llotypes.Aggregator, error) {
	byStream := make(map[llotypes.StreamID]llotypes.Aggregator, len(cd.Streams))
	for _, stream := range cd.Streams {
		if stream.Aggregator == llotypes.AggregatorCalculated {
			continue
		}
		if existing, ok := byStream[stream.StreamID]; ok && existing != stream.Aggregator {
			return nil, fmt.Errorf("stream %d appears with aggregators %d and %d", stream.StreamID, existing, stream.Aggregator)
		}
		byStream[stream.StreamID] = stream.Aggregator
	}
	return byStream, nil
}

// bindChannelHistory loads every window each of a channel's expressions
// declares, keyed by the identifier the compile-time rewrite will use.
//
// It runs in the sequential prepare phase because it is the only part of
// evaluation that reads persisted state. Binding stops at the first expression
// that cannot be satisfied: the returned slice covers the expressions before it,
// and the error says why that one — and therefore every one after it — is not
// evaluated.
//
// An unsatisfiable window wraps ErrInsufficientHistory. That is not an error in
// the operational sense — it is the warmup gate — but the expression must not be
// evaluated, because a short window would silently change the meaning of the
// result.
func bindChannelHistory(o *calculatedStreamOpts, cid llotypes.ChannelID, history HistoryReader, aggByStream map[llotypes.StreamID]llotypes.Aggregator) ([][]boundSeries, error) {
	windows := make([][]boundSeries, 0, len(o.ABI))
	for _, abi := range o.ABI {
		if abi.ExpressionStreamID == 0 {
			return windows, fmt.Errorf("expression stream ID is 0, channelID: %d, expression: %s",
				cid, abi.Expression)
		}

		if abi.Expression == "" {
			return windows, fmt.Errorf(
				"expression is empty, channelID: %d, expressionStreamID: %d",
				cid, abi.ExpressionStreamID)
		}

		window, err := resolveHistory(abi.Expression, history, aggByStream)
		if err != nil {
			return windows, fmt.Errorf(
				"failed to bind stream history, channelID: %d, expression: %s, error: %w",
				cid, abi.Expression, err)
		}
		windows = append(windows, window)
	}
	return windows, nil
}

// boundSeries is a window and the environment identifier the compile-time
// rewrite of a History call binds it to.
//
// A slice of these rather than a map: a window carries at most a handful of
// series, and the binding is walked once per round per expression, so a map
// would cost an allocation and a hash to save nothing.
type boundSeries struct {
	name   string
	series Series
}

// resolveHistory reads the windows one expression declares, keyed by the
// identifier the compile-time rewrite binds them to. A nil result means the
// expression declares no windows.
func resolveHistory(expression string, history HistoryReader, aggByStream map[llotypes.StreamID]llotypes.Aggregator) ([]boundSeries, error) {
	refs, err := AnalyzeExpressionHistory(expression)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if history == nil {
		// v30 has no replicated key-value state, so there is nowhere for
		// history to live. Fail closed rather than evaluate against nothing.
		return nil, fmt.Errorf("expression uses %s but stream history is unavailable in this protocol version", HistoryFunctionName)
	}

	window := make([]boundSeries, 0, len(refs))
	for _, ref := range refs {
		aggregator, ok := aggByStream[ref.StreamID]
		if !ok {
			return nil, fmt.Errorf("%s references stream %d, which the channel does not observe", HistoryFunctionName, ref.StreamID)
		}
		series, err := history.Series(ref.StreamID, aggregator, ref.Count, ref.Field)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		if uint32(series.Len()) != ref.Count {
			// Defensive: a reader returning a differently sized window would
			// change what the expression computes.
			return nil, fmt.Errorf("%s: reader returned %d records, expected %d", ref, series.Len(), ref.Count)
		}
		window = append(window, boundSeries{name: ref.envName(), series: series})
	}
	return window, nil
}

// evalChannel evaluates a channel's expressions against its prepared
// environment. It is pure — it reads only the work it is given and writes only
// the result it returns — which is what makes it safe to run concurrently.
//
// Expressions do not see each other's values: the environment carries observed
// streams only, so they are independent and evaluation order is immaterial.
// Evaluation still stops at the first failure, so a channel either contributes a
// prefix of its expressions or, at apply time, none of them.
func evalChannel(work *channelWork) channelResult {
	values := make([]decimal.Decimal, 0, len(work.windows))
	for i, window := range work.windows {
		for _, bound := range window {
			work.env[bound.name] = bound.series
		}

		value, err := evalDecimal(work.opts.ABI[i].Expression, work.env)
		if err != nil {
			return channelResult{values: values, err: fmt.Errorf(
				"failed to evaluate expression, channelID: %d, expression: %s, error: %w",
				work.cid, work.opts.ABI[i].Expression, err)}
		}
		values = append(values, value)
	}
	return channelResult{values: values, err: work.err}
}

// calculatedStreamOpts is the options structure for expression/calculated streams.
// It is used with protocol.OptsCache for decoding channel opts in ProcessCalculatedStreams.
type calculatedStreamOpts struct {
	ABI []struct {
		Type               string            `json:"type"`
		Expression         string            `json:"expression"`
		ExpressionStreamID llotypes.StreamID `json:"expressionStreamID"`
	} `json:"abi"`
}

// getCalculatedStreamOpts resolves a channel's calculated-stream opts, preferring
// the (node-local) decode cache and falling back to decoding the channel
// definition's opts on a cache miss. The fallback keeps the result deterministic
// across oracles even when the cache has not been populated (e.g. after a
// restart, or in stages that never reset it).
//
// Returns an error if the opts cannot be decoded or declare no expressions.
func getCalculatedStreamOpts(optsCache *protocol.OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) (calculatedStreamOpts, error) {
	var o calculatedStreamOpts
	var err error
	if optsCache != nil {
		o, err = protocol.GetOpts[calculatedStreamOpts](optsCache, cid)
	}
	if optsCache == nil || err != nil {
		o = calculatedStreamOpts{}
		if uerr := json.Unmarshal(cd.Opts, &o); uerr != nil {
			return o, fmt.Errorf("failed to decode calculated stream opts, channelID: %d: %w", cid, uerr)
		}
	}
	if len(o.ABI) == 0 {
		return o, fmt.Errorf("no expressions found in channel definition, channelID: %d", cid)
	}
	return o, nil
}

// ExpressionStreamIDs returns the calculated (expression) stream IDs declared by
// a channel's opts, in declaration order. It is the source of truth for which
// calculated streams a channel is expected to produce: the streams appended to
// the channel definition by ProcessCalculatedStreams are only present when
// evaluation reached that point, so callers that need to verify completeness
// (e.g. reportability checks) must consult the opts instead.
//
// Returns an error if the opts cannot be resolved, declare no expressions, or
// declare a zero expression stream ID.
func ExpressionStreamIDs(optsCache *protocol.OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) ([]llotypes.StreamID, error) {
	o, err := getCalculatedStreamOpts(optsCache, cd, cid)
	if err != nil {
		return nil, err
	}
	ids := make([]llotypes.StreamID, 0, len(o.ABI))
	for _, abi := range o.ABI {
		if abi.ExpressionStreamID == 0 {
			return nil, fmt.Errorf("expression stream ID is 0, channelID: %d, expression: %s", cid, abi.Expression)
		}
		ids = append(ids, abi.ExpressionStreamID)
	}
	return ids, nil
}

// Expressions returns a channel's expressions in declaration order.
//
// It exists so the plugin can derive history requirements from the same source of
// truth evaluation uses — the channel's opts — rather than from the streams
// appended to the channel definition, which only appear once evaluation has got
// that far.
//
// An ABI entry with no expression is an error, not a skip. Such an entry names a
// calculated stream that nothing can produce: evalExpression fails on it, the
// stream stays absent, and the channel is therefore never reportable. Reporting
// that when the definition is validated is the whole point of validating it.
func Expressions(optsCache *protocol.OptsCache, cd llotypes.ChannelDefinition, cid llotypes.ChannelID) ([]string, error) {
	o, err := getCalculatedStreamOpts(optsCache, cd, cid)
	if err != nil {
		return nil, err
	}
	expressions := make([]string, 0, len(o.ABI))
	for i, abi := range o.ABI {
		if abi.Expression == "" {
			return nil, fmt.Errorf("expression is empty, channelID: %d, abi index: %d, expressionStreamID: %d", cid, i, abi.ExpressionStreamID)
		}
		expressions = append(expressions, abi.Expression)
	}
	return expressions, nil
}

// ProcessCalculatedStreamsDryRun processes the calculated streams for the given expression
// against synthetic inputs and returns an error if it cannot be evaluated. Useful for
// validating expressions.
func ProcessCalculatedStreamsDryRun(expression string) error {
	tree, err := parser.Parse(expression)
	if err != nil {
		return fmt.Errorf("failed to parse expression: %w", err)
	}

	v := &visitor{}
	ast.Walk(&tree.Node, v)

	// Create outcome with required streams
	aggr := protocol.StreamAggregates{}
	streams := []llotypes.Stream{}

	for streamID, kind := range v.Identifiers {
		switch kind {
		case "bid", "ask", "benchmark":
			aggr[streamID] = map[llotypes.Aggregator]protocol.StreamValue{
				llotypes.AggregatorMedian: &protocol.Quote{
					Bid:       decimal.NewFromInt(110000000000000002),
					Ask:       decimal.NewFromInt(110000000000000001),
					Benchmark: decimal.NewFromInt(110000000000000000),
				},
			}
		case "timestamp":
			aggr[streamID] = map[llotypes.Aggregator]protocol.StreamValue{
				llotypes.AggregatorMedian: &protocol.TimestampedStreamValue{
					ObservedAtNanoseconds: uint64(time.Now().UnixNano()),
					StreamValue:           protocol.ToDecimal(decimal.NewFromInt(109999999999999999)),
				},
			}
		default:
			aggr[streamID] = map[llotypes.Aggregator]protocol.StreamValue{
				llotypes.AggregatorMedian: protocol.ToDecimal(decimal.NewFromInt(109999999999999998)),
			}
		}

		streams = append(streams, llotypes.Stream{
			StreamID:   streamID,
			Aggregator: llotypes.AggregatorMedian,
		})
	}

	cd := llotypes.ChannelDefinitions{
		1: {
			ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
			Streams:      streams,
			Opts:         []byte(fmt.Sprintf(`{"abi":[{"type":"int256","expression":"%s","expressionStreamID":999}]}`, expression)),
		},
	}

	// The same timestamp anchors the environment and the synthesized history, so
	// window-relative functions see records inside their window.
	dryRunObservationTimestampNanoseconds := uint64(time.Now().UnixNano())
	env := NewEnv(dryRunObservationTimestampNanoseconds)
	defer env.release()
	for _, stream := range cd[1].Streams {
		if err := env.SetStreamValue(stream.StreamID, aggr[stream.StreamID][stream.Aggregator]); err != nil {
			return fmt.Errorf("failed to set stream value: %w", err)
		}
	}

	// Process the calculated streams
	o := &calculatedStreamOpts{
		ABI: []struct {
			Type               string            `json:"type"`
			Expression         string            `json:"expression"`
			ExpressionStreamID llotypes.StreamID `json:"expressionStreamID"`
		}{
			{
				Type:               "int256",
				Expression:         expression,
				ExpressionStreamID: 999,
			},
		},
	}
	aggByStream, err := AggregatorByStream(cd[1])
	if err != nil {
		return fmt.Errorf("failed to index channel streams: %w", err)
	}

	// History windows are synthesized: there is no persisted state offline, and
	// what is being validated is the shape of the expression, not the values.
	// The reader always returns the full requested depth so validation
	// exercises the evaluable path rather than the warmup gate.
	dryRunHistory := syntheticHistoryReader{
		endNanoseconds:      dryRunObservationTimestampNanoseconds,
		intervalNanoseconds: uint64(time.Second),
	}

	work := channelWork{cid: 1, cd: cd[1], opts: *o, env: env}
	work.windows, work.err = bindChannelHistory(o, 1, dryRunHistory, aggByStream)
	result := evalChannel(&work)
	if result.err != nil {
		return fmt.Errorf("failed to process expression: %w", result.err)
	}
	for i, value := range result.values {
		aggr[o.ABI[i].ExpressionStreamID] = map[llotypes.Aggregator]protocol.StreamValue{
			llotypes.AggregatorCalculated: protocol.ToDecimal(value),
		}
	}

	if _, ok := aggr[999]; !ok {
		return fmt.Errorf("calculated stream aggregate ID does not exist: %v", aggr[999])
	}

	return nil
}

type visitor struct {
	Identifiers map[llotypes.StreamID]string
}

func (v *visitor) Visit(node *ast.Node) {
	if v.Identifiers == nil {
		v.Identifiers = make(map[llotypes.StreamID]string)
	}

	if n, ok := (*node).(*ast.IdentifierNode); ok {
		match := streamMatch.FindStringSubmatch(n.Value)
		if len(match) > 0 {
			id, err := strconv.ParseUint(match[1], 10, 32)
			if err != nil {
				return
			}
			if _, ok := v.Identifiers[llotypes.StreamID(id)]; !ok && v.Identifiers[llotypes.StreamID(id)] == "" {
				v.Identifiers[llotypes.StreamID(id)] = match[2]
			}
		}
	}
}

var streamMatch = regexp.MustCompile(`s(\d+)(?:_(bid|ask|benchmark|timestamp))?`)
