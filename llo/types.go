package llo

import (
	"fmt"

	"github.com/goccy/go-json"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type ObservationCodec interface {
	Encode(obs Observation) (types.Observation, error)
	Decode(encoded types.Observation) (obs Observation, err error)
}

type OutcomeCodec interface {
	Encode(outcome Outcome) (ocr3types.Outcome, error)
	Decode(encoded ocr3types.Outcome) (outcome Outcome, err error)
}

type ReportCodec interface {
	// Encode may be lossy, so no Decode function is expected
	// Encode should handle nil stream aggregate values without panicking (it
	// may return error instead)
	//
	// parsedOpts is a pre-parsed instantiation of ChannelDefinition.Opts which is created from
	// the codecs Opts struct. cd.Opts can be nil and is up to the codec to determine
	// if it needs Opts. If the codec does not have Opts it should pass in nil.
	// For codecs with opts:
	// If parsedOpts is nil, the codec is expected to parse cd.Opts directly.
	// If parsedOpts is non-nil, the codec should type-assert it to its expected opts type.
	Encode(r Report, cd llotypes.ChannelDefinition, parsedOpts any) ([]byte, error)
	// Verify may optionally verify a channel definition to ensure it is valid
	// for the given report codec. If a codec does not wish to implement
	// validation it may simply return nil here. If any definition fails
	// validation, the entire channel definitions file will be rejected.
	// This can be useful to ensure that e.g. options aren't changed
	// accidentally to something that would later break a report on encoding.
	Verify(llotypes.ChannelDefinition) error
}

// OptsParser parses raw channel opts bytes into a codec-specific structure.
// ReportCodecs may implement this interface to enable caching of parsed opts
// to avoid repeated unmarshalling of Opts bytes. For example unmarshalling
// from JSON is much more expensive than peforming a type assertion on an `any` type.
// Since not all Codec Opts might have the same Options - you can create
// `OptsProvider` interfaces where needed. For Example `TimeResolutionProvider` or `ABIProvider`
// and the Codecs can implement all interfaces that match the specific Opt.
type OptsParser interface {
	// ParseOpts parses the raw opts bytes and returns a codec-specific
	// parsed opts structure as `any`.
	// Use the returned interface to type assert to the specific Opts type.
	// For Example:
	// opts, err := codec.ParseOpts(optsBytes)
	// if err != nil {
	// 	return nil, err
	// }
	//
	// optsProvider, ok := opts.(OptsProvider)
	// if !ok {
	// 	return nil, fmt.Errorf("invalid opts type")
	// }
	ParseOpts(opts []byte) (any, error)
}

// TimeResolutionProvider extracts time resolution information from codec opts
type TimeResolutionProvider interface {
	// TimeResolution returns the time resolution from the parsed opts.
	// The parsedOpts must be the value returned by OptsParser.ParseOpts.
	TimeResolution(parsedOpts any) (TimeResolution, error)
}

// CalculatedStreamABI represents expression config needed for calculated streams
type CalculatedStreamABI struct {
	Type               string
	Expression         string
	ExpressionStreamID llotypes.StreamID
}

// CalculatedStreamABIProvider extracts expression ABI config from parsed codec opts
type CalculatedStreamABIProvider interface {
	// CalculatedStreamABI returns the expression ABI entries from the parsed opts.
	// The parsedOpts must be the value returned by OptsParser.ParseOpts.
	CalculatedStreamABI(parsedOpts any) ([]CalculatedStreamABI, error)
}

type ChannelDefinitionWithID struct {
	llotypes.ChannelDefinition
	ChannelID llotypes.ChannelID
}

type ChannelHash [32]byte

type Transmitter interface {
	// NOTE: Mercury doesn't actually transmit on-chain, so there is no
	// "contract" involved with the transmitter.
	// - Transmit should be implemented and send to Mercury server
	// - FromAccount() should return CSA public key
	ocr3types.ContractTransmitter[llotypes.ReportInfo]
}

// TimeResolution can be used to represent the resolution of an epoch timestamp
type TimeResolution uint8

const (
	ResolutionSeconds TimeResolution = iota
	ResolutionMilliseconds
	ResolutionMicroseconds
	ResolutionNanoseconds
)

func (tp TimeResolution) MarshalJSON() ([]byte, error) {
	var s string
	switch tp {
	case ResolutionSeconds:
		s = "s"
	case ResolutionMilliseconds:
		s = "ms"
	case ResolutionMicroseconds:
		s = "us"
	case ResolutionNanoseconds:
		s = "ns"
	default:
		return nil, fmt.Errorf("invalid time resolution %d", tp)
	}
	return json.Marshal(s)
}

func (tp *TimeResolution) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "s":
		*tp = ResolutionSeconds
	case "ms":
		*tp = ResolutionMilliseconds
	case "us":
		*tp = ResolutionMicroseconds
	case "ns":
		*tp = ResolutionNanoseconds
	default:
		return fmt.Errorf("invalid time resolution %q", s)
	}
	return nil
}
