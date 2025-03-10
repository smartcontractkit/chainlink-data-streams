package llo

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
)

type OffchainConfig struct {
	ProtocolVersion                     uint32
	DefaultMinReportIntervalNanoseconds uint64
}

func DecodeOffchainConfig(b []byte) (o OffchainConfig, err error) {
	pbuf := &LLOOffchainConfigProto{}
	err = proto.Unmarshal(b, pbuf)
	if err != nil {
		return o, fmt.Errorf("failed to decode offchain config: expected protobuf (got: 0x%x); %w", b, err)
	}
	if err := o.Validate(); err != nil {
		return o, fmt.Errorf("failed to decode offchain config: %w", err)
	}
	o.ProtocolVersion = pbuf.ProtocolVersion
	o.DefaultMinReportIntervalNanoseconds = pbuf.DefaultMinReportIntervalNanoseconds
	return
}

func (c OffchainConfig) Encode() ([]byte, error) {
	pbuf := &LLOOffchainConfigProto{
		ProtocolVersion:                     c.ProtocolVersion,
		DefaultMinReportIntervalNanoseconds: c.DefaultMinReportIntervalNanoseconds,
	}
	return proto.Marshal(pbuf)
}

func (c OffchainConfig) Validate() error {
	switch c.ProtocolVersion {
	case 0:
		if c.DefaultMinReportIntervalNanoseconds != 0 {
			return errors.New("default report cadence must be 0 if protocol version is 0")
		}
	case 1:
		if c.DefaultMinReportIntervalNanoseconds == 0 {
			return errors.New("default report cadence must be non-zero if protocol version is 1")
		}
	default:
		return fmt.Errorf("unknown protocol version: %d", c.ProtocolVersion)
	}
	return nil
}

func (c OffchainConfig) GetOutcomeCodec() OutcomeCodec {
	switch c.ProtocolVersion {
	case 0:
		return protoOutcomeCodecV0{}
	default:
		return protoOutcomeCodecV1{}
	}
}
