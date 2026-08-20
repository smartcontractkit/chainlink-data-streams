package protocol

import (
	"errors"
	"fmt"
	"time"

	"github.com/goccy/go-json"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

const (
	// DefaultMaxReportRange is the default maximum range of the report if unset in the opts.
	DefaultMaxReportRange = Duration(5 * time.Minute)
)

type ReportCodec interface {
	// Encode may be lossy, so no Decode function is expected
	// Encode should handle nil stream aggregate values without panicking (it
	// may return error instead).
	// Codecs may use GetOpts(optsCache, report.ChannelID) to get cached parsed opts.
	Encode(Report, llotypes.ChannelDefinition, *OptsCache) ([]byte, error)
	// Verify may optionally verify a channel definition to ensure it is valid
	// for the given report codec. If a codec does not wish to implement
	// validation it may simply return nil here. If any definition fails
	// validation, the entire channel definitions file will be rejected.
	// This can be useful to ensure that e.g. options aren't changed
	// accidentally to something that would later break a report on encoding.
	Verify(llotypes.ChannelDefinition) error
}

// FeedIDer is optionally implemented by a ReportCodec whose reports are
// published under a feed ID. It exists so that VerifyChannelDefinitions can
// check feed IDs are unique across the whole definition set without knowing the
// format-specific opts the ID is carried in, or the format-specific rules for
// when a channel has one at all.
//
// FeedID must be a pure function of the definition, like Verify, and is only
// consulted for definitions Verify accepted. The boolean is false for a channel
// whose reports carry no feed ID -- those are identified by channel ID, which is
// unique by construction and so needs no check.
type FeedIDer interface {
	FeedID(llotypes.ChannelDefinition) (feedID [32]byte, ok bool, err error)
}

type ChannelDefinitionWithID struct {
	llotypes.ChannelDefinition
	ChannelID llotypes.ChannelID
}

type ChannelHash [32]byte

// TimeResolution represents the resolution for timestamp conversion
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
		return nil, fmt.Errorf("invalid timestamp resolution %d", tp)
	}
	return json.Marshal(s)
}

// UnmarshalJSON unmarshals TimeResolution from JSON - used to unmarshal from the Opts structs.
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
		return fmt.Errorf("invalid timestamp resolution %q", s)
	}
	return nil
}

// ConvertTimestamp converts a nanosecond timestamp to a specified resolution.
func ConvertTimestamp(timestampNanos uint64, resolution TimeResolution) uint64 {
	switch resolution {
	case ResolutionSeconds:
		return timestampNanos / 1e9
	case ResolutionMilliseconds:
		return timestampNanos / 1e6
	case ResolutionMicroseconds:
		return timestampNanos / 1e3
	case ResolutionNanoseconds:
		return timestampNanos
	default:
		return timestampNanos
	}
}

// ScaleSeconds converts a duration in seconds to a target resolution.
func ScaleSeconds(seconds uint32, resolution TimeResolution) uint64 {
	switch resolution {
	case ResolutionSeconds:
		return uint64(seconds)
	case ResolutionMilliseconds:
		return uint64(seconds) * 1e3
	case ResolutionMicroseconds:
		return uint64(seconds) * 1e6
	case ResolutionNanoseconds:
		return uint64(seconds) * 1e9
	default:
		return uint64(seconds)
	}
}

type Duration time.Duration

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case float64:
		*d = Duration(time.Duration(value))
		return nil
	case string:
		tmp, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		*d = Duration(tmp)
		return nil
	default:
		return errors.New("invalid duration")
	}
}
