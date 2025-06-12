package expression

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToDecimal(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		expected    decimal.Decimal
		expectError bool
	}{
		{
			name:     "string valid",
			input:    "123.45",
			expected: decimal.NewFromFloat(123.45),
		},
		{
			name:        "string invalid",
			input:       "invalid",
			expectError: true,
		},
		{
			name:     "int",
			input:    123,
			expected: decimal.NewFromInt(123),
		},
		{
			name:     "int32",
			input:    int32(123),
			expected: decimal.NewFromInt32(123),
		},
		{
			name:     "int64",
			input:    int64(123),
			expected: decimal.NewFromInt(123),
		},
		{
			name:     "float32",
			input:    float32(123.45),
			expected: decimal.NewFromFloat32(123.45),
		},
		{
			name:     "float64",
			input:    float64(123.45),
			expected: decimal.NewFromFloat(123.45),
		},
		{
			name:     "uint",
			input:    uint(123),
			expected: decimal.NewFromUint64(123),
		},
		{
			name:     "uint32",
			input:    uint32(123),
			expected: decimal.NewFromUint64(123),
		},
		{
			name:     "uint64",
			input:    uint64(123),
			expected: decimal.NewFromUint64(123),
		},
		{
			name:     "decimal.Decimal",
			input:    decimal.NewFromFloat(123.45),
			expected: decimal.NewFromFloat(123.45),
		},
		{
			name:        "unsupported type",
			input:       []int{1, 2, 3},
			expectError: true,
		},
		{
			name:        "nil",
			input:       nil,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := toDecimal(tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.True(t, tt.expected.Equal(result), "expected %s, got %s", tt.expected.String(), result.String())
			}
		})
	}
}

func TestEqual(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    bool
		expectError bool
	}{
		{
			name:     "equal decimals",
			x:        "123.45",
			y:        123.45,
			expected: true,
		},
		{
			name:     "different decimals",
			x:        "123.45",
			y:        123.46,
			expected: false,
		},
		{
			name:     "zero equals zero",
			x:        0,
			y:        "0.00",
			expected: true,
		},
		{
			name:        "invalid x",
			x:           "invalid",
			y:           123.45,
			expectError: true,
		},
		{
			name:        "invalid y",
			x:           123.45,
			y:           "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Equal(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestGreaterThan(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    bool
		expectError bool
	}{
		{
			name:     "x greater than y",
			x:        "123.46",
			y:        123.45,
			expected: true,
		},
		{
			name:     "x less than y",
			x:        "123.44",
			y:        123.45,
			expected: false,
		},
		{
			name:     "x equal to y",
			x:        "123.45",
			y:        123.45,
			expected: false,
		},
		{
			name:        "invalid x",
			x:           "invalid",
			y:           123.45,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GreaterThan(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestGreaterThanOrEqual(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    bool
		expectError bool
	}{
		{
			name:     "x greater than y",
			x:        "123.46",
			y:        123.45,
			expected: true,
		},
		{
			name:     "x equal to y",
			x:        "123.45",
			y:        123.45,
			expected: true,
		},
		{
			name:     "x less than y",
			x:        "123.44",
			y:        123.45,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GreaterThanOrEqual(tt.x, tt.y)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLessThan(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    bool
		expectError bool
	}{
		{
			name:     "x less than y",
			x:        "123.44",
			y:        123.45,
			expected: true,
		},
		{
			name:     "x greater than y",
			x:        "123.46",
			y:        123.45,
			expected: false,
		},
		{
			name:     "x equal to y",
			x:        "123.45",
			y:        123.45,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LessThan(tt.x, tt.y)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLessThanOrEqual(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    bool
		expectError bool
	}{
		{
			name:     "x less than y",
			x:        "123.44",
			y:        123.45,
			expected: true,
		},
		{
			name:     "x equal to y",
			x:        "123.45",
			y:        123.45,
			expected: true,
		},
		{
			name:     "x greater than y",
			x:        "123.46",
			y:        123.45,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := LessThanOrEqual(tt.x, tt.y)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAbs(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		expected    string
		expectError bool
	}{
		{
			name:     "positive number",
			input:    "123.45",
			expected: "123.45",
		},
		{
			name:     "negative number",
			input:    "-123.45",
			expected: "123.45",
		},
		{
			name:     "zero",
			input:    0,
			expected: "0",
		},
		{
			name:     "negative integer",
			input:    -100,
			expected: "100",
		},
		{
			name:        "invalid input",
			input:       "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Abs(tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestMul(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    string
		expectError bool
	}{
		{
			name:     "positive multiplication",
			x:        "12.5",
			y:        2,
			expected: "25",
		},
		{
			name:     "negative multiplication",
			x:        "-12.5",
			y:        2,
			expected: "-25",
		},
		{
			name:     "multiplication by zero",
			x:        "123.45",
			y:        0,
			expected: "0",
		},
		{
			name:     "float multiplication",
			x:        2.5,
			y:        4.0,
			expected: "10",
		},
		{
			name:        "invalid x",
			x:           "invalid",
			y:           2,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Mul(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestDiv(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    string
		expectError bool
	}{
		{
			name:     "normal division",
			x:        "10",
			y:        2,
			expected: "5",
		},
		{
			name:     "division with decimal result",
			x:        "10",
			y:        3,
			expected: "3.3333333333333333",
		},
		{
			name:     "negative division",
			x:        "-10",
			y:        2,
			expected: "-5",
		},
		{
			name:        "division by zero",
			x:           "10",
			y:           0,
			expectError: true,
		},
		{
			name:        "invalid divisor",
			x:           10,
			y:           "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Div(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestAdd(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    string
		expectError bool
	}{
		{
			name:     "positive addition",
			x:        "12.5",
			y:        2.5,
			expected: "15",
		},
		{
			name:     "negative addition",
			x:        "-12.5",
			y:        5,
			expected: "-7.5",
		},
		{
			name:     "zero addition",
			x:        "123.45",
			y:        0,
			expected: "123.45",
		},
		{
			name:        "invalid input",
			x:           "invalid",
			y:           2,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Add(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestSub(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    string
		expectError bool
	}{
		{
			name:     "positive subtraction",
			x:        "15",
			y:        2.5,
			expected: "12.5",
		},
		{
			name:     "negative result",
			x:        "5",
			y:        12.5,
			expected: "-7.5",
		},
		{
			name:     "zero subtraction",
			x:        "123.45",
			y:        0,
			expected: "123.45",
		},
		{
			name:        "invalid input",
			x:           5,
			y:           "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Sub(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestIsZero(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		expected    bool
		expectError bool
	}{
		{
			name:     "zero integer",
			input:    0,
			expected: true,
		},
		{
			name:     "zero string",
			input:    "0",
			expected: true,
		},
		{
			name:     "zero decimal string",
			input:    "0.00",
			expected: true,
		},
		{
			name:     "positive number",
			input:    1,
			expected: false,
		},
		{
			name:     "negative number",
			input:    -1,
			expected: false,
		},
		{
			name:        "invalid input",
			input:       "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := IsZero(tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestIsNegative(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		expected    bool
		expectError bool
	}{
		{
			name:     "negative integer",
			input:    -1,
			expected: true,
		},
		{
			name:     "negative string",
			input:    "-123.45",
			expected: true,
		},
		{
			name:     "positive number",
			input:    1,
			expected: false,
		},
		{
			name:     "zero",
			input:    0,
			expected: false,
		},
		{
			name:        "invalid input",
			input:       "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := IsNegative(tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestIsPositive(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		expected    bool
		expectError bool
	}{
		{
			name:     "positive integer",
			input:    1,
			expected: true,
		},
		{
			name:     "positive string",
			input:    "123.45",
			expected: true,
		},
		{
			name:     "negative number",
			input:    -1,
			expected: false,
		},
		{
			name:     "zero",
			input:    0,
			expected: false,
		},
		{
			name:        "invalid input",
			input:       "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := IsPositive(tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestRound(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		precision   int
		expected    string
		expectError bool
	}{
		{
			name:      "round to 2 decimal places",
			input:     "123.456",
			precision: 2,
			expected:  "123.46",
		},
		{
			name:      "round to 0 decimal places",
			input:     "123.456",
			precision: 0,
			expected:  "123",
		},
		{
			name:      "round negative number",
			input:     "-123.456",
			precision: 1,
			expected:  "-123.5",
		},
		{
			name:      "round with negative precision",
			input:     "123.456",
			precision: -1,
			expected:  "120",
		},
		{
			name:        "precision too large",
			input:       "123.456",
			precision:   math.MaxInt32 + 1,
			expectError: true,
		},
		{
			name:        "invalid input",
			input:       "invalid",
			precision:   2,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Round(tt.input, tt.precision)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestMax(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    string
		expectError bool
	}{
		{
			name:     "x greater than y",
			x:        "123.46",
			y:        123.45,
			expected: "123.46",
		},
		{
			name:     "y greater than x",
			x:        "123.44",
			y:        123.45,
			expected: "123.45",
		},
		{
			name:     "equal values",
			x:        "123.45",
			y:        123.45,
			expected: "123.45",
		},
		{
			name:        "invalid x",
			x:           "invalid",
			y:           123.45,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Max(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestMin(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    string
		expectError bool
	}{
		{
			name:     "x less than y",
			x:        "123.44",
			y:        123.45,
			expected: "123.44",
		},
		{
			name:     "y less than x",
			x:        "123.46",
			y:        123.45,
			expected: "123.45",
		},
		{
			name:     "equal values",
			x:        "123.45",
			y:        123.45,
			expected: "123.45",
		},
		{
			name:        "invalid y",
			x:           123.45,
			y:           "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Min(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestCeil(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		expected    string
		expectError bool
	}{
		{
			name:     "positive decimal",
			input:    "123.45",
			expected: "124",
		},
		{
			name:     "negative decimal",
			input:    "-123.45",
			expected: "-123",
		},
		{
			name:     "integer",
			input:    123,
			expected: "123",
		},
		{
			name:     "zero",
			input:    0,
			expected: "0",
		},
		{
			name:        "invalid input",
			input:       "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Ceil(tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestFloor(t *testing.T) {
	tests := []struct {
		name        string
		input       any
		expected    string
		expectError bool
	}{
		{
			name:     "positive decimal",
			input:    "123.45",
			expected: "123",
		},
		{
			name:     "negative decimal",
			input:    "-123.45",
			expected: "-124",
		},
		{
			name:     "integer",
			input:    123,
			expected: "123",
		},
		{
			name:     "zero",
			input:    0,
			expected: "0",
		},
		{
			name:        "invalid input",
			input:       "invalid",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Floor(tt.input)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestAvg(t *testing.T) {
	tests := []struct {
		name        string
		x, y        any
		expected    string
		expectError bool
	}{
		{
			name:     "positive numbers",
			x:        "10",
			y:        20,
			expected: "15",
		},
		{
			name:     "mixed positive/negative",
			x:        "-10",
			y:        30,
			expected: "10",
		},
		{
			name:     "decimal numbers",
			x:        "12.5",
			y:        17.5,
			expected: "15",
		},
		{
			name:     "equal numbers",
			x:        "123.45",
			y:        123.45,
			expected: "123.45",
		},
		{
			name:        "invalid x",
			x:           "invalid",
			y:           20,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Avg(tt.x, tt.y)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}
		})
	}
}

func TestEvalDecimal(t *testing.T) {
	tests := []struct {
		name        string
		stmt        string
		env         Env
		expected    string
		expectError bool
	}{
		{
			name:     "simple addition",
			stmt:     "Add(10, 5)",
			env:      NewEnv(),
			expected: "15",
		},
		{
			name:     "complex expression",
			stmt:     "Mul(Add(10, 5), 2)",
			env:      NewEnv(),
			expected: "30",
		},
		{
			name:     "with variables",
			stmt:     "Add(x, y)",
			env:      Env{"Add": Add, "x": 10, "y": 5},
			expected: "15",
		},
		{
			name:     "division",
			stmt:     "Div(10, 2)",
			env:      NewEnv(),
			expected: "5",
		},
		{
			name:        "invalid expression",
			stmt:        "InvalidFunction(10, 5)",
			env:         NewEnv(),
			expectError: true,
		},
		{
			name:        "division by zero",
			stmt:        "Div(10, 0)",
			env:         NewEnv(),
			expectError: true,
		},
		{
			name:        "non-decimal result",
			stmt:        "GT(10, 5)",
			env:         NewEnv(),
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvalDecimal(tt.stmt, tt.env)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				expected, _ := decimal.NewFromString(tt.expected)
				assert.True(t, expected.Equal(result), "expected %s, got %s", expected.String(), result.String())
			}

			tt.env.Release()
		})
	}
}

func TestNewEnvAndRelease(t *testing.T) {
	t.Run("NewEnv creates environment with all functions", func(t *testing.T) {
		env := NewEnv()
		defer env.Release()

		for key := range keys {
			_, ok := env[key]
			assert.True(t, ok, "Environment should contain function %s", key)
		}
	})

	t.Run("Release cleans up environment", func(t *testing.T) {
		env := NewEnv()
		env["customKey"] = "customValue"

		// Verify custom key exists
		assert.Contains(t, env, "customKey")
		env.Release()

		// Get a new environment from pool to verify cleanup
		env2 := NewEnv()
		defer env2.Release()

		// Custom key should not be present in reused environment
		assert.NotContains(t, env2, "customKey")

		// But default functions should still be there
		assert.Contains(t, env2, "Add")
	})

	t.Run("multiple environments work independently", func(t *testing.T) {
		env1 := NewEnv()
		env2 := NewEnv()

		env1["test1"] = "value1"
		env2["test2"] = "value2"

		assert.Contains(t, env1, "test1")
		assert.NotContains(t, env1, "test2")
		assert.Contains(t, env2, "test2")
		assert.NotContains(t, env2, "test1")

		env1.Release()
		env2.Release()
	})
}

func TestEnvPooling(t *testing.T) {
	t.Run("environment reuse through pool", func(t *testing.T) {
		// Get an environment and add a custom key that should be cleaned up
		env1 := NewEnv()
		env1["shouldBeRemoved"] = "test"
		env1.Release()

		// Get another environment - should be clean
		env2 := NewEnv()
		defer env2.Release()

		assert.NotContains(t, env2, "shouldBeRemoved")
		assert.Contains(t, env2, "Add") // Default functions should still be there
	})
}

// Benchmark tests to ensure performance
func BenchmarkNewEnv(b *testing.B) {
	for i := 0; i < b.N; i++ {
		env := NewEnv()
		env.Release()
	}
}

func BenchmarkAdd(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = Add(123.45, 67.89)
	}
}

func BenchmarkEvalDecimal(b *testing.B) {
	env := NewEnv()
	defer env.Release()

	for i := 0; i < b.N; i++ {
		_, _ = EvalDecimal("Add(123.45, 67.89)", env)
	}
}
