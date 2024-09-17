package llo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_OffchainConfig(t *testing.T) {
	t.Run("encode and decode", func(t *testing.T) {
		cfg := OffchainConfig{}

		b, err := cfg.Encode()
		require.NoError(t, err)

		cfgDecoded, err := DecodeOffchainConfig(b)
		require.NoError(t, err)
		assert.Equal(t, cfg, cfgDecoded)
	})
}
