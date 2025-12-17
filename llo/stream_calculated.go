package llo

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/shopspring/decimal"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

var (
	pool = sync.Pool{
		New: func() any {
			return environment{
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
				"Sum":                Add,
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
				"Duration":           Duration,
			}
		},
	}

	keys = map[string]bool{
		"EQ":                 true,
		"Equal":              true,
		"GT":                 true,
		"GreaterThan":        true,
		"GTE":                true,
		"GreaterThanOrEqual": true,
		"LT":                 true,
		"LessThan":           true,
		"LTE":                true,
		"LessThanOrEqual":    true,
		"Abs":                true,
		"Mul":                true,
		"Div":                true,
		"Add":                true,
		"Sum":                true,
		"Sub":                true,
		"Pow":                true,
		"Sqrt":               true,
		"Ln":                 true,
		"Log":                true,
		"IsZero":             true,
		"IsNegative":         true,
		"IsPositive":         true,
		"Round":              true,
		"Max":                true,
		"Min":                true,
		"Ceil":               true,
		"Floor":              true,
		"Avg":                true,
		"Duration":           true,
	}
)

const (
	// precision defines the precision level for power calculations, representing the number of decimal places.
	// See PowerWithPrecision at https://github.com/shopspring/decimal/blob/master/decimal.go#L798.
	precision = 18
	// doublePrecision is used when we intend to further modify the result and we don't want to suffer from rounding
	// errors.
	doublePrecision = 2 * precision
)

type environment map[string]any

func (e environment) SetStreamValue(id llotypes.StreamID, value StreamValue) error {
	if value == nil {
		return fmt.Errorf("stream value is nil")
	}

	switch value.Type() {
	case LLOStreamValue_Decimal:
		e[fmt.Sprintf("s%d", id)] = value.(*Decimal).Decimal()
	case LLOStreamValue_Quote:
		quote := value.(*Quote)
		e[fmt.Sprintf("s%d_bid", id)] = quote.Bid
		e[fmt.Sprintf("s%d_benchmark", id)] = quote.Benchmark
		e[fmt.Sprintf("s%d_ask", id)] = quote.Ask
	case LLOStreamValue_TimestampedStreamValue:
		tsv := value.(*TimestampedStreamValue)
		e[fmt.Sprintf("s%d_timestamp", id)] = tsv.ObservedAtNanoseconds
		e.SetStreamValue(id, tsv.StreamValue)
	}

	return nil
}

func (e environment) release() {
	for k := range e {
		if keys[k] {
			continue
		}
		delete(e, k)
	}
	pool.Put(e)
}

// NewEnv returns a new environment with the default functions
func NewEnv(outcome *Outcome) environment {
	env := pool.Get().(environment)
	env["observations_timestamp"] = outcome.ObservationTimestampNanoseconds
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

// Avg returns the average of x elements
func Avg(x ...any) (decimal.Decimal, error) {
	if len(x) == 0 {
		return decimal.Decimal{}, fmt.Errorf("no elements to calculate avg")
	}

	sum := decimal.Zero
	for _, v := range x {
		ad, err := toDecimal(v)
		if err != nil {
			return decimal.Decimal{}, err
		}
		sum = sum.Add(ad)
	}
	return sum.Div(decimal.NewFromInt(int64(len(x)))), nil
}

// Max returns the maximum of x elements
func Max(x ...any) (decimal.Decimal, error) {
	if len(x) == 0 {
		return decimal.Decimal{}, fmt.Errorf("no elements to calculate max")
	}

	max, err := toDecimal(x[0])
	if err != nil {
		return decimal.Decimal{}, err
	}

	for _, v := range x[1:] {
		ad, err := toDecimal(v)
		if err != nil {
			return decimal.Decimal{}, err
		}
		max = decimal.Max(max, ad)
	}
	return max, nil
}

// Min returns the minimum of x elements
func Min(x ...any) (decimal.Decimal, error) {
	if len(x) == 0 {
		return decimal.Decimal{}, fmt.Errorf("no elements to calculate min")
	}

	min, err := toDecimal(x[0])
	if err != nil {
		return decimal.Decimal{}, err
	}

	for _, v := range x[1:] {
		ad, err := toDecimal(v)
		if err != nil {
			return decimal.Decimal{}, err
		}
		min = decimal.Min(min, ad)
	}
	return min, nil
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
	if bd.IsZero() {
		return decimal.Decimal{}, fmt.Errorf("division by zero")
	}

	return ad.Div(bd), nil
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
	res, err := base.PowWithPrecision(power, doublePrecision)
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
	res, err := n.PowWithPrecision(sqrtPow, doublePrecision)
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
	return n.Ln(precision)
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
	lnLog, err := log.Ln(doublePrecision) // double precision, since we're going to divide them
	if err != nil {
		return decimal.Decimal{}, err
	}

	base, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	lnBase, err := base.Ln(doublePrecision) // double precision, since we're going to divide them
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

// Duration parses a duration string into a time.Duration
func Duration(x string) (time.Duration, error) {
	return time.ParseDuration(x)
}

// toDecimal converts x to a decimal.Decimal
func toDecimal(x any) (decimal.Decimal, error) {
	switch v := x.(type) {
	case string:
		return decimal.NewFromString(v)
	case int:
		return decimal.NewFromInt(int64(v)), nil
	case int32:
		return decimal.NewFromInt32(v), nil
	case int64:
		return decimal.NewFromInt(v), nil
	case float32:
		return decimal.NewFromFloat32(v), nil
	case float64:
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

// evalDecimal evaluates the given expression and returns the result as a decimal.Decimal
func evalDecimal(stmt string, env map[string]any) (decimal.Decimal, error) {
	// compile with the environment for	type checking
	// disable all builtins to avoid unexpected behaviors
	p, err := expr.Compile(stmt, expr.Env(env), expr.DisableAllBuiltins())

	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("failed to compile expression: %w", err)
	}

	r, err := expr.Run(p, env)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("failed to evaluate expression: %w", err)
	}

	d, ok := r.(decimal.Decimal)
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("expected decimal.Decimal, got %T", r)
	}

	return d, nil
}

// ProcessCalculatedStreams evaluates expressions for each channel of the
// EncodeUnpackedExpr format and returns each result as a decimal.Decimal
func (p *Plugin) ProcessCalculatedStreams(outcome *Outcome) {
	for cid, cd := range outcome.ChannelDefinitions {
		if cd.ReportFormat != llotypes.ReportFormatEVMABIEncodeUnpackedExpr {
			continue
		}

		// TODO: we can potentially cache the opts for each channel definition
		// and avoid unmarshalling the options on outcome.
		// for now keep it simple as this will require invalidating on
		// channel definitions updates.
		copt := opts{}
		if err := json.Unmarshal(cd.Opts, &copt); err != nil {
			p.Logger.Errorw("failed to unmarshal channel definition options", "channelID", cid, "error", err)
			continue
		}

		if len(copt.ABI) == 0 {
			p.Logger.Errorw("no expressions found in channel definition", "channelID", cid)
			continue

		}

		// channel definitions are inherited from the previous outcome,
		// so we only update the channel definition streams if we haven't done it before
		if cd.Streams[len(cd.Streams)-1].StreamID != copt.ABI[len(copt.ABI)-1].ExpressionStreamID {
			for _, abi := range copt.ABI {
				cd.Streams = append(cd.Streams, llotypes.Stream{
					StreamID:   abi.ExpressionStreamID,
					Aggregator: llotypes.AggregatorCalculated,
				})
			}
			outcome.ChannelDefinitions[cid] = cd
		}

		var err error
		env := NewEnv(outcome)
		for _, stream := range cd.Streams {
			if stream.Aggregator == llotypes.AggregatorCalculated {
				continue
			}

			p.Logger.Debugw("setting stream value", "channelID", cid, "streamID", stream.StreamID, "aggregator", stream.Aggregator)

			if err = env.SetStreamValue(stream.StreamID, outcome.StreamAggregates[stream.StreamID][stream.Aggregator]); err != nil {
				p.Logger.Errorw("failed to set stream value", "channelID", cid, "error", err, "streamID", stream.StreamID, "aggregator", stream.Aggregator)
				env.release()
				break
			}
		}

		if err != nil {
			env.release()
			continue
		}

		if err := p.evalExpression(&copt, cid, env, outcome); err != nil {
			p.Logger.Errorw("failed to process expression", "channelID", cid, "error", err)
		}
		env.release()
	}
}

func (p *Plugin) evalExpression(o *opts, cid llotypes.ChannelID, env environment, outcome *Outcome) error {
	for _, abi := range o.ABI {
		if abi.ExpressionStreamID == 0 {
			return fmt.Errorf("expression stream ID is 0, channelID: %d, expression: %s",
				cid, abi.Expression)
		}

		if abi.Expression == "" {
			return fmt.Errorf(
				"expression is empty, channelID: %d, expressionStreamID: %d",
				cid, abi.ExpressionStreamID)
		}

		if len(outcome.StreamAggregates[abi.ExpressionStreamID]) > 0 {
			return fmt.Errorf(
				"calculated stream aggregate ID already exists, channelID: %d, expressionStreamID: %d, expression: %s",
				cid, abi.ExpressionStreamID, abi.Expression)
		}

		value, err := evalDecimal(abi.Expression, env)
		if err != nil {
			return fmt.Errorf(
				"failed to evaluate expression, channelID: %d, expression: %s, error: %w",
				cid, abi.Expression, err)
		}

		// update the outcome with the new stream value if expression was successfully evaluated
		outcome.StreamAggregates[abi.ExpressionStreamID] = map[llotypes.Aggregator]StreamValue{
			llotypes.AggregatorCalculated: ToDecimal(value),
		}
	}
	return nil
}

type opts struct {
	ABI []struct {
		Type               string            `json:"type"`
		Expression         string            `json:"expression"`
		ExpressionStreamID llotypes.StreamID `json:"expressionStreamID"`
	} `json:"abi"`
}

// ProcessCalculatedStreamsDryRun processes the calculated streams for the given expressions
// and returns the outcome with the calculated streams, useful for testing.
func (p *Plugin) ProcessCalculatedStreamsDryRun(expression string) error {
	tree, err := parser.Parse(expression)
	if err != nil {
		return fmt.Errorf("failed to parse expression: %w", err)
	}

	v := &visitor{}
	ast.Walk(&tree.Node, v)

	// Create outcome with required streams
	aggr := StreamAggregates{}
	streams := []llotypes.Stream{}

	for streamID, kind := range v.Identifiers {
		switch kind {
		case "bid", "ask", "benchmark":
			aggr[streamID] = map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorMedian: &Quote{
					Bid:       decimal.NewFromInt(110000000000000002),
					Ask:       decimal.NewFromInt(110000000000000001),
					Benchmark: decimal.NewFromInt(110000000000000000),
				},
			}
		case "timestamp":
			aggr[streamID] = map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorMedian: &TimestampedStreamValue{
					ObservedAtNanoseconds: uint64(time.Now().UnixNano()),
					StreamValue:           ToDecimal(decimal.NewFromInt(109999999999999999)),
				},
			}
		default:
			aggr[streamID] = map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(109999999999999998)),
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

	outcome := Outcome{
		ObservationTimestampNanoseconds: uint64(time.Now().UnixNano()),
		ChannelDefinitions:              cd,
		StreamAggregates:                aggr,
	}

	env := NewEnv(&outcome)
	defer env.release()
	for _, stream := range cd[1].Streams {
		if err := env.SetStreamValue(stream.StreamID, outcome.StreamAggregates[stream.StreamID][stream.Aggregator]); err != nil {
			return fmt.Errorf("failed to set stream value: %w", err)
		}
	}

	// Process the calculated streams
	o := &opts{
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
	err = p.evalExpression(o, 1, env, &outcome)
	if err != nil {
		return fmt.Errorf("failed to process expression: %w", err)
	}

	if _, ok := outcome.StreamAggregates[999]; !ok {
		return fmt.Errorf("calculated stream aggregate ID does not exist: %v", outcome.StreamAggregates[999])
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
