package llo

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/expr-lang/expr"
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
				"IsZero":             IsZero,
				"IsNegative":         IsNegative,
				"IsPositive":         IsPositive,
				"Round":              Round,
				"Max":                Max,
				"Min":                Min,
				"Ceil":               Ceil,
				"Floor":              Floor,
				"Avg":                Avg,
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
		"IsZero":             true,
		"IsNegative":         true,
		"IsPositive":         true,
		"Round":              true,
		"Max":                true,
		"Min":                true,
		"Ceil":               true,
		"Floor":              true,
		"Avg":                true,
	}
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

// Avg returns the average of x and y
func Avg(x, y any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return decimal.Avg(ad, bd), nil
}

// Max returns the maximum of x and y
func Max(x, y any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return decimal.Max(ad, bd), nil
}

// Min returns the minimum of x and y
func Min(x, y any) (decimal.Decimal, error) {
	ad, err := toDecimal(x)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bd, err := toDecimal(y)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return decimal.Min(ad, bd), nil
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
	case decimal.Decimal:
		return v, nil
	default:
		return decimal.Decimal{}, fmt.Errorf("unsupported type: %T", x)
	}
}

// evalDecimal evaluates the given expression and returns the result as a decimal.Decimal
func evalDecimal(stmt string, env environment) (decimal.Decimal, error) {
	r, err := expr.Eval(stmt, env)
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
		if cd.ReportFormat == llotypes.ReportFormatEVMABIEncodeUnpackedExpr {
			env := NewEnv(outcome)

			for _, stream := range cd.Streams {
				p.Logger.Debugw("setting stream value", "channelID", cid, "streamID", stream.StreamID, "aggregator", stream.Aggregator)
				env.SetStreamValue(stream.StreamID, outcome.StreamAggregates[stream.StreamID][stream.Aggregator])
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
						Aggregator: llotypes.AggregatorMedian,
					})
				}
				outcome.ChannelDefinitions[cid] = cd
			}

			for _, abi := range copt.ABI {
				if abi.ExpressionStreamID == 0 {
					p.Logger.Errorw("expression stream ID is 0", "channelID", cid, "expression", abi.Expression)
					break
				}

				if abi.Expression == "" {
					p.Logger.Errorw("expression is empty", "channelID", cid, "expressionStreamID", abi.ExpressionStreamID)
					break
				}

				if len(outcome.StreamAggregates[abi.ExpressionStreamID]) > 0 {
					p.Logger.Errorw(
						"calculated stream aggregate ID already exists, skipping",
						"channelID", cid,
						"ExpressionStreamID", abi.ExpressionStreamID,
						"Expression", abi.Expression,
					)
					break
				}

				p.Logger.Infow("evaluating expression", "channelID", cid, "expression", abi.Expression, "env", fmt.Sprintf("%+v", env))
				value, err := evalDecimal(abi.Expression, env)
				if err != nil {
					p.Logger.Errorw("failed to evaluate expression", "channelID", cid, "expression", abi.Expression, "error", err)
					break
				}

				// update the outcome with the new stream value if expression was successfully evaluated
				outcome.StreamAggregates[abi.ExpressionStreamID] = map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(value),
				}

			}

			env.release()
			p.Logger.Debugw("ChannelDefinition", "channelID", cid, "reportFormat", cd.ReportFormat)
		}
	}
}

type opts struct {
	ABI []struct {
		Type               string            `json:"type"`
		Expression         string            `json:"expression"`
		ExpressionStreamID llotypes.StreamID `json:"expressionStreamID"`
	} `json:"abi"`
}
