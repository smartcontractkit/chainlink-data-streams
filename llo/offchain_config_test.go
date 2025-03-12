package llo

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_OffchainConfig(t *testing.T) {
	t.Run("decoding invalid bytes", func(t *testing.T) {
		b, err := hex.DecodeString("7b2265787069726174696f6e57696e646f77223a38363430302c2262617365555344466565223a22302e3332227d")
		require.NoError(t, err)
		cfgDecoded, err := DecodeOffchainConfig(b)
		// HACK: We have actual invalid bytes written on-chain, which we have
		// to handle to be compatible with older builds which ignored offchain
		// config.
		//
		// FIXME: Return error instead after v0 is fully decomissioned and all
		// contracts have been updated with proper v1 config.
		//
		// MERC-2272
		require.NoError(t, err)
		assert.Equal(t, OffchainConfig{
			ProtocolVersion:                     0,
			DefaultMinReportIntervalNanoseconds: 0,
		}, cfgDecoded)
	})
	t.Run("version 0", func(t *testing.T) {
		t.Run("decodes empty offchainconfig (version 0)", func(t *testing.T) {
			cfgDecoded, err := DecodeOffchainConfig([]byte{})
			require.NoError(t, err)

			var cfg OffchainConfig
			assert.Equal(t, cfg, cfgDecoded)
			assert.Equal(t, uint32(0), cfgDecoded.ProtocolVersion)
			assert.Equal(t, uint64(0), cfgDecoded.DefaultMinReportIntervalNanoseconds)
		})
		t.Run("setting DefaultMinReportIntervalNanoseconds is invalid", func(t *testing.T) {
			cfg := OffchainConfig{
				ProtocolVersion:                     0,
				DefaultMinReportIntervalNanoseconds: 1,
			}

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "default report cadence must be 0 if protocol version is 0")
		})
	})
	t.Run("version 1", func(t *testing.T) {
		t.Run("encode/decode valid values", func(t *testing.T) {
			cfg := OffchainConfig{
				ProtocolVersion:                     1,
				DefaultMinReportIntervalNanoseconds: 1000,
			}

			b, err := cfg.Encode()
			require.NoError(t, err)

			cfgDecoded, err := DecodeOffchainConfig(b)
			require.NoError(t, err)
			assert.Equal(t, cfg, cfgDecoded)
		})
	})
	t.Run("DefaultMinReportIntervalNanoseconds=0 is invalid", func(t *testing.T) {
		cfg := OffchainConfig{
			ProtocolVersion:                     1,
			DefaultMinReportIntervalNanoseconds: 0,
		}

		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default report cadence must be non-zero if protocol version is 1")
	})
}
