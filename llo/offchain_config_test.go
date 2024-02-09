package llo

import (
	"testing"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_OffchainConfig(t *testing.T) {
	t.Run("garbage bytes", func(t *testing.T) {
		_, err := DecodeOffchainConfig([]byte{1})
		require.Error(t, err)

		assert.Contains(t, err.Error(), "failed to decode offchain config: expected protobuf (got: 0x01); proto:")
	})

	t.Run("zero length for PredecessorConfigDigest is ok", func(t *testing.T) {
		decoded, err := DecodeOffchainConfig([]byte{})
		require.NoError(t, err)
		assert.Equal(t, OffchainConfig{}, decoded)
	})

	t.Run("encoding nil PredecessorConfigDigest is ok", func(t *testing.T) {
		cfg := OffchainConfig{nil}

		b, err := cfg.Encode()
		require.NoError(t, err)

		assert.Len(t, b, 0)
	})

	t.Run("encode and decode", func(t *testing.T) {
		cd := types.ConfigDigest([32]byte{1, 2, 3})
		cfg := OffchainConfig{&cd}

		b, err := cfg.Encode()
		require.NoError(t, err)

		cfgDecoded, err := DecodeOffchainConfig(b)
		require.NoError(t, err)
		assert.Equal(t, cfg, cfgDecoded)
	})
}
