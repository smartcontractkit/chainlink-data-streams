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
	o.ProtocolVersion = pbuf.ProtocolVersion
	o.DefaultMinReportIntervalNanoseconds = pbuf.DefaultMinReportIntervalNanoseconds
	o.EnableObservationCompression = pbuf.EnableObservationCompression
	// NOTE: Validate must run on the decoded values. A node that cannot honour the
	// configured version must refuse to run rather than diverge from the nodes
	// that can.
	if err := o.Validate(); err != nil {
		return o, fmt.Errorf("failed to decode offchain config: %w", err)
	}
	return
}

func (c OffchainConfig) Encode() ([]byte, error) {
	pbuf := &LLOOffchainConfigProto{
		ProtocolVersion:                     c.ProtocolVersion,
		DefaultMinReportIntervalNanoseconds: c.DefaultMinReportIntervalNanoseconds,
		EnableObservationCompression:        c.EnableObservationCompression,
	}
	return proto.Marshal(pbuf)
}

func (c OffchainConfig) Validate() error {
	switch c.ProtocolVersion {
	case 0:
		if c.DefaultMinReportIntervalNanoseconds != 0 {
			return errors.New("default report cadence must be 0 if protocol version is 0")
		}
	case 1, 2:
		// Version 2 is version 1 with a channel vote hash that commits to every
		// field of the channel definition (see protocol.ChannelHashV2).
		// Nothing else differs, so the cadence rules are the same.
		if c.DefaultMinReportIntervalNanoseconds == 0 {
			return fmt.Errorf("default report cadence must be non-zero if protocol version is %d", c.ProtocolVersion)
		}
	default:
		return fmt.Errorf("unknown protocol version: %d", c.ProtocolVersion)
	}
	return nil
}
