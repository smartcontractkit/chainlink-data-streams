package evm

import (
	"fmt"
	"math"
	"math/big"

	"github.com/goccy/go-json"

	"github.com/shopspring/decimal"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
	"github.com/smartcontractkit/chainlink-data-streams/llo/ubig"
)

// Extracts nanosecond timestamps as uint32 number of seconds
func ExtractTimestamps(report protocol.Report) (validAfterSeconds, observationTimestampSeconds uint32, err error) {
	vas := report.ValidAfterNanoseconds / 1e9
	ots := report.ObservationTimestampNanoseconds / 1e9
	if vas > math.MaxUint32 {
		err = fmt.Errorf("validAfterSeconds too large: %d", vas)
		return
	}
	if ots > math.MaxUint32 {
		err = fmt.Errorf("observationTimestampSeconds too large: %d", ots)
		return
	}
	return uint32(vas), uint32(ots), nil
}

// An ABIEncoder encodes exactly one stream value into a byte slice
type ABIEncoder struct {
	encoders []singleABIEncoder
}

func (a ABIEncoder) MarshalJSON() ([]byte, error) {
	if len(a.encoders) == 1 {
		return json.Marshal(a.encoders[0])
	}
	return json.Marshal(a.encoders)
}

func (a *ABIEncoder) UnmarshalJSON(data []byte) error {
	// Try to unmarshal into a slice
	var encoders []singleABIEncoder
	if err := json.Unmarshal(data, &encoders); err == nil {
		a.encoders = encoders
		return nil
	}

	// Try to unmarshal into a single encoder
	var encoder singleABIEncoder
	if err := json.Unmarshal(data, &encoder); err != nil {
		return fmt.Errorf("failed to unmarshal ABIEncoder: %w", err)
	}
	a.encoders = []singleABIEncoder{encoder}
	return nil
}

func (a ABIEncoder) EncodePacked(sv protocol.StreamValue) ([]byte, error) {
	switch v := sv.(type) {
	case *protocol.Decimal:
		if len(a.encoders) != 1 {
			return nil, fmt.Errorf("expected exactly one encoder; got: %d", len(a.encoders))
		}
		b, err := a.encoders[0].encodePacked(sv)
		if err != nil {
			return nil, fmt.Errorf("failed to encode stream value; %w", err)
		}
		return b, nil
	case *protocol.TimestampedStreamValue:
		if len(a.encoders) != 2 {
			return nil, fmt.Errorf("expected exactly two encoders for *protocol.TimestampedStreamValue; got: %d", len(a.encoders))
		}
		// encode as two packed values
		// <type0> observed_at_nanoseconds
		// <type1> stream_value
		encodedTimestamp, err := a.encoders[0].encodeUint64Packed(v.ObservedAtNanoseconds)
		if err != nil {
			return nil, fmt.Errorf("failed to encode timestamped stream value; %w", err)
		}
		encodedDecimal, err := a.encoders[1].encodePacked(v.StreamValue)
		if err != nil {
			return nil, fmt.Errorf("failed to encode nested stream value; %w", err)
		}
		return append(encodedTimestamp, encodedDecimal...), nil
	default:
		return nil, fmt.Errorf("unhandled type; supported types are: *protocol.Decimal or *protocol.TimestampedStreamValue; got: %T", sv)
	}
}

func (a ABIEncoder) EncodePadded(sv protocol.StreamValue) ([]byte, error) {
	switch v := sv.(type) {
	case *protocol.Decimal:
		if len(a.encoders) != 1 {
			return nil, fmt.Errorf("expected exactly one encoder; got: %d", len(a.encoders))
		}
		return a.encoders[0].encodeDecimalStreamValuePadded(v)
	case *protocol.TimestampedStreamValue:
		if len(a.encoders) != 2 {
			return nil, fmt.Errorf("expected exactly two encoders for *protocol.TimestampedStreamValue; got: %d", len(a.encoders))
		}
		// encode as two zero-padded 32 byte evm words:
		// uint256 observed_at_nanoseconds
		// uint256 stream_value
		encodedTimestamp, err := a.encoders[0].encodeUint64Padded(v.ObservedAtNanoseconds)
		if err != nil {
			return nil, fmt.Errorf("failed to encode timestamped stream value; %w", err)
		}
		var encodedDecimal []byte
		switch d := v.StreamValue.(type) {
		case *protocol.Decimal:
			var err error
			encodedDecimal, err = a.encoders[1].encodeDecimalStreamValuePadded(d)
			if err != nil {
				return nil, fmt.Errorf("failed to encode nested stream value; %w", err)
			}
		default:
			return nil, fmt.Errorf("unhandled type; supported nested types for *protocol.TimestampedStreamValue are: *protocol.Decimal; got: %T", d)
		}
		return append(encodedTimestamp, encodedDecimal...), nil
	default:
		return nil, fmt.Errorf("unhandled type; supported types are: *protocol.Decimal, *protocol.TimestampedStreamValue, or *protocol.Quote; got: %T", sv)
	}
}

type singleABIEncoder struct {
	// Type is the ABI type of the stream value. E.g. "uint192", "int256", "bool", "string" etc.
	// MANDATORY
	Type string `json:"type"`
	// Multiplier, if provided, will be multiplied with the stream value before
	// encoding.
	// OPTIONAL
	Multiplier *ubig.Big `json:"multiplier"`
	// Expression, if provided, will be evaluated and the result will be encoded.
	// OPTIONAL
	Expression string `json:"expression,omitempty"`
	// ExpressionStreamID, if provided, will be the stream ID of the stream value
	// that will be used to evaluate the expression.
	// OPTIONAL
	ExpressionStreamID llotypes.StreamID `json:"expressionStreamId,omitempty"`
}

// applyMultiplier scales d the same way protocol.ABIElement.Scale does, so the
// reportability check and the encoder range check the same integer.
func (a singleABIEncoder) applyMultiplier(d decimal.Decimal) *big.Int {
	return protocol.ABIElement{Type: a.Type, Multiplier: a.Multiplier}.Scale(d)
}

// EncodePadded uses standard ABI encoding to encode the stream value, padding
// result to a multiple of 32 bytes
func (a singleABIEncoder) EncodePadded(sv protocol.StreamValue) (b []byte, err error) {
	switch sv := sv.(type) {
	case *protocol.Decimal:
		b, err = a.encodeDecimalStreamValuePadded(sv)
	default:
		return nil, fmt.Errorf("unhandled type; supported types are: *protocol.Decimal; got: %T", sv)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to encode stream value %v (%T) with ABI type %q; %w", sv, sv, a.Type, err)
	}
	return b, nil
}

func (a singleABIEncoder) encodeUint64Padded(v uint64) (b []byte, err error) {
	return EncodePaddedBigInt(a.applyMultiplier(decimal.NewFromBigInt(new(big.Int).SetUint64(v), 0)), a.Type)
}

func (a singleABIEncoder) encodeDecimalStreamValuePadded(sv *protocol.Decimal) (b []byte, err error) {
	if sv == nil {
		return nil, fmt.Errorf("expected non-nil *Decimal; got: %v", sv)
	}
	return EncodePaddedBigInt(a.applyMultiplier(sv.Decimal()), a.Type)
}

func EncodePaddedBigInt(v *big.Int, t string) (b []byte, err error) {
	b, err = EncodePackedBigInt(v, t)
	if err != nil {
		return nil, fmt.Errorf("failed to encode stream value %v with ABI type %q; %w", v, t, err)
	}
	if len(b) > 32 {
		return nil, fmt.Errorf("encoded stream value %v with ABI type %q is too large; got: %d bytes, max: 32 bytes", v, t, len(b))
	}
	if v.Sign() < 0 {
		// If the value is negative, we need to sign-extend it to 32 bytes.
		// This is done by padding with 0xff bytes.
		return padWithOnes(b), nil
	}
	return padWithZeroes(b), nil
}

func padWithZeroes(b []byte) []byte {
	return append(make([]byte, 32-len(b)), b...)
}

func padWithOnes(b []byte) []byte {
	padded := make([]byte, 32)
	copy(padded[32-len(b):], b)
	for i := 0; i < 32-len(b); i++ {
		padded[i] = 0xff
	}
	return padded
}

const ZeroBytesSentinel = protocol.ABIZeroBytes

func (a singleABIEncoder) encodeUint64Packed(v uint64) ([]byte, error) {
	if a.Type == ZeroBytesSentinel {
		return nil, nil
	}
	return EncodePackedBigInt(a.applyMultiplier(decimal.NewFromBigInt(new(big.Int).SetUint64(v), 0)), a.Type)
}

// EncodePacked uses packed ABI encoding to encode the stream value (no padding)
func (a singleABIEncoder) encodePacked(sv protocol.StreamValue) ([]byte, error) {
	if a.Type == ZeroBytesSentinel {
		return nil, nil
	}
	var v *big.Int
	switch sv := sv.(type) {
	case *protocol.Decimal:
		if sv == nil {
			return nil, fmt.Errorf("expected non-nil *Decimal; got: %v", sv)
		}
		v = a.applyMultiplier(sv.Decimal())
	default:
		return nil, fmt.Errorf("EncodePacked only currently supports StreamValue type of *protocol.Decimal, got: %T", sv)
	}
	return EncodePackedBigInt(v, a.Type)
}

// EncodePackedBigInt converts a *big.Int to packed EVM bytes according to the Solidity type.
// For unsigned types ("uintN"), the value must be non-negative and fit in N bits.
// For signed types ("intN"), the value must be between -2^(N-1) and 2^(N-1)-1; then the function returns the two's complement representation in N bits.
// The range rule is protocol.SolidityInt.Check, which the reportability check also applies.
func EncodePackedBigInt(value *big.Int, typeStr string) ([]byte, error) {
	s, err := protocol.ParseSolidityInt(typeStr)
	if err != nil {
		return nil, err
	}
	if err := s.Check(value); err != nil {
		return nil, err
	}
	byteLen := s.Bits / 8
	if !s.Signed {
		result := make([]byte, byteLen)
		value.FillBytes(result)
		return result, nil
	}

	// Solidity uses two's complement in fixed N-bit representation.
	// twoPow = 2^bitWidth
	twoPow := new(big.Int).Lsh(big.NewInt(1), s.Bits)
	// Compute the modulo to obtain the two's complement representation.
	modValue := new(big.Int).Mod(value, twoPow)
	result := make([]byte, byteLen)
	modValue.FillBytes(result)
	return result, nil
}

// verifyMaxReportRange rejects a maxReportRange that is negative or above
// MaxReportRangeCeiling.
func verifyMaxReportRange(maxReportRange protocol.Duration) error {
	if maxReportRange < 0 {
		return fmt.Errorf("maxReportRange must be non-negative; got: %s", maxReportRange)
	}
	if maxReportRange > protocol.MaxReportRangeCeiling {
		return fmt.Errorf("maxReportRange must not exceed %s; got: %s", protocol.MaxReportRangeCeiling, maxReportRange)
	}
	return nil
}

// ClampReportRange clamps the report range to the max report range if it exceeds the max range.
// Returns the clamped valid after nanoseconds.
// If the report range is within the max range, returns the valid after nanoseconds unchanged.
// If the max report range is not specified, uses the default max report range.
// A non-positive maxReportRange falls back to the default and one above
// MaxReportRangeCeiling is capped to it. Verify rejects both in channel
// definitions, but a definition committed before those checks existed could
// still carry one.
func ClampReportRange(r logger.Logger, report protocol.Report, maxReportRange protocol.Duration) uint64 {
	if maxReportRange <= 0 {
		maxReportRange = protocol.DefaultMaxReportRange
	}
	if maxReportRange > protocol.MaxReportRangeCeiling {
		maxReportRange = protocol.MaxReportRangeCeiling
	}
	maxRange := uint64(maxReportRange)

	// ValidAfterNanoseconds < ObservationTimestampNanoseconds is enforced
	// upstream by IsReportable, but the subtractions below are unsigned so an
	// inverted range would underflow into a nonsense validity window.
	if report.ValidAfterNanoseconds > report.ObservationTimestampNanoseconds {
		r.Errorw("ValidAfterNanoseconds exceeds ObservationTimestampNanoseconds; clamping to zero range",
			"channelID", report.ChannelID, "seqNr", report.SeqNr,
			"validAfterNanoseconds", report.ValidAfterNanoseconds,
			"observationTimestampNanoseconds", report.ObservationTimestampNanoseconds)

		return report.ObservationTimestampNanoseconds
	}

	if report.ObservationTimestampNanoseconds-report.ValidAfterNanoseconds > maxRange {
		r.Warnw("Report range exceeds max report range",
			"channelID", report.ChannelID, "seqNr", report.SeqNr,
			"maxReportRange", maxReportRange, "clamping to max range", maxReportRange.String())

		if report.ObservationTimestampNanoseconds < maxRange {
			return 0
		}
		return report.ObservationTimestampNanoseconds - maxRange
	}

	return report.ValidAfterNanoseconds
}

// checkFee returns an error when fee cannot be encoded as a uint192. abi.Pack
// does not range check, so an out of range fee would wrap into a different one.
func checkFee(name string, fee *big.Int) error {
	if fee == nil {
		return fmt.Errorf("%s may not be nil", name)
	}
	if fee.Sign() < 0 {
		return fmt.Errorf("%s may not be negative (got: %s)", name, fee)
	}
	if err := protocol.CheckSolidityInt(fee, "uint192"); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// MaxExpirationWindow bounds the expirationWindow opt, in seconds. On seconds
// resolution the report encodes expiresAt as a uint32, so a larger window would
// overflow it long before the timestamp itself does.
const MaxExpirationWindow = 365 * 24 * 60 * 60

func verifyExpirationWindow(expirationWindow uint32) error {
	if expirationWindow > MaxExpirationWindow {
		return fmt.Errorf("expirationWindow must not exceed %d seconds; got: %d", MaxExpirationWindow, expirationWindow)
	}
	return nil
}

// verifyABITypes rejects ABI element types the encoders cannot encode, which
// would fail every report of the channel. packed allows ZeroBytesSentinel,
// which only packed encoding accepts.
func verifyABITypes(abi []ABIEncoder, packed bool) error {
	for i, enc := range abi {
		for j, e := range enc.encoders {
			if packed && e.Type == ZeroBytesSentinel {
				continue
			}
			if _, err := protocol.ParseSolidityInt(e.Type); err != nil {
				return fmt.Errorf("abi[%d][%d]: %w", i, j, err)
			}
		}
	}
	return nil
}
