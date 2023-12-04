package streams

import "github.com/smartcontractkit/libocr/offchainreporting2/types"

type OnchainConfig struct {
	PredecessorConfigDigest *types.ConfigDigest
}

var _ OnchainConfigCodec = &JSONOnchainConfigCodec{}

type JSONOnchainConfigCodec struct{}

func (c *JSONOnchainConfigCodec) Encode(OnchainConfig) ([]byte, error) {
	// TODO
	return nil, nil
}

func (c *JSONOnchainConfigCodec) Decode([]byte) (OnchainConfig, error) {
	// TODO
	return OnchainConfig{}, nil
}
