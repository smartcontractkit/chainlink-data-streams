package expression

import (
	"fmt"
	"math"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/shopspring/decimal"
)

var (
	pool = sync.Pool{
		New: func() any {
			return newEnv()
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

type Env map[string]any

func (e Env) Release() {
	for k := range e {
		if keys[k] {
			continue
		}
		delete(e, k)
	}
	pool.Put(e)
}

// NewEnv returns a new environment with the default functions
func NewEnv() Env {
	return pool.Get().(Env)
}

func newEnv() Env {
	return Env{
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

// EvalDecimal evaluates the given expression and returns the result as a decimal.Decimal
func EvalDecimal(stmt string, env Env) (decimal.Decimal, error) {
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
