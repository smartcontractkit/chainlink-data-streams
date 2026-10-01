package pluginconfig

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/utils/hex"
)

func Test_Config(t *testing.T) {
	t.Run("unmarshals from toml", func(t *testing.T) {
		cdjson := `{
	"42": {
		"reportFormat": 42,
		"chainSelector": 142,
		"streamIds": [1, 2]
	},
	"43": {
		"reportFormat": 42,
		"chainSelector": 142,
		"streamIds": [1, 3]
	},
	"44": {
		"reportFormat": 42,
		"chainSelector": 143,
		"streamIds": [1, 4]
	}
}`

		t.Run("with all possible values set", func(t *testing.T) {
			rawToml := fmt.Sprintf(`
				Servers = { "example.com:80" = "724ff6eae9e900270edfff233e16322a70ec06e1a6e62a81ef13921f398f6c93", "example2.invalid:1234" = "524ff6eae9e900270edfff233e16322a70ec06e1a6e62a81ef13921f398f6c93" }
				BenchmarkMode = true
				ChannelDefinitionsContractAddress = "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
				ChannelDefinitionsContractFromBlock = 1234
				ChannelDefinitions = """
%s
"""`, cdjson)

			var mc PluginConfig
			err := toml.Unmarshal([]byte(rawToml), &mc)
			require.NoError(t, err)

			assert.Len(t, mc.Servers, 2)
			assert.Equal(t, map[string]hex.PlainHexBytes{"example.com:80": hex.PlainHexBytes{0x72, 0x4f, 0xf6, 0xea, 0xe9, 0xe9, 0x0, 0x27, 0xe, 0xdf, 0xff, 0x23, 0x3e, 0x16, 0x32, 0x2a, 0x70, 0xec, 0x6, 0xe1, 0xa6, 0xe6, 0x2a, 0x81, 0xef, 0x13, 0x92, 0x1f, 0x39, 0x8f, 0x6c, 0x93}, "example2.invalid:1234": hex.PlainHexBytes{0x52, 0x4f, 0xf6, 0xea, 0xe9, 0xe9, 0x0, 0x27, 0xe, 0xdf, 0xff, 0x23, 0x3e, 0x16, 0x32, 0x2a, 0x70, 0xec, 0x6, 0xe1, 0xa6, 0xe6, 0x2a, 0x81, 0xef, 0x13, 0x92, 0x1f, 0x39, 0x8f, 0x6c, 0x93}}, mc.Servers)
			assert.Equal(t, "0xDeaDbeefdEAdbeefdEadbEEFdeadbeEFdEaDbeeF", mc.ChannelDefinitionsContractAddress.Hex())
			assert.Equal(t, int64(1234), mc.ChannelDefinitionsContractFromBlock)
			assert.JSONEq(t, cdjson, mc.ChannelDefinitions)
			assert.True(t, mc.BenchmarkMode)

			err = mc.Validate()
			require.Error(t, err)

			assert.Contains(t, err.Error(), "llo: ChannelDefinitionsContractAddress is not allowed if ChannelDefinitions is specified")
			assert.Contains(t, err.Error(), "llo: ChannelDefinitionsContractFromBlock is not allowed if ChannelDefinitions is specified")
		})

		t.Run("with only channelDefinitions", func(t *testing.T) {
			rawToml := fmt.Sprintf(`
				Servers = { "example.com:80" = "724ff6eae9e900270edfff233e16322a70ec06e1a6e62a81ef13921f398f6c93" }
				DonID = 12345
				ChannelDefinitions = """
%s
"""`, cdjson)

			var mc PluginConfig
			err := toml.Unmarshal([]byte(rawToml), &mc)
			require.NoError(t, err)

			assert.Len(t, mc.Servers, 1)
			assert.JSONEq(t, cdjson, mc.ChannelDefinitions)
			assert.Equal(t, uint32(12345), mc.DonID)
			assert.False(t, mc.BenchmarkMode)

			err = mc.Validate()
			require.NoError(t, err)
		})
		t.Run("with only channelDefinitions contract details", func(t *testing.T) {
			rawToml := `
			Servers = { "example.com:80" = "724ff6eae9e900270edfff233e16322a70ec06e1a6e62a81ef13921f398f6c93" }
			DonID = 12345
			ChannelDefinitionsContractAddress = "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"`

			var mc PluginConfig
			err := toml.Unmarshal([]byte(rawToml), &mc)
			require.NoError(t, err)

			assert.Len(t, mc.Servers, 1)
			assert.Equal(t, "0xDeaDbeefdEAdbeefdEadbEEFdeadbeEFdEaDbeeF", mc.ChannelDefinitionsContractAddress.Hex())
			assert.Equal(t, uint32(12345), mc.DonID)
			assert.False(t, mc.BenchmarkMode)

			err = mc.Validate()
			require.NoError(t, err)
		})
		t.Run("with missing ChannelDefinitionsContractAddress", func(t *testing.T) {
			rawToml := `
			DonID = 12345
			Servers = { "example.com:80" = "724ff6eae9e900270edfff233e16322a70ec06e1a6e62a81ef13921f398f6c93" }
			`

			var mc PluginConfig
			err := toml.Unmarshal([]byte(rawToml), &mc)
			require.NoError(t, err)

			assert.Len(t, mc.Servers, 1)
			assert.Equal(t, uint32(12345), mc.DonID)
			assert.False(t, mc.BenchmarkMode)

			err = mc.Validate()
			require.EqualError(t, err, "llo: ChannelDefinitionsContractAddress is required if ChannelDefinitions is not specified")
		})

		t.Run("with invalid values", func(t *testing.T) {
			rawToml := `
				ChannelDefinitionsContractFromBlock = "invalid"
			`

			var mc PluginConfig
			err := toml.Unmarshal([]byte(rawToml), &mc)
			require.Error(t, err)
			require.EqualError(t, err, `toml: cannot decode TOML string into struct field pluginconfig.PluginConfig.ChannelDefinitionsContractFromBlock of type int64`)
			assert.False(t, mc.BenchmarkMode)

			rawToml = `
				ServerURL = "http://example.com"
				ServerPubKey = "4242"
			`

			err = toml.Unmarshal([]byte(rawToml), &mc)
			require.NoError(t, err)

			err = mc.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), `DonID must be specified and not zero`)
			assert.Contains(t, err.Error(), `At least one Mercury server or Transmitter must be specified`)
			assert.Contains(t, err.Error(), `ChannelDefinitionsContractAddress is required if ChannelDefinitions is not specified`)
		})
	})
}

func Test_PluginConfig_PluginVersion(t *testing.T) {
	base := PluginConfig{
		DonID:                             12345,
		Servers:                           map[string]hex.PlainHexBytes{"example.com:80": make(hex.PlainHexBytes, 32)},
		ChannelDefinitionsContractAddress: common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
	}

	t.Run("defaults to v30 when empty", func(t *testing.T) {
		pc := base
		require.NoError(t, pc.Validate())
		assert.False(t, pc.IsV31())
	})
	t.Run("explicit v30 is not v31", func(t *testing.T) {
		pc := base
		pc.PluginVersion = PluginVersionV30
		require.NoError(t, pc.Validate())
		assert.False(t, pc.IsV31())
	})
	t.Run("v31 selects v31", func(t *testing.T) {
		pc := base
		pc.PluginVersion = PluginVersionV31
		require.NoError(t, pc.Validate())
		assert.True(t, pc.IsV31())
	})
	t.Run("unmarshals pluginVersion from toml", func(t *testing.T) {
		var pc PluginConfig
		require.NoError(t, toml.Unmarshal([]byte(`pluginVersion = "v31"`), &pc))
		assert.True(t, pc.IsV31())
	})
	t.Run("rejects unknown version", func(t *testing.T) {
		pc := base
		pc.PluginVersion = "9.9"
		err := pc.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PluginVersion must be one of")
	})
}

func Test_PluginConfig_PluginVersions(t *testing.T) {
	base := PluginConfig{
		DonID:                             12345,
		Servers:                           map[string]hex.PlainHexBytes{"example.com:80": make(hex.PlainHexBytes, 32)},
		ChannelDefinitionsContractAddress: common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
	}

	t.Run("falls back to the scalar PluginVersion when unset", func(t *testing.T) {
		t.Run("empty scalar means v30 everywhere", func(t *testing.T) {
			pc := base
			require.NoError(t, pc.Validate())
			for i := range MaxProtocolInstances {
				assert.Equal(t, PluginVersionV30, pc.PluginVersionForInstance(i))
				assert.False(t, pc.IsV31Instance(i))
			}
			assert.False(t, pc.AnyV31())
		})
		t.Run("v31 scalar means v31 everywhere", func(t *testing.T) {
			pc := base
			pc.PluginVersion = PluginVersionV31
			require.NoError(t, pc.Validate())
			for i := range MaxProtocolInstances {
				assert.True(t, pc.IsV31Instance(i))
			}
			assert.True(t, pc.AnyV31())
		})
	})

	t.Run("takes precedence over the scalar when set", func(t *testing.T) {
		pc := base
		pc.PluginVersion = PluginVersionV31
		pc.PluginVersions = []PluginVersion{PluginVersionV30}
		require.NoError(t, pc.Validate())
		assert.Equal(t, PluginVersionV30, pc.PluginVersionForInstance(0))
		assert.False(t, pc.AnyV31())
	})

	t.Run("mixed list selects per instance", func(t *testing.T) {
		pc := base
		pc.PluginVersions = []PluginVersion{PluginVersionV30, PluginVersionV31}
		require.NoError(t, pc.Validate())

		assert.Equal(t, PluginVersionV30, pc.PluginVersionForInstance(0))
		assert.False(t, pc.IsV31Instance(0))

		assert.Equal(t, PluginVersionV31, pc.PluginVersionForInstance(1))
		assert.True(t, pc.IsV31Instance(1))

		// The v31-only dependencies are per job, so one v31 instance is enough.
		assert.True(t, pc.AnyV31())
	})

	t.Run("an empty entry means v30", func(t *testing.T) {
		pc := base
		pc.PluginVersion = PluginVersionV31
		pc.PluginVersions = []PluginVersion{"", PluginVersionV31}
		require.NoError(t, pc.Validate())
		assert.Equal(t, PluginVersionV30, pc.PluginVersionForInstance(0))
		assert.Equal(t, PluginVersionV31, pc.PluginVersionForInstance(1))
	})

	t.Run("an index past the list falls back to the scalar", func(t *testing.T) {
		pc := base
		pc.PluginVersion = PluginVersionV31
		pc.PluginVersions = []PluginVersion{PluginVersionV30}
		require.NoError(t, pc.Validate())
		assert.Equal(t, PluginVersionV30, pc.PluginVersionForInstance(0))
		assert.Equal(t, PluginVersionV31, pc.PluginVersionForInstance(1))
		// A negative index cannot index the list either.
		assert.Equal(t, PluginVersionV31, pc.PluginVersionForInstance(-1))
	})

	t.Run("unmarshals pluginVersions from toml", func(t *testing.T) {
		var pc PluginConfig
		require.NoError(t, toml.Unmarshal([]byte(`pluginVersions = ["v30", "v31"]`), &pc))
		assert.Equal(t, []PluginVersion{PluginVersionV30, PluginVersionV31}, pc.PluginVersions)
		assert.False(t, pc.IsV31Instance(0))
		assert.True(t, pc.IsV31Instance(1))
	})

	t.Run("unmarshals pluginVersions from json", func(t *testing.T) {
		var pc PluginConfig
		require.NoError(t, pc.Unmarshal([]byte(`{"pluginVersions": ["v30", "v31"]}`)))
		assert.Equal(t, []PluginVersion{PluginVersionV30, PluginVersionV31}, pc.PluginVersions)
	})

	t.Run("rejects an unknown entry", func(t *testing.T) {
		pc := base
		pc.PluginVersions = []PluginVersion{PluginVersionV30, "9.9"}
		err := pc.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `PluginVersions[1] must be one of`)
	})

	t.Run("rejects more entries than protocol instances", func(t *testing.T) {
		pc := base
		pc.PluginVersions = []PluginVersion{PluginVersionV30, PluginVersionV31, PluginVersionV31}
		err := pc.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PluginVersions must have at most 2 entries")
	})
}

func Test_PluginConfig_Validate(t *testing.T) {
	t.Run("with invalid URLs or keys", func(t *testing.T) {
		servers := map[string]hex.PlainHexBytes{
			"not a valid url":                hex.PlainHexBytes([]byte{1, 2, 3}),
			"mercuryserver.invalid:1234/foo": nil,
		}
		pc := PluginConfig{Servers: servers}

		err := pc.Validate()
		assert.Contains(t, err.Error(), "ServerPubKey must be a 32-byte hex string")
		assert.Contains(t, err.Error(), "invalid value for ServerURL: llo: invalid value for ServerURL, got: \"not a valid url\"")
	})
}

func Test_PluginConfig_GetServers(t *testing.T) {
	t.Run("with multiple servers", func(t *testing.T) {
		servers := map[string]hex.PlainHexBytes{
			"example.com:80":                 hex.PlainHexBytes([]byte{1, 2, 3}),
			"mercuryserver.invalid:1234/foo": hex.PlainHexBytes([]byte{4, 5, 6}),
		}
		pc := PluginConfig{Servers: servers}

		require.Len(t, pc.GetServers(), 2)
		assert.Equal(t, "example.com:80", pc.GetServers()[0].URL)
		assert.Equal(t, hex.PlainHexBytes{1, 2, 3}, pc.GetServers()[0].PubKey)
		assert.Equal(t, "mercuryserver.invalid:1234/foo", pc.GetServers()[1].URL)
		assert.Equal(t, hex.PlainHexBytes{4, 5, 6}, pc.GetServers()[1].PubKey)
	})
}

func Test_PluginConfig_Unmarshal_SizeLimit(t *testing.T) {
	t.Run("accepts config at the size limit", func(t *testing.T) {
		// pad with whitespace to exactly hit the limit
		data := []byte(`{"donID":1}`)
		data = append(data, make([]byte, MaxPluginConfigSize-len(data))...)
		for i := len(`{"donID":1}`); i < len(data); i++ {
			data[i] = ' '
		}
		require.Len(t, data, MaxPluginConfigSize)

		var p PluginConfig
		require.NoError(t, p.Unmarshal(data))
		assert.Equal(t, uint32(1), p.DonID)
	})

	t.Run("rejects config over the size limit", func(t *testing.T) {
		data := make([]byte, MaxPluginConfigSize+1)

		var p PluginConfig
		err := p.Unmarshal(data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("plugin config too large; got %d bytes, max %d bytes", MaxPluginConfigSize+1, MaxPluginConfigSize))
	})
}
