package llo

import (
	"testing"

	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_EVMOnchainConfigCodec(t *testing.T) {
	c := EVMOnchainConfigCodec{}

	t.Run("invalid length", func(t *testing.T) {
		_, err := c.Decode([]byte{1})
		require.Error(t, err)

		assert.Contains(t, err.Error(), "unexpected length of OnchainConfig, expected 64, got 1")
	})

	t.Run("encoding unsupported version fails", func(t *testing.T) {
		cfg := OnchainConfig{Version: uint8(100)}

		_, err := c.Encode(cfg)
		require.EqualError(t, err, "unexpected version of OnchainConfig, expected 1, got 100")
	})
	t.Run("decoding unsupported version fails", func(t *testing.T) {
		b := make([]byte, 64)
		b[30] = 100

		_, err := c.Decode(b)
		require.EqualError(t, err, "unexpected version of OnchainConfig, expected 1, got 25600")
	})

	t.Run("encoding nil PredecessorConfigDigest is ok", func(t *testing.T) {
		cfg := OnchainConfig{Version: uint8(1)}

		b, err := c.Encode(cfg)
		require.NoError(t, err)

		assert.Len(t, b, 64)
		assert.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, b)

		t.Run("decoding zero predecessor config digest results in nil", func(t *testing.T) {
			decoded, err := c.Decode(b)
			require.NoError(t, err)
			assert.Equal(t, OnchainConfig{Version: 1}, decoded)
		})
	})

	t.Run("encode and decode", func(t *testing.T) {
		cd := types.ConfigDigest([32]byte{1, 2, 3})
		cfg := OnchainConfig{Version: uint8(1), PredecessorConfigDigest: &cd}

		b, err := c.Encode(cfg)
		require.NoError(t, err)

		cfgDecoded, err := c.Decode(b)
		require.NoError(t, err)
		assert.Equal(t, cfg, cfgDecoded)
	})
}
