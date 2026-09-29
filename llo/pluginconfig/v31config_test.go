package pluginconfig

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/utils/hex"
)

func v31Base() PluginConfig {
	return PluginConfig{
		DonID:                             12345,
		Servers:                           map[string]hex.PlainHexBytes{"example.com:80": make(hex.PlainHexBytes, 32)},
		ChannelDefinitionsContractAddress: common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		PluginVersion:                     PluginVersionV31,
	}
}

func Test_V31Config_Unmarshal(t *testing.T) {
	t.Run("from toml", func(t *testing.T) {
		rawToml := `
			pluginVersion = "v31"
			[v31]
			verboseLogging = true
			maxSnapshotRounds = 2
			blobLifetimeRounds = 8
			maxDurationBlobObservation = "1s"
			blobInFlightWaitFactor = 4
			maxBlobSnapshotAge = "-1s"
			maxRoundPeriod = "3s"`

		var pc PluginConfig
		require.NoError(t, toml.Unmarshal([]byte(rawToml), &pc))

		assert.True(t, pc.IsV31())
		assert.True(t, pc.V31.VerboseLogging)
		assert.Equal(t, uint64(2), pc.V31.MaxSnapshotRounds)
		assert.Equal(t, uint64(8), pc.V31.BlobLifetimeRounds)
		assert.Equal(t, time.Second, pc.V31.MaxDurationBlobObservation.Duration())
		assert.Equal(t, uint64(4), pc.V31.BlobInFlightWaitFactor)
		assert.Equal(t, -time.Second, pc.V31.MaxBlobSnapshotAge.Duration())
		assert.Equal(t, 3*time.Second, pc.V31.MaxRoundPeriod.Duration())
		require.NoError(t, pc.V31.Validate())
	})

	t.Run("from json, duration as string or nanoseconds", func(t *testing.T) {
		var pc PluginConfig
		require.NoError(t, pc.Unmarshal([]byte(`{"v31":{"maxRoundPeriod":"2s","maxDurationBlobObservation":1500000000}}`)))
		assert.Equal(t, 2*time.Second, pc.V31.MaxRoundPeriod.Duration())
		assert.Equal(t, 1500*time.Millisecond, pc.V31.MaxDurationBlobObservation.Duration())
	})

	t.Run("round trips through json", func(t *testing.T) {
		in := V31Config{MaxRoundPeriod: Duration(2 * time.Second), MaxBlobSnapshotAge: Duration(-time.Second)}
		b, err := json.Marshal(in)
		require.NoError(t, err)
		var out V31Config
		require.NoError(t, json.Unmarshal(b, &out))
		assert.Equal(t, in, out)
	})

	t.Run("rejects an invalid duration", func(t *testing.T) {
		var pc PluginConfig
		require.Error(t, pc.Unmarshal([]byte(`{"v31":{"maxRoundPeriod":"banana"}}`)))
	})
}

func Test_V31Config_Validate(t *testing.T) {
	t.Run("zero is valid and means defaults", func(t *testing.T) {
		pc := v31Base()
		assert.True(t, pc.V31.IsZero())
		require.NoError(t, pc.Validate())
	})

	t.Run("rejects a lifetime past the maximum", func(t *testing.T) {
		pc := v31Base()
		pc.V31.BlobLifetimeRounds = 65
		err := pc.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds MaxBlobLifetimeRounds")
	})

	t.Run("rejects too little fetch margin", func(t *testing.T) {
		pc := v31Base()
		pc.V31.MaxSnapshotRounds = 8
		pc.V31.BlobLifetimeRounds = 8
		err := pc.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rounds of fetch margin")
	})

	t.Run("rejects negative durations that are not the snapshot age", func(t *testing.T) {
		pc := v31Base()
		pc.V31.MaxDurationBlobObservation = Duration(-time.Second)
		pc.V31.MaxRoundPeriod = Duration(-time.Second)
		err := pc.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MaxDurationBlobObservation must not be negative")
		assert.Contains(t, err.Error(), "MaxRoundPeriod must not be negative")
	})

	t.Run("rejects the block on OCR3.0", func(t *testing.T) {
		pc := v31Base()
		pc.PluginVersion = PluginVersionV30
		pc.V31.VerboseLogging = true
		err := pc.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `V31 config is only allowed when PluginVersion is "v31"`)
	})
}
