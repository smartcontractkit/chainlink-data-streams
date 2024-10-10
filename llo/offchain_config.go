package llo

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

type OffchainConfig struct {
	// NOTE: Currently OffchainConfig does not contain anything, and is not used
}

func DecodeOffchainConfig(b []byte) (o OffchainConfig, err error) {
	pbuf := &LLOOffchainConfigProto{}
	err = proto.Unmarshal(b, pbuf)
	if err != nil {
		return o, fmt.Errorf("failed to decode offchain config: expected protobuf (got: 0x%x); %w", b, err)
	}
	return
}

func (c OffchainConfig) Encode() ([]byte, error) {
	pbuf := LLOOffchainConfigProto{}
	return proto.Marshal(&pbuf)
}
