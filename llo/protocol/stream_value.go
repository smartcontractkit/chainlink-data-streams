package protocol

import (
	"encoding"
	"errors"
	"fmt"
	"regexp"

	"github.com/goccy/go-json"

	"google.golang.org/protobuf/proto"

	"github.com/shopspring/decimal"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type StreamValue interface {
	// Binary marshaler/unmarshaler used for protobufs
	// Unmarshal should NOT panic on nil receiver, but instead return ErrNilStreamValue
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
	// TextMarshaler needed for JSON serialization and logging
	// Unmarshal should NOT panic on nil receiver, but instead return ErrNilStreamValue
	encoding.TextMarshaler
	encoding.TextUnmarshaler
	// Type is needed for proto serialization so we know how to unserialize it
	Type() LLOStreamValue_Type
}

var (
	ErrNilStreamValue = errors.New("nil stream value")
	// ErrDecimalExponentOutOfRange is returned when a decimal decoded from an
	// untrusted source carries an exponent outside ±MaxDecimalExponent. A
	// well-behaved node never encodes such a value; accepting one would let a
	// single byzantine node force unbounded rescale work on every honest node.
	ErrDecimalExponentOutOfRange = errors.New("decimal exponent out of range")
	// ErrDecimalCoefficientOutOfRange is returned when a decimal carried by an
	// observation has a coefficient longer than MaxDecimalCoefficientBits. The
	// exponent bound says nothing about coefficient length, so without this a
	// single stream value is unbounded in bytes.
	ErrDecimalCoefficientOutOfRange = errors.New("decimal coefficient out of range")
	// ErrStreamValueNestingTooDeep is returned when a stream value nests deeper
	// than MaxStreamValueNesting.
	ErrStreamValueNestingTooDeep = errors.New("stream value nesting too deep")
)

// UnmarshalObservedProtoStreamValue decodes a stream value that arrived in a
// peer's observation, and additionally enforces the bounds that only observed
// values are held to today.
//
// Observation decode is the one untrusted entry point where a rejection is
// cheap: it is a pure function of the observation bytes, so every oracle reaches
// the same verdict, and both callers already discard an individual observation
// that fails to decode rather than failing the round (see
// decodeObservations). The same bound applied to values decoded from stored
// state -- a v3.0 outcome, a v3.1 r/agg record -- would instead reject what is
// already persisted and fail decode on every upgraded oracle at once, so those
// paths keep using UnmarshalProtoStreamValue until that change can be
// coordinated across versions.
//
// Bounding observations bounds everything written from them going forward.
func UnmarshalObservedProtoStreamValue(enc *LLOStreamValue) (StreamValue, error) {
	sv, err := UnmarshalProtoStreamValue(enc)
	if err != nil {
		return nil, err
	}
	if err := checkObservedStreamValue(sv, 0); err != nil {
		return nil, err
	}
	return sv, nil
}

// checkObservedStreamValue applies the observation-only bounds to every decimal
// a stream value carries, at any nesting depth.
func checkObservedStreamValue(sv StreamValue, depth int) error {
	if depth > MaxStreamValueNesting {
		return fmt.Errorf("%w: got more than %d levels", ErrStreamValueNestingTooDeep, MaxStreamValueNesting)
	}
	switch v := sv.(type) {
	case nil:
		return nil
	case *Decimal:
		return checkDecimalCoefficient(v.Decimal())
	case *Quote:
		if v == nil {
			return nil
		}
		for _, d := range []decimal.Decimal{v.Bid, v.Benchmark, v.Ask} {
			if err := checkDecimalCoefficient(d); err != nil {
				return err
			}
		}
		return nil
	case *TimestampedStreamValue:
		if v == nil {
			return nil
		}
		return checkObservedStreamValue(v.StreamValue, depth+1)
	default:
		// An unknown type carries no decimal this function knows how to reach.
		// UnmarshalProtoStreamValue rejects types it does not recognize, so this
		// is unreachable rather than a silent pass.
		return nil
	}
}

// checkDecimalCoefficient bounds the coefficient length of a decimal carried by
// an observation. See MaxDecimalCoefficientBits.
func checkDecimalCoefficient(d decimal.Decimal) error {
	if bits := d.Coefficient().BitLen(); bits > MaxDecimalCoefficientBits {
		return fmt.Errorf("%w: got %d bits, expected <= %d", ErrDecimalCoefficientOutOfRange, bits, MaxDecimalCoefficientBits)
	}
	return nil
}

// checkDecimalExponent bounds the exponent of a decimal decoded from an
// untrusted source. See MaxDecimalExponent.
func checkDecimalExponent(d decimal.Decimal) error {
	exp := d.Exponent()
	if exp > MaxDecimalExponent || exp < -MaxDecimalExponent {
		return fmt.Errorf("%w: got %d, expected absolute value <= %d", ErrDecimalExponentOutOfRange, exp, MaxDecimalExponent)
	}
	return nil
}

// unmarshalBinaryDecimal decodes a decimal and enforces the exponent bound
// before writing it to d.
func unmarshalBinaryDecimal(d *decimal.Decimal, data []byte) error {
	var decoded decimal.Decimal
	if err := decoded.UnmarshalBinary(data); err != nil {
		return err
	}
	if err := checkDecimalExponent(decoded); err != nil {
		return err
	}
	*d = decoded
	return nil
}

// unmarshalTextDecimal decodes a decimal and enforces the exponent bound
// before writing it to d.
func unmarshalTextDecimal(d *decimal.Decimal, data []byte) error {
	var decoded decimal.Decimal
	if err := decoded.UnmarshalText(data); err != nil {
		return err
	}
	if err := checkDecimalExponent(decoded); err != nil {
		return err
	}
	*d = decoded
	return nil
}

func UnmarshalProtoStreamValue(enc *LLOStreamValue) (sv StreamValue, err error) {
	if enc == nil {
		// Shouldn't ever happen except from byzantine node, but we must not panic
		return nil, ErrNilStreamValue
	}
	switch enc.Type {
	case LLOStreamValue_Quote:
		sv = new(Quote)
	case LLOStreamValue_Decimal:
		sv = new(Decimal)
	case LLOStreamValue_TimestampedStreamValue:
		sv = new(TimestampedStreamValue)
	default:
		return nil, fmt.Errorf("cannot unmarshal protobuf stream value; unknown StreamValueType %d", enc.Type)
	}
	if err := sv.UnmarshalBinary(enc.Value); err != nil {
		return nil, err
	}
	return sv, nil
}

func NewTypedTextStreamValue(sv StreamValue) (TypedTextStreamValue, error) {
	if sv == nil {
		return TypedTextStreamValue{}, ErrNilStreamValue
	}
	b, err := sv.MarshalText()
	if err != nil {
		return TypedTextStreamValue{}, fmt.Errorf("failed to encode StreamValue: %w", err)
	}
	return TypedTextStreamValue{
		Type:                  sv.Type(),
		SerializedStreamValue: string(b),
	}, nil
}

type TypedTextStreamValue struct {
	Type                  LLOStreamValue_Type `json:"t"`
	SerializedStreamValue string              `json:"v"`
}

func UnmarshalTypedTextStreamValue(enc *TypedTextStreamValue) (StreamValue, error) {
	if enc == nil {
		// Shouldn't ever happen except from byzantine node, but we must not panic
		return nil, ErrNilStreamValue
	}
	var sv StreamValue
	switch enc.Type {
	case LLOStreamValue_Decimal:
		sv = new(Decimal)
	case LLOStreamValue_Quote:
		sv = new(Quote)
	case LLOStreamValue_TimestampedStreamValue:
		sv = new(TimestampedStreamValue)
	default:
		return nil, fmt.Errorf("unknown StreamValueType %d", enc.Type)
	}
	if err := (sv).UnmarshalText([]byte(enc.SerializedStreamValue)); err != nil {
		return nil, err
	}
	return sv, nil
}

func Decode(value StreamValue, data []byte) error {
	return value.UnmarshalBinary(data)
}

// Values for a set of streams, e.g. "eth-usd", "link-usd", "eur-chf" etc
// StreamIDs are uint32
type StreamValues map[llotypes.StreamID]StreamValue
type StreamAggregates map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue

// Quote implements StreamValue for a {Bid, Benchmark, Ask} tuple

type Quote struct {
	Bid       decimal.Decimal
	Benchmark decimal.Decimal
	Ask       decimal.Decimal
}

var _ StreamValue = (*Quote)(nil)

func (v *Quote) MarshalBinary() (b []byte, err error) {
	if v == nil {
		return nil, ErrNilStreamValue
	}
	q := LLOStreamValueQuote{}
	q.Bid, err = v.Bid.MarshalBinary()
	if err != nil {
		return nil, err
	}
	q.Benchmark, err = v.Benchmark.MarshalBinary()
	if err != nil {
		return nil, err
	}
	q.Ask, err = v.Ask.MarshalBinary()
	if err != nil {
		return nil, err
	}
	// Deterministic: these bytes are compared across oracles. See
	// deterministicMarshal.
	return deterministicMarshal.Marshal(&q)
}

func (v *Quote) UnmarshalBinary(data []byte) error {
	q := new(LLOStreamValueQuote)
	if err := proto.Unmarshal(data, q); err != nil {
		return err
	}
	if err := unmarshalBinaryDecimal(&v.Bid, q.Bid); err != nil {
		return err
	}
	if err := unmarshalBinaryDecimal(&v.Benchmark, q.Benchmark); err != nil {
		return err
	}
	return unmarshalBinaryDecimal(&v.Ask, q.Ask)
}

func (v *Quote) MarshalText() ([]byte, error) {
	if v == nil {
		return nil, ErrNilStreamValue
	}
	return []byte(fmt.Sprintf("Q{Bid: %s, Benchmark: %s, Ask: %s}", v.Bid.String(), v.Benchmark.String(), v.Ask.String())), nil
}

var quoteRegex = regexp.MustCompile(`Q\{Bid: ([0-9.]+), Benchmark: ([0-9.]+), Ask: ([0-9.]+)\}`)

func (v *Quote) UnmarshalText(data []byte) error {
	if v == nil {
		return ErrNilStreamValue
	}

	matches := quoteRegex.FindStringSubmatch(string(data))
	if len(matches) != 4 {
		return fmt.Errorf("unexpected input for quote, expected format Q{Bid: <bid>, Benchmark: <benchmark>, Ask: <ask>}, got %s", string(data))
	}

	bid := matches[1]
	benchmark := matches[2]
	ask := matches[3]
	if err := unmarshalTextDecimal(&v.Bid, []byte(bid)); err != nil {
		return err
	}
	if err := unmarshalTextDecimal(&v.Benchmark, []byte(benchmark)); err != nil {
		return err
	}
	return unmarshalTextDecimal(&v.Ask, []byte(ask))
}

func (v *Quote) Type() LLOStreamValue_Type {
	return LLOStreamValue_Quote
}

func (v *Quote) IsValid() bool {
	return v.Bid.Cmp(v.Benchmark) <= 0 && v.Benchmark.Cmp(v.Ask) <= 0
}

// Decimal implements StreamValue for a simple decimal value
// Use this also for integers

type Decimal decimal.Decimal

var _ StreamValue = (*Decimal)(nil)

func ToDecimal(d decimal.Decimal) *Decimal {
	return (*Decimal)(&d)
}

func (v *Decimal) Decimal() decimal.Decimal {
	return decimal.Decimal(*v)
}

func (v *Decimal) MarshalBinary() ([]byte, error) {
	if v == nil {
		return nil, ErrNilStreamValue
	}
	return decimal.Decimal(*v).MarshalBinary()
}

func (v *Decimal) UnmarshalBinary(data []byte) error {
	if v == nil {
		return ErrNilStreamValue
	}
	return unmarshalBinaryDecimal((*decimal.Decimal)(v), data)
}

func (v *Decimal) String() string {
	return decimal.Decimal(*v).String()
}

func (v *Decimal) MarshalText() ([]byte, error) {
	if v == nil {
		return nil, ErrNilStreamValue
	}
	return []byte(v.String()), nil
}

func (v *Decimal) UnmarshalText(data []byte) error {
	if v == nil {
		return ErrNilStreamValue
	}
	return unmarshalTextDecimal((*decimal.Decimal)(v), data)
}

func (v *Decimal) Type() LLOStreamValue_Type {
	return LLOStreamValue_Decimal
}

// TimestampedStreamValue is a StreamValue with an associated timestamp
type TimestampedStreamValue struct {
	ObservedAtNanoseconds uint64      `json:"observedAtNanoseconds"`
	StreamValue           StreamValue `json:"streamValue"`
}

var _ StreamValue = (*TimestampedStreamValue)(nil)

func (v *TimestampedStreamValue) MarshalBinary() ([]byte, error) {
	if v == nil {
		return nil, ErrNilStreamValue
	}
	t := LLOTimestampedStreamValue{}
	t.ObservedAtNanoseconds = v.ObservedAtNanoseconds
	if v.StreamValue == nil {
		return nil, ErrNilStreamValue
	}
	sv, err := v.StreamValue.MarshalBinary()
	if err != nil {
		return nil, err
	}
	t.StreamValue = &LLOStreamValue{
		Type:  v.StreamValue.Type(),
		Value: sv,
	}
	// Deterministic: these bytes are compared across oracles. See
	// deterministicMarshal.
	return deterministicMarshal.Marshal(&t)
}

func (v *TimestampedStreamValue) UnmarshalBinary(data []byte) error {
	t := new(LLOTimestampedStreamValue)
	if err := proto.Unmarshal(data, t); err != nil {
		return err
	}
	v.ObservedAtNanoseconds = t.ObservedAtNanoseconds
	sv, err := UnmarshalProtoStreamValue(t.StreamValue)
	if err != nil {
		return err
	}
	v.StreamValue = sv
	return nil
}

func (v *TimestampedStreamValue) MarshalText() ([]byte, error) {
	if v == nil {
		return nil, ErrNilStreamValue
	}
	serializedSv, err := v.StreamValue.MarshalText()
	if err != nil {
		return nil, err
	}
	t := TypedTextStreamValue{
		Type:                  v.StreamValue.Type(),
		SerializedStreamValue: string(serializedSv),
	}
	serializedT, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("TSV{ObservedAtNanoseconds: %d, StreamValue: %s}", v.ObservedAtNanoseconds, serializedT)), nil
}

var timestampedStreamValueRegex = regexp.MustCompile(`^TSV\{ObservedAtNanoseconds: ([0-9]+), StreamValue: (.+)\}$`)

func (v *TimestampedStreamValue) UnmarshalText(data []byte) error {
	if v == nil {
		return ErrNilStreamValue
	}

	matches := timestampedStreamValueRegex.FindStringSubmatch(string(data))
	if len(matches) != 3 {
		return fmt.Errorf("unexpected input for timestamped stream value, expected format TSV{ObservedAt: <timestamp>, Value: <value>}, got %s", string(data))
	}

	timestamp := matches[1]
	serializedT := matches[2]
	if _, err := fmt.Sscanf(timestamp, "%d", &v.ObservedAtNanoseconds); err != nil {
		return fmt.Errorf("failed to parse timestamp: %w", err)
	}

	tSv := new(TypedTextStreamValue)
	if err := json.Unmarshal([]byte(serializedT), tSv); err != nil {
		return fmt.Errorf("failed to unmarshal text stream value: %w", err)
	}

	sv, err := UnmarshalTypedTextStreamValue(tSv)
	if err != nil {
		return fmt.Errorf("failed to unmarshal text stream value: %w", err)
	}
	v.StreamValue = sv
	return nil
}

func (v *TimestampedStreamValue) Type() LLOStreamValue_Type {
	return LLOStreamValue_TimestampedStreamValue
}
