package protocol

import (
	reflect "reflect"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_TimestampedStreamValue_MarshalBinary(t *testing.T) {
	sv := &TimestampedStreamValue{
		ObservedAtNanoseconds: 123,
		StreamValue:           ToDecimal(decimal.NewFromFloat(456.548)),
	}
	b, err := sv.MarshalBinary()
	require.NoError(t, err)
	require.NotNil(t, b)

	sv2 := &TimestampedStreamValue{}
	err = sv2.UnmarshalBinary(b)
	require.NoError(t, err)
	require.Equal(t, sv, sv2)
}

func Test_Decimal_ExponentBound(t *testing.T) {
	// A decimal with an unbounded exponent is only 6 bytes on the wire but
	// costs hundreds of MB to compare during aggregation, so it must be
	// rejected at the decode boundary.
	craft := func(exp int32) []byte {
		b, err := decimal.New(1, exp).MarshalBinary()
		require.NoError(t, err)
		return b
	}

	t.Run("UnmarshalBinary", func(t *testing.T) {
		t.Run("accepts exponents within the bound", func(t *testing.T) {
			for _, exp := range []int32{0, 18, -18, MaxDecimalExponent, -MaxDecimalExponent} {
				var d Decimal
				require.NoError(t, d.UnmarshalBinary(craft(exp)))
				assert.Equal(t, exp, d.Decimal().Exponent())
			}
		})
		t.Run("rejects exponents outside the bound", func(t *testing.T) {
			for _, exp := range []int32{MaxDecimalExponent + 1, -MaxDecimalExponent - 1, 1_000_000_000, -1_000_000_000} {
				d := Decimal(decimal.NewFromInt(42))
				err := d.UnmarshalBinary(craft(exp))
				require.ErrorIs(t, err, ErrDecimalExponentOutOfRange)
				// The receiver must be left untouched by a rejected decode
				assert.Equal(t, "42", d.String())
			}
		})
		t.Run("returns ErrNilStreamValue on nil receiver", func(t *testing.T) {
			var d *Decimal
			require.ErrorIs(t, d.UnmarshalBinary(craft(0)), ErrNilStreamValue)
		})
	})

	t.Run("UnmarshalText", func(t *testing.T) {
		var d Decimal
		require.NoError(t, d.UnmarshalText([]byte("1e-1000")))

		err := d.UnmarshalText([]byte("1e-10000000"))
		require.ErrorIs(t, err, ErrDecimalExponentOutOfRange)
	})

	t.Run("UnmarshalProtoStreamValue", func(t *testing.T) {
		_, err := UnmarshalProtoStreamValue(&LLOStreamValue{Type: LLOStreamValue_Decimal, Value: craft(-1_000_000_000)})
		require.ErrorIs(t, err, ErrDecimalExponentOutOfRange)
	})

	t.Run("nested in TimestampedStreamValue", func(t *testing.T) {
		sv := &TimestampedStreamValue{
			ObservedAtNanoseconds: 123,
			StreamValue:           ToDecimal(decimal.New(1, -1_000_000_000)),
		}
		b, err := sv.MarshalBinary()
		require.NoError(t, err)

		err = (&TimestampedStreamValue{}).UnmarshalBinary(b)
		require.ErrorIs(t, err, ErrDecimalExponentOutOfRange)
	})

	t.Run("Quote", func(t *testing.T) {
		q := &Quote{
			Bid:       decimal.New(1, -1_000_000_000),
			Benchmark: decimal.NewFromInt(2),
			Ask:       decimal.NewFromInt(3),
		}
		b, err := q.MarshalBinary()
		require.NoError(t, err)

		err = (&Quote{}).UnmarshalBinary(b)
		require.ErrorIs(t, err, ErrDecimalExponentOutOfRange)
	})
}

func Test_TimestampedStreamValue_Properties(t *testing.T) {
	properties := gopter.NewProperties(nil)

	properties.Property("Encode/Decode", prop.ForAll(
		func(sv TimestampedStreamValue) bool {
			b, err := sv.MarshalBinary()
			require.NoError(t, err)
			require.NotNil(t, b)

			sv2 := TimestampedStreamValue{}
			err = (&sv2).UnmarshalBinary(b)
			require.NoError(t, err)
			return assert.Equal(t, sv, sv2)
		},
		gen.StrictStruct(reflect.TypeOf(&TimestampedStreamValue{}), map[string]gopter.Gen{
			"ObservedAtNanoseconds": gen.UInt64(),
			"StreamValue":           genStreamValue(false),
		}),
	))

	properties.TestingRun(t)
}
