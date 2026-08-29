package protocol

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
)

type OffchainConfig struct {
	ProtocolVersion uint32
	// DefaultMinReportIntervalNanoseconds is the default minimum report interval in nanoseconds.
	// It must be set to 0 for protocol version 0.
	// It must be set to 1 or greater for protocol version 1+.
	//
	// NOTE: This merely controls the _minimum_ interval between reports. It
	// does not guarantee a maximum interval. If you want reports to be
	// produced quickly, you are still limited by OCR3's DeltaRound and
	// DeltaGrace params, as well as networking latency.
	DefaultMinReportIntervalNanoseconds uint64
	// DefaultMinObservationIntervalNanoseconds is the default minimum interval
	// in nanoseconds between the last report of a channel and the next time its
	// streams are observed/aggregated. When the elapsed time since a channel's
	// last report (its validAfter watermark) is less than this threshold, the
	// channel's observation and aggregation are skipped entirely for the round.
	//
	// It must be set to 0 for protocol version 0.
	// For protocol version 1+, 0 means disabled (all channels are always
	// observed); a non-zero value enables the skip. It must not exceed
	// DefaultMinReportIntervalNanoseconds, or a channel could be reportable
	// but lack the observations needed to produce a report.
	//
	// Setting it equal to DefaultMinReportIntervalNanoseconds is safe: a
	// channel's streams enter the observed set one round before the channel
	// itself becomes due, so the values are already gathered by the round that
	// aggregates and reports them.
	DefaultMinObservationIntervalNanoseconds uint64
	// EnableObservationCompression enables observation compression.
	EnableObservationCompression bool
}

func DecodeOffchainConfig(b []byte) (o OffchainConfig, err error) {
	pbuf := &LLOOffchainConfigProto{}
	err = proto.Unmarshal(b, pbuf)
	if err != nil {
		// HACK: We have actual invalid bytes written on-chain, which we have
		// to handle to be compatible with older builds which ignored offchain
		// config.
		//
		// FIXME: Return error instead after v0 is fully decommissioned and all
		// contracts have been updated with proper v1 config.
		//
		// MERC-2272
		return o, nil
		// return o, fmt.Errorf("failed to decode offchain config: expected protobuf (got: 0x%x); %w", b, err)
	}
	if err := o.Validate(); err != nil {
		return o, fmt.Errorf("failed to decode offchain config: %w", err)
	}
	o.ProtocolVersion = pbuf.ProtocolVersion
	o.DefaultMinReportIntervalNanoseconds = pbuf.DefaultMinReportIntervalNanoseconds
	o.DefaultMinObservationIntervalNanoseconds = pbuf.DefaultMinObservationIntervalNanoseconds
	o.EnableObservationCompression = pbuf.EnableObservationCompression
	return
}

func (c OffchainConfig) Encode() ([]byte, error) {
	pbuf := &LLOOffchainConfigProto{
		ProtocolVersion:                          c.ProtocolVersion,
		DefaultMinReportIntervalNanoseconds:      c.DefaultMinReportIntervalNanoseconds,
		DefaultMinObservationIntervalNanoseconds: c.DefaultMinObservationIntervalNanoseconds,
		EnableObservationCompression:             c.EnableObservationCompression,
	}
	return proto.Marshal(pbuf)
}

func (c OffchainConfig) Validate() error {
	switch c.ProtocolVersion {
	case 0:
		if c.DefaultMinReportIntervalNanoseconds != 0 {
			return errors.New("default report cadence must be 0 if protocol version is 0")
		}
		if c.DefaultMinObservationIntervalNanoseconds != 0 {
			return errors.New("default observation cadence must be 0 if protocol version is 0")
		}
	case 1:
		if c.DefaultMinReportIntervalNanoseconds == 0 {
			return errors.New("default report cadence must be non-zero if protocol version is 1")
		}
		if c.DefaultMinObservationIntervalNanoseconds > c.DefaultMinReportIntervalNanoseconds {
			return errors.New("default observation cadence must not exceed default report cadence")
		}
	default:
		return fmt.Errorf("unknown protocol version: %d", c.ProtocolVersion)
	}
	return nil
}
