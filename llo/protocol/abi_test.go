package protocol

import (
	"math/big"
	"testing"

	"github.com/goccy/go-json"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-data-streams/llo/ubig"
)

func TestSolidityInt_Check(t *testing.T) {
	pow2 := func(n uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), n) }
	sub := func(a *big.Int, b int64) *big.Int { return new(big.Int).Sub(a, big.NewInt(b)) }
	neg := func(a *big.Int) *big.Int { return new(big.Int).Neg(a) }

	for _, tc := range []struct {
		typ string
		v   *big.Int
		err string
	}{
		{"uint8", big.NewInt(0), ""},
		{"uint8", big.NewInt(255), ""},
		{"uint8", big.NewInt(256), "value 256 out of range for type uint8"},
		{"uint8", big.NewInt(-1), "negative value provided for unsigned type uint8"},
		{"int8", big.NewInt(127), ""},
		{"int8", big.NewInt(-128), ""},
		{"int8", big.NewInt(128), "out of range for type int8"},
		{"int8", big.NewInt(-129), "out of range for type int8"},
		{"uint256", sub(pow2(256), 1), ""},
		{"uint256", pow2(256), "out of range for type uint256"},
		{"int192", sub(pow2(191), 1), ""},
		{"int192", pow2(191), "out of range for type int192"},
		{"int192", neg(pow2(191)), ""},
		{"int192", sub(neg(pow2(191)), 1), "out of range for type int192"},
		{"uint7", big.NewInt(0), "invalid Solidity type: uint7"},
		{"int264", big.NewInt(0), "invalid Solidity type: int264"},
		{"bytes0", big.NewInt(0), "invalid Solidity type: bytes0"},
		{"bool", big.NewInt(0), "invalid Solidity type: bool"},
	} {
		t.Run(tc.typ+"/"+tc.v.String(), func(t *testing.T) {
			err := CheckSolidityInt(tc.v, tc.typ)
			if tc.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.err)
			}
		})
	}
}

func TestABIElement_Scale(t *testing.T) {
	d := decimal.RequireFromString("-1.239")
	require.Equal(t, big.NewInt(-1), ABIElement{}.Scale(d), "nil multiplier is 1 and truncates toward zero")
	require.Equal(t, big.NewInt(-123), ABIElement{Multiplier: ubig.NewI(100)}.Scale(d))
	require.Equal(t, big.NewInt(123), ABIElement{Multiplier: ubig.NewI(-100)}.Scale(d))
}

func TestABISpec_UnmarshalJSON(t *testing.T) {
	var single ABISpec
	require.NoError(t, json.Unmarshal([]byte(`{"type":"int192","multiplier":"100","expression":"s1"}`), &single))
	require.Equal(t, ABISpec{{Type: "int192", Multiplier: ubig.NewI(100)}}, single)

	var composite ABISpec
	require.NoError(t, json.Unmarshal([]byte(`[{"type":"uint64"},{"type":"int192"}]`), &composite))
	require.Equal(t, ABISpec{{Type: "uint64"}, {Type: "int192"}}, composite)

	var bad ABISpec
	require.Error(t, json.Unmarshal([]byte(`"int192"`), &bad))
}

func TestABISpec_CheckValue(t *testing.T) {
	dec := func(s string) *Decimal { return ToDecimal(decimal.RequireFromString(s)) }
	uint8Spec := ABISpec{{Type: "uint8"}}

	require.NoError(t, uint8Spec.CheckValue(dec("255.9"), false))
	require.ErrorContains(t, uint8Spec.CheckValue(dec("256"), false), "out of range")
	require.ErrorContains(t, uint8Spec.CheckValue(dec("-1"), false), "negative value")
	require.ErrorContains(t, ABISpec{{Type: "uint8", Multiplier: ubig.NewI(10)}}.CheckValue(dec("25.6"), false), "out of range")
	require.ErrorContains(t, uint8Spec.CheckValue((*Decimal)(nil), false), "non-nil")
	require.ErrorContains(t, uint8Spec.CheckValue(&Quote{}, false), "unhandled type")
	require.ErrorContains(t, ABISpec{{Type: "uint8"}, {Type: "uint8"}}.CheckValue(dec("1"), false), "exactly one encoder")

	// Only packed encoding accepts bytes0, and then for any value.
	zero := ABISpec{{Type: ABIZeroBytes}}
	require.NoError(t, zero.CheckValue(dec("-1e100"), true))
	require.ErrorContains(t, zero.CheckValue(dec("1"), false), "invalid Solidity type")

	ts := ABISpec{{Type: "uint64"}, {Type: "int8"}}
	require.NoError(t, ts.CheckValue(&TimestampedStreamValue{ObservedAtNanoseconds: 1_700_000_000_000_000_000, StreamValue: dec("-128")}, false))
	require.ErrorContains(t, ts.CheckValue(&TimestampedStreamValue{ObservedAtNanoseconds: 1, StreamValue: dec("128")}, false), "nested value")
	require.ErrorContains(t, ABISpec{{Type: "uint32"}, {Type: "int8"}}.CheckValue(&TimestampedStreamValue{ObservedAtNanoseconds: 1_700_000_000_000_000_000, StreamValue: dec("1")}, false), "observation timestamp")
	require.ErrorContains(t, ts.CheckValue(&TimestampedStreamValue{ObservedAtNanoseconds: 1}, false), "nested value must be")
	require.ErrorContains(t, uint8Spec.CheckValue(&TimestampedStreamValue{ObservedAtNanoseconds: 1, StreamValue: dec("1")}, false), "exactly two encoders")
}
