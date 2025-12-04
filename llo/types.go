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
	Encode(Report, llotypes.ChannelDefinition) ([]byte, error)
	// Verify may optionally verify a channel definition to ensure it is valid
	// for the given report codec. If a codec does not wish to implement
	// validation it may simply return nil here. If any definition fails
	// validation, the entire channel definitions file will be rejected.
	// This can be useful to ensure that e.g. options aren't changed
	// accidentally to something that would later break a report on encoding.
	Verify(llotypes.ChannelDefinition) error
}

type OptsParser interface {
	// ParseOpts parses the ReportCodec's opts and returns an interface{} which can be type asserted
	// to the specific ReportCodec's opts type.
	// Use when parsing of Opts is considered too expensive to do repeatedly.
	ParseOpts(opts []byte) (interface{}, error)

	// TimeResolution returns the time resolution available in the parsed opts
	TimeResolution(parsedOpts interface{}) (TimeResolution, error)
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
