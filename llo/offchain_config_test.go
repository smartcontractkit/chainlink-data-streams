package llo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_OffchainConfig(t *testing.T) {
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
