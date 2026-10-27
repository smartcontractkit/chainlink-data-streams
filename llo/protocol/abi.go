package protocol

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/goccy/go-json"
	"github.com/shopspring/decimal"

	"github.com/smartcontractkit/chainlink-data-streams/llo/ubig"
)

// ABIZeroBytes is the ABI type that packed encoding emits as no bytes, so any
// value is encodable under it. Padded encoding does not accept it.
const ABIZeroBytes = "bytes0"

// SolidityInt is a Solidity integer type, uintN or intN with N a multiple of 8
// up to 256.
type SolidityInt struct {
	Name   string
	Signed bool
	Bits   uint
	min    *big.Int
	max    *big.Int
}

// solidityInts holds every Solidity integer type with its bounds computed once,
// so range checks in the state transition do no parsing.
var solidityInts = func() map[string]SolidityInt {
	m := make(map[string]SolidityInt, 64)
	one := big.NewInt(1)
	for bits := uint(8); bits <= 256; bits += 8 {
		name := fmt.Sprintf("uint%d", bits)
		m[name] = SolidityInt{
			Name: name,
			Bits: bits,
			min:  new(big.Int),
			max:  new(big.Int).Sub(new(big.Int).Lsh(one, bits), one),
		}
		name = fmt.Sprintf("int%d", bits)
		half := new(big.Int).Lsh(one, bits-1)
		m[name] = SolidityInt{
			Name:   name,
			Signed: true,
			Bits:   bits,
			min:    new(big.Int).Neg(half),
			max:    new(big.Int).Sub(half, one),
		}
	}
	return m
}()

// ParseSolidityInt returns the Solidity integer type named t.
func ParseSolidityInt(t string) (SolidityInt, error) {
	s, ok := solidityInts[t]
	if !ok {
		return SolidityInt{}, fmt.Errorf("invalid Solidity type: %s", t)
	}
	return s, nil
}

// Check returns an error when v is outside the range of s.
func (s SolidityInt) Check(v *big.Int) error {
	if !s.Signed && v.Sign() < 0 {
		return fmt.Errorf("negative value provided for unsigned type %s", s.Name)
	}
	if v.Cmp(s.min) < 0 || v.Cmp(s.max) > 0 {
		return fmt.Errorf("value %s out of range for type %s", v.String(), s.Name)
	}
	return nil
}

// CheckSolidityInt returns an error when t is not a Solidity integer type or v
// is outside its range.
func CheckSolidityInt(v *big.Int, t string) error {
	s, err := ParseSolidityInt(t)
	if err != nil {
		return err
	}
	return s.Check(v)
}

// ABIElement is the part of an ABI encoder spec that decides whether a value
// can be encoded: its type and the multiplier applied before encoding.
type ABIElement struct {
	Type       string    `json:"type"`
	Multiplier *ubig.Big `json:"multiplier"`
}

// Scale applies the multiplier to d and truncates toward zero, which is the
// integer the codecs encode. A nil multiplier is 1. A negative one flips the
// sign.
func (e ABIElement) Scale(d decimal.Decimal) *big.Int {
	if e.Multiplier == nil {
		return d.BigInt()
	}
	return d.Mul(decimal.NewFromBigInt(e.Multiplier.ToInt(), 0)).BigInt()
}

// check returns an error when the scaled d does not fit the element's type.
// packed allows ABIZeroBytes, which packed encoding emits as nothing.
func (e ABIElement) check(d decimal.Decimal, packed bool) error {
	if packed && e.Type == ABIZeroBytes {
		return nil
	}
	return CheckSolidityInt(e.Scale(d), e.Type)
}

// ABISpec is one entry of an ABI opt: a single element for a scalar value, or
// a list of elements for a composite value. JSON accepts either an object or an
// array of objects.
type ABISpec []ABIElement

// UnmarshalJSON accepts a single element or an array of elements.
func (s *ABISpec) UnmarshalJSON(data []byte) error {
	var elems []ABIElement
	if err := json.Unmarshal(data, &elems); err == nil {
		*s = elems
		return nil
	}
	var elem ABIElement
	if err := json.Unmarshal(data, &elem); err != nil {
		return fmt.Errorf("failed to unmarshal ABI spec: %w", err)
	}
	*s = ABISpec{elem}
	return nil
}

// CheckValue returns an error when the codecs cannot encode sv under s. It
// follows the encoders' layout: a Decimal takes one element, a
// TimestampedStreamValue takes two, the observation time and then the nested
// Decimal. packed selects packed encoding, which also accepts ABIZeroBytes.
func (s ABISpec) CheckValue(sv StreamValue, packed bool) error {
	switch v := sv.(type) {
	case *Decimal:
		if len(s) != 1 {
			return fmt.Errorf("expected exactly one encoder; got: %d", len(s))
		}
		if v == nil {
			return errors.New("expected non-nil *Decimal")
		}
		return s[0].check(v.Decimal(), packed)
	case *TimestampedStreamValue:
		if len(s) != 2 {
			return fmt.Errorf("expected exactly two encoders for *protocol.TimestampedStreamValue; got: %d", len(s))
		}
		if v == nil {
			return errors.New("expected non-nil *TimestampedStreamValue")
		}
		if err := s[0].check(decimal.NewFromBigInt(new(big.Int).SetUint64(v.ObservedAtNanoseconds), 0), packed); err != nil {
			return fmt.Errorf("observation timestamp: %w", err)
		}
		d, ok := v.StreamValue.(*Decimal)
		if !ok || d == nil {
			return fmt.Errorf("nested value must be a non-nil *protocol.Decimal; got: %T", v.StreamValue)
		}
		if err := s[1].check(d.Decimal(), packed); err != nil {
			return fmt.Errorf("nested value: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unhandled type; supported types are: *protocol.Decimal or *protocol.TimestampedStreamValue; got: %T", sv)
	}
}
