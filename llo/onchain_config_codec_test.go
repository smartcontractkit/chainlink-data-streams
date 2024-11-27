package llo

import (
	"bytes"
	reflect "reflect"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Fuzz_EVMOnchainConfigCodec_Decode(f *testing.F) {
	c := EVMOnchainConfigCodec{}

	f.Add([]byte{})
	f.Add([]byte{1})

	cfg := OnchainConfig{Version: uint8(1), PredecessorConfigDigest: nil}
	noCd, err := c.Encode(cfg)
	require.NoError(f, err)
	f.Add(noCd)

	cd := types.ConfigDigest([32]byte{1, 2, 3})
	cfg = OnchainConfig{Version: uint8(1), PredecessorConfigDigest: &cd}
	valid, err := c.Encode(cfg)
	require.NoError(f, err)
	f.Add(valid)

	f.Fuzz(func(t *testing.T, data []byte) {
		// test that it doesn't panic, don't care about errors
		c.Decode(data)
	})
}

func Test_EVMOnchainConfigCodec_Properties(t *testing.T) {
	properties := gopter.NewProperties(nil)

	codec := EVMOnchainConfigCodec{}

	properties.Property("Encode/Decode", prop.ForAll(
		func(cfg OnchainConfig) bool {
			b, err := codec.Encode(cfg)
			require.NoError(t, err)
			cfg2, err := codec.Decode(b)
			require.NoError(t, err)

			return cfg.Version == cfg2.Version && bytes.Equal(cfg.PredecessorConfigDigest[:], cfg2.PredecessorConfigDigest[:])
		},
		gen.StrictStruct(reflect.TypeOf(&OnchainConfig{}), map[string]gopter.Gen{
			"Version":                 gen.Const(uint8(1)), // 1 is the only supported version
			"PredecessorConfigDigest": genConfigDigestPtr(),
		}),
	))

	properties.TestingRun(t)
}

func genConfigDigestPtr() gopter.Gen {
	return func(p *gopter.GenParameters) *gopter.GenResult {
		var cd types.ConfigDigest
		p.Rng.Read(cd[:])
		return gopter.NewGenResult(&cd, gopter.NoShrinker)
	}
}

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
