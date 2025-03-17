package llo

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
