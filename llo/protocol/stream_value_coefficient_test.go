package protocol

import (
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// bigDecimal returns a decimal whose coefficient is exactly bits long, with an
// exponent well inside MaxDecimalExponent so the exponent bound cannot be what
// rejects it.
func bigDecimal(bits int) decimal.Decimal {
	coefficient := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	return decimal.NewFromBigInt(coefficient, -2)
}

func protoOf(t *testing.T, sv StreamValue) *LLOStreamValue {
	t.Helper()
	pb, err := StreamValueToProto(sv)
	require.NoError(t, err)
	return pb
}

func Test_UnmarshalObservedProtoStreamValue_CoefficientBound(t *testing.T) {
	atLimit := bigDecimal(MaxDecimalCoefficientBits)
	over := bigDecimal(MaxDecimalCoefficientBits + 1)

	// The exponent bound is not what does the work here: both values are well
	// inside it, which is the whole point of adding a coefficient bound.
	require.NoError(t, checkDecimalExponent(atLimit))
	require.NoError(t, checkDecimalExponent(over))

	t.Run("decimal", func(t *testing.T) {
		_, err := UnmarshalObservedProtoStreamValue(protoOf(t, ToDecimal(atLimit)))
		require.NoError(t, err)

		_, err = UnmarshalObservedProtoStreamValue(protoOf(t, ToDecimal(over)))
		require.ErrorIs(t, err, ErrDecimalCoefficientOutOfRange)
	})

	t.Run("quote rejects any of its three fields", func(t *testing.T) {
		small := decimal.NewFromInt(1)
		for _, q := range []*Quote{
			{Bid: over, Benchmark: small, Ask: small},
			{Bid: small, Benchmark: over, Ask: small},
			{Bid: small, Benchmark: small, Ask: over},
		} {
			_, err := UnmarshalObservedProtoStreamValue(protoOf(t, q))
			require.ErrorIs(t, err, ErrDecimalCoefficientOutOfRange)
		}
		_, err := UnmarshalObservedProtoStreamValue(protoOf(t, &Quote{Bid: atLimit, Benchmark: atLimit, Ask: atLimit}))
		require.NoError(t, err)
	})

	t.Run("timestamped values are checked through the wrapper", func(t *testing.T) {
		_, err := UnmarshalObservedProtoStreamValue(protoOf(t, &TimestampedStreamValue{
			ObservedAtNanoseconds: 1,
			StreamValue:           ToDecimal(over),
		}))
		require.ErrorIs(t, err, ErrDecimalCoefficientOutOfRange)

		_, err = UnmarshalObservedProtoStreamValue(protoOf(t, &TimestampedStreamValue{
			ObservedAtNanoseconds: 1,
			StreamValue:           ToDecimal(atLimit),
		}))
		require.NoError(t, err)
	})

	t.Run("nesting is bounded during unmarshal", func(t *testing.T) {
		nested := func(levels int) *LLOStreamValue {
			var sv StreamValue = ToDecimal(decimal.NewFromInt(1))
			for range levels {
				sv = &TimestampedStreamValue{ObservedAtNanoseconds: 1, StreamValue: sv}
			}
			return protoOf(t, sv)
		}

		// The bound is enforced by unmarshal itself, not by a check over the
		// decoded value: the recursion is inside unmarshalling, and each level
		// re-slices the nested bytes, so a post-hoc check runs too late.
		_, err := UnmarshalProtoStreamValue(nested(MaxStreamValueNesting + 1))
		require.ErrorIs(t, err, ErrStreamValueNestingTooDeep)
		_, err = UnmarshalObservedProtoStreamValue(nested(MaxStreamValueNesting + 1))
		require.ErrorIs(t, err, ErrStreamValueNestingTooDeep)
		_, err = UnmarshalProtoStreamValue(nested(MaxStreamValueNesting))
		require.NoError(t, err)

		// Same recursion, text encoding.
		tsv := &TimestampedStreamValue{ObservedAtNanoseconds: 1, StreamValue: ToDecimal(decimal.NewFromInt(1))}
		for range MaxStreamValueNesting {
			tsv = &TimestampedStreamValue{ObservedAtNanoseconds: 1, StreamValue: tsv}
		}
		ttsv, err := NewTypedTextStreamValue(tsv)
		require.NoError(t, err)
		_, err = UnmarshalTypedTextStreamValue(&ttsv)
		require.ErrorIs(t, err, ErrStreamValueNestingTooDeep)
	})

	t.Run("stored-state decode is deliberately unchecked", func(t *testing.T) {
		// Phase 1 bounds observations only. Rejecting a value already persisted
		// would fail decode on every upgraded oracle at once, so this path must
		// keep accepting it until the change is coordinated across versions.
		sv, err := UnmarshalProtoStreamValue(protoOf(t, ToDecimal(over)))
		require.NoError(t, err)
		require.NotNil(t, sv)
	})
}

// TestDecimalCoefficientBoundFitsHistoryRecord keeps MaxDecimalCoefficientBits
// and MaxHistoryRecordBytes consistent. Observation decode admits a value and
// history then has to store it, so a bound the other refuses means a value the
// round agreed on leaves a gap in the series. MaxDecimalCoefficientBits is
// derived from this relationship, so raising either constant without the other
// fails here.
func TestDecimalCoefficientBoundFitsHistoryRecord(t *testing.T) {
	// A timestamped quote is the largest shape a stream value takes: three
	// coefficients plus the wrapper's timestamp.
	worst := func(bits int) StreamValue {
		d := bigDecimal(bits)
		return &TimestampedStreamValue{
			ObservedAtNanoseconds: 1_700_000_000_000_000_000,
			StreamValue:           &Quote{Bid: d, Benchmark: d, Ask: d},
		}
	}

	size, err := historyRecordSize(1_700_000_000_000_000_000, worst(MaxDecimalCoefficientBits))
	require.NoError(t, err)
	require.LessOrEqual(t, size, MaxHistoryRecordBytes,
		"a timestamped quote at the coefficient bound (%d B) must fit a history record", size)

	// And the bound is the largest that does fit: the next step up does not.
	// Without this the two constants could drift apart silently, with the
	// coefficient bound quietly stopping short of what history can hold.
	oversize, err := historyRecordSize(1_700_000_000_000_000_000, worst(MaxDecimalCoefficientBits+32))
	require.NoError(t, err)
	require.Greater(t, oversize, MaxHistoryRecordBytes)
}
