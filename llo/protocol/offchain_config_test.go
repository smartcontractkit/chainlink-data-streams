package protocol

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_OffchainConfig_AggregationFaultTolerance(t *testing.T) {
	t.Run("unset and zero are distinct configs", func(t *testing.T) {
		unset, err := OffchainConfig{ProtocolVersion: 1, DefaultMinReportIntervalNanoseconds: 1}.Encode()
		require.NoError(t, err)
		zero := uint32(0)
		explicitZero, err := OffchainConfig{ProtocolVersion: 1, DefaultMinReportIntervalNanoseconds: 1, AggregationFaultTolerance: &zero}.Encode()
		require.NoError(t, err)
		assert.NotEqual(t, unset, explicitZero)

		decodedUnset, err := DecodeOffchainConfig(unset)
		require.NoError(t, err)
		assert.Nil(t, decodedUnset.AggregationFaultTolerance)

		decodedZero, err := DecodeOffchainConfig(explicitZero)
		require.NoError(t, err)
		require.NotNil(t, decodedZero.AggregationFaultTolerance)
		assert.Equal(t, uint32(0), *decodedZero.AggregationFaultTolerance)
	})
	t.Run("round-trips a set value", func(t *testing.T) {
		aft := uint32(5)
		b, err := OffchainConfig{ProtocolVersion: 2, DefaultMinReportIntervalNanoseconds: 1, AggregationFaultTolerance: &aft}.Encode()
		require.NoError(t, err)
		decoded, err := DecodeOffchainConfig(b)
		require.NoError(t, err)
		require.NotNil(t, decoded.AggregationFaultTolerance)
		assert.Equal(t, uint32(5), *decoded.AggregationFaultTolerance)
	})
	t.Run("invalid onchain bytes decode to unset", func(t *testing.T) {
		b, err := hex.DecodeString("7b2265787069726174696f6e57696e646f77223a38363430302c2262617365555344466565223a22302e3332227d")
		require.NoError(t, err)
		decoded, err := DecodeOffchainConfig(b)
		require.NoError(t, err)
		assert.Nil(t, decoded.AggregationFaultTolerance)
	})
	t.Run("out of range is rejected", func(t *testing.T) {
		aft := uint32(1 << 20)
		err := OffchainConfig{ProtocolVersion: 1, DefaultMinReportIntervalNanoseconds: 1, AggregationFaultTolerance: &aft}.Validate()
		require.EqualError(t, err, "aggregationFaultTolerance out of range: 1048576")
	})
	t.Run("is not required by Validate, since v3.0 ignores it", func(t *testing.T) {
		require.NoError(t, OffchainConfig{ProtocolVersion: 1, DefaultMinReportIntervalNanoseconds: 1}.Validate())
	})
}

func Test_OffchainConfig(t *testing.T) {
	t.Run("decoding invalid bytes", func(t *testing.T) {
		b, err := hex.DecodeString("7b2265787069726174696f6e57696e646f77223a38363430302c2262617365555344466565223a22302e3332227d")
		require.NoError(t, err)
		cfgDecoded, err := DecodeOffchainConfig(b)
		// HACK: We have actual invalid bytes written on-chain, which we have
		// to handle to be compatible with older builds which ignored offchain
		// config.
		//
		// FIXME: Return error instead after v0 is fully decommissioned and all
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
				EnableObservationCompression:        true,
			}

			b, err := cfg.Encode()
			require.NoError(t, err)

			cfgDecoded, err := DecodeOffchainConfig(b)
			require.NoError(t, err)
			assert.Equal(t, cfg, cfgDecoded)
		})
	})
	t.Run("version 2", func(t *testing.T) {
		t.Run("encode/decode valid values", func(t *testing.T) {
			cfg := OffchainConfig{
				ProtocolVersion:                     2,
				DefaultMinReportIntervalNanoseconds: 1000,
				EnableObservationCompression:        true,
			}

			b, err := cfg.Encode()
			require.NoError(t, err)

			cfgDecoded, err := DecodeOffchainConfig(b)
			require.NoError(t, err)
			assert.Equal(t, cfg, cfgDecoded)
		})
		t.Run("DefaultMinReportIntervalNanoseconds=0 is invalid", func(t *testing.T) {
			cfg := OffchainConfig{
				ProtocolVersion:                     2,
				DefaultMinReportIntervalNanoseconds: 0,
			}

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "default report cadence must be non-zero if protocol version is 2")
		})
	})
	t.Run("decoding rejects an unknown protocol version", func(t *testing.T) {
		// Validate used to run on the zero-valued struct before the decoded
		// fields were assigned, so this always passed and an unknown version was
		// silently treated as the latest known one. A node that does not
		// understand the configured version must refuse to run rather than
		// diverge from the nodes that do.
		b, err := OffchainConfig{
			ProtocolVersion:                     99,
			DefaultMinReportIntervalNanoseconds: 1000,
		}.Encode()
		require.NoError(t, err)

		_, err = DecodeOffchainConfig(b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown protocol version: 99")
	})
	t.Run("decoding rejects values that are invalid for their version", func(t *testing.T) {
		b, err := OffchainConfig{
			ProtocolVersion:                     0,
			DefaultMinReportIntervalNanoseconds: 1,
		}.Encode()
		require.NoError(t, err)

		_, err = DecodeOffchainConfig(b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default report cadence must be 0 if protocol version is 0")
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
