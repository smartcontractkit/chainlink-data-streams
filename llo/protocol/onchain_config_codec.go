package protocol

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/smartcontractkit/libocr/bigbigendian"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
)

const onchainConfigVersion = 1

var onchainConfigVersionBig = big.NewInt(onchainConfigVersion)

const onchainConfigEncodedLength = 2 * 32 // 2x 32bit evm word: version and predecessorConfigDigest

// ErrUnsupportedOnchainConfigVersion is returned when decoding an onchain
// config whose version this node does not support.
var ErrUnsupportedOnchainConfigVersion = errors.New("unsupported OnchainConfig version")

type OnchainConfig struct {
	Version                 uint8
	PredecessorConfigDigest *types.ConfigDigest
}

type OnchainConfigCodec interface {
	Decode(b []byte) (OnchainConfig, error)
	Encode(OnchainConfig) ([]byte, error)
}

var _ OnchainConfigCodec = EVMOnchainConfigCodec{}

// EVMOnchainConfigCodec provides a llo-specific implementation of
// OnchainConfigCodec.
//
// An encoded onchain config is expected to be in the format
// <version><predecessorConfigDigest>
// where each field is a 32 byte EVM word and a zero predecessorConfigDigest
// means no predecessor.
//
// Decode rejects any version other than onchainConfigVersion, since a node
// cannot safely interpret the semantics of a newer version. Upgrade nodes
// before publishing a config with a newer version.
type EVMOnchainConfigCodec struct{}

func (EVMOnchainConfigCodec) Decode(b []byte) (OnchainConfig, error) {
	if len(b) != onchainConfigEncodedLength {
		return OnchainConfig{}, fmt.Errorf("unexpected length of OnchainConfig, expected %v, got %v", onchainConfigEncodedLength, len(b))
	}

	v, err := bigbigendian.DeserializeSigned(32, b[:32])
	if err != nil {
		return OnchainConfig{}, err
	}
	if v.Cmp(onchainConfigVersionBig) != 0 {
		return OnchainConfig{}, fmt.Errorf("%w: got %v, supported %v; upgrade the node before publishing a config with a newer version", ErrUnsupportedOnchainConfigVersion, v, onchainConfigVersion)
	}

	o := OnchainConfig{
		Version: onchainConfigVersion,
	}

	cd := types.ConfigDigest(b[32:64])
	if (cd != types.ConfigDigest{}) {
		o.PredecessorConfigDigest = &cd
	}
	return o, nil
}

func (EVMOnchainConfigCodec) Encode(c OnchainConfig) ([]byte, error) {
	if c.Version != onchainConfigVersion {
		return nil, fmt.Errorf("unexpected version of OnchainConfig, expected %v, got %v", onchainConfigVersion, c.Version)
	}
	verBytes, err := bigbigendian.SerializeSigned(32, onchainConfigVersionBig)
	if err != nil {
		return nil, err
	}
	cdBytes := make([]byte, 32)
	if c.PredecessorConfigDigest != nil {
		copy(cdBytes, c.PredecessorConfigDigest[:])
	}
	result := make([]byte, 0, onchainConfigEncodedLength)
	result = append(result, verBytes...)
	result = append(result, cdBytes...)
	return result, nil
}
