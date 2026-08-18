package protocol

import (
	"testing"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testOptsA struct {
	FeedID string `json:"feedID"`
	Value  int    `json:"value"`
}

type testOptsB struct {
	Multiplier int `json:"multiplier"`
}

func TestOptsCache_Len(t *testing.T) {
	t.Run("new cache is empty", func(t *testing.T) {
		cache := NewOptsCache()
		assert.Equal(t, 0, cache.Len())
	})

	t.Run("Len reflects number of channels", func(t *testing.T) {
		cache := NewOptsCache()
		assert.Equal(t, 0, cache.Len())

		cache.Set(1, []byte(`{}`))
		assert.Equal(t, 1, cache.Len())

		cache.Set(2, []byte(`{}`))
		assert.Equal(t, 2, cache.Len())

		cache.Set(3, []byte(`{}`))
		assert.Equal(t, 3, cache.Len())
	})

	t.Run("Remove decreases Len", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{}`))
		cache.Set(2, []byte(`{}`))
		assert.Equal(t, 2, cache.Len())

		cache.Remove(1)
		assert.Equal(t, 1, cache.Len())

		cache.Remove(2)
		assert.Equal(t, 0, cache.Len())
	})

	t.Run("Set same channel with identical bytes does not change Len", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"x"}`))
		assert.Equal(t, 1, cache.Len())
		cache.Set(1, []byte(`{"feedID":"x"}`))
		assert.Equal(t, 1, cache.Len())
	})

	t.Run("Set same channel with different bytes does not change Len", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"a"}`))
		assert.Equal(t, 1, cache.Len())
		cache.Set(1, []byte(`{"feedID":"b"}`))
		assert.Equal(t, 1, cache.Len())
	})
}

func TestOptsCache_GetOpts(t *testing.T) {
	t.Run("missing channel returns error", func(t *testing.T) {
		cache := NewOptsCache()
		_, err := GetOpts[testOptsA](cache, 1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not in opts cache")
	})

	t.Run("decodes and caches on first access", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"eth-usd","value":42}`))

		r1, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "eth-usd", r1.FeedID)
		assert.Equal(t, 42, r1.Value)

		r2, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, r1, r2)
	})

	t.Run("empty raw bytes returns zero value", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, nil)

		r, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, testOptsA{}, r)
	})

	t.Run("different types for same channel are independent", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"eth","value":1,"multiplier":10}`))

		a, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "eth", a.FeedID)

		b, err := GetOpts[testOptsB](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, 10, b.Multiplier)
	})

	t.Run("different channels are independent", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"eth"}`))
		cache.Set(2, []byte(`{"feedID":"btc"}`))

		r1, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "eth", r1.FeedID)

		r2, err := GetOpts[testOptsA](cache, 2)
		require.NoError(t, err)
		assert.Equal(t, "btc", r2.FeedID)
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`not-json`))

		_, err := GetOpts[testOptsA](cache, 1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode")
	})
}

func TestOptsCache_Set(t *testing.T) {
	t.Run("Set invalidates decoded entries", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"v1"}`))

		r1, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "v1", r1.FeedID)

		cache.Set(1, []byte(`{"feedID":"v2"}`))

		r2, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "v2", r2.FeedID)
	})

	t.Run("Set with identical bytes is a no-op", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"same"}`))

		r1, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "same", r1.FeedID)

		// Set again with the same bytes — decoded cache should survive
		cache.Set(1, []byte(`{"feedID":"same"}`))

		// Should still be the same cached object (no re-decode)
		r2, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "same", r2.FeedID)
	})
}

func TestOptsCache_Remove(t *testing.T) {
	t.Run("removes raw and decoded entries", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"eth"}`))
		cache.Set(2, []byte(`{"feedID":"btc"}`))

		_, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		_, err = GetOpts[testOptsA](cache, 2)
		require.NoError(t, err)

		cache.Remove(1)

		_, err = GetOpts[testOptsA](cache, 1)
		require.Error(t, err, "channel 1 should be gone")

		r, err := GetOpts[testOptsA](cache, 2)
		require.NoError(t, err, "channel 2 should remain")
		assert.Equal(t, "btc", r.FeedID)
	})

	t.Run("remove then re-set works", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"old"}`))
		_, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)

		cache.Remove(1)
		cache.Set(1, []byte(`{"feedID":"new"}`))

		r, err := GetOpts[testOptsA](cache, 1)
		require.NoError(t, err)
		assert.Equal(t, "new", r.FeedID)
	})
}

func TestOptsCache_ResetTo(t *testing.T) {
	t.Run("replaces all entries with channel definitions", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"old1"}`))
		cache.Set(2, []byte(`{"feedID":"old2"}`))
		cache.Set(3, []byte(`{"feedID":"old3"}`))
		_, _ = GetOpts[testOptsA](cache, 1)
		_, _ = GetOpts[testOptsA](cache, 2)
		require.Equal(t, 3, cache.Len())

		cache.ResetTo(llotypes.ChannelDefinitions{
			2: {Opts: llotypes.ChannelOpts(`{"feedID":"ch2"}`)},
			4: {Opts: llotypes.ChannelOpts(`{"feedID":"ch4"}`)},
		})

		assert.Equal(t, 2, cache.Len())
		_, err := GetOpts[testOptsA](cache, 1)
		require.Error(t, err, "channel 1 should be gone after reset")
		r2, err := GetOpts[testOptsA](cache, 2)
		require.NoError(t, err)
		assert.Equal(t, "ch2", r2.FeedID)
		_, err = GetOpts[testOptsA](cache, 3)
		require.Error(t, err, "channel 3 should be gone after reset")
		r4, err := GetOpts[testOptsA](cache, 4)
		require.NoError(t, err)
		assert.Equal(t, "ch4", r4.FeedID)
	})

	t.Run("empty definitions clears cache", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"x"}`))
		require.Equal(t, 1, cache.Len())

		cache.ResetTo(nil)
		assert.Equal(t, 0, cache.Len())
		_, err := GetOpts[testOptsA](cache, 1)
		require.Error(t, err)
	})

	t.Run("empty map clears cache", func(t *testing.T) {
		cache := NewOptsCache()
		cache.Set(1, []byte(`{"feedID":"x"}`))
		require.Equal(t, 1, cache.Len())

		cache.ResetTo(llotypes.ChannelDefinitions{})
		assert.Equal(t, 0, cache.Len())
		_, err := GetOpts[testOptsA](cache, 1)
		require.Error(t, err)
	})
}

func TestOptsCache_ChannelDefinitionWorkflow(t *testing.T) {
	cache := NewOptsCache()
	defs := llotypes.ChannelDefinitions{
		1: {Opts: llotypes.ChannelOpts(`{"feedID":"ch1"}`)},
		2: {Opts: llotypes.ChannelOpts(`{"feedID":"ch2"}`)},
		3: {Opts: llotypes.ChannelOpts(`{"feedID":"ch3"}`)},
	}

	// Populate cache from initial definitions
	for cid, cd := range defs {
		cache.Set(cid, cd.Opts)
	}

	r1, err := GetOpts[testOptsA](cache, 1)
	require.NoError(t, err)
	assert.Equal(t, "ch1", r1.FeedID)

	// Update channel 2
	cache.Set(2, []byte(`{"feedID":"ch2-updated"}`))
	r2, err := GetOpts[testOptsA](cache, 2)
	require.NoError(t, err)
	assert.Equal(t, "ch2-updated", r2.FeedID)

	// Remove channel 3
	cache.Remove(3)
	_, err = GetOpts[testOptsA](cache, 3)
	require.Error(t, err)

	// Channel 1 unchanged
	r1Again, err := GetOpts[testOptsA](cache, 1)
	require.NoError(t, err)
	assert.Equal(t, "ch1", r1Again.FeedID)
}

func Test_OptsCache_SyncTo_GatesOnSeqNr(t *testing.T) {
	defs := func(raw string) llotypes.ChannelDefinitions {
		return llotypes.ChannelDefinitions{1: {Opts: llotypes.ChannelOpts(raw)}}
	}
	type vOpts struct {
		V int `json:"v"`
	}
	c := NewOptsCache()

	c.SyncTo(5, defs(`{"v":1}`))
	got, err := GetOpts[vOpts](c, 1)
	require.NoError(t, err)
	require.Equal(t, 1, got.V)

	// Same seqNr: the record is a pure function of its sequence number, so this
	// returns without inspecting the definitions at all.
	c.SyncTo(5, defs(`{"v":2}`))
	got, err = GetOpts[vOpts](c, 1)
	require.NoError(t, err)
	require.Equal(t, 1, got.V, "a matching seqNr must short-circuit")

	// A different seqNr syncs, in either direction (replay may present an older
	// record).
	c.SyncTo(6, defs(`{"v":2}`))
	got, err = GetOpts[vOpts](c, 1)
	require.NoError(t, err)
	require.Equal(t, 2, got.V)

	c.SyncTo(4, defs(`{"v":3}`))
	got, err = GetOpts[vOpts](c, 1)
	require.NoError(t, err)
	require.Equal(t, 3, got.V, "an older seqNr must still sync")

	// Direct mutation clears the watermark, so a later SyncTo at the same seqNr
	// cannot be short-circuited into serving mutated content.
	c.SyncTo(9, defs(`{"v":4}`))
	c.Set(1, llotypes.ChannelOpts(`{"v":99}`))
	c.SyncTo(9, defs(`{"v":4}`))
	got, err = GetOpts[vOpts](c, 1)
	require.NoError(t, err)
	require.Equal(t, 4, got.V, "Set must invalidate the watermark")

	c.SyncTo(10, defs(`{"v":5}`))
	c.Remove(1)
	c.SyncTo(10, defs(`{"v":5}`))
	got, err = GetOpts[vOpts](c, 1)
	require.NoError(t, err)
	require.Equal(t, 5, got.V, "Remove must invalidate the watermark")

	c.SyncTo(11, defs(`{"v":6}`))
	c.ResetTo(llotypes.ChannelDefinitions{})
	c.SyncTo(11, defs(`{"v":6}`))
	got, err = GetOpts[vOpts](c, 1)
	require.NoError(t, err)
	require.Equal(t, 6, got.V, "ResetTo must invalidate the watermark")

	// Channels absent from the definitions are dropped.
	c.SyncTo(12, llotypes.ChannelDefinitions{})
	_, err = GetOpts[vOpts](c, 1)
	require.Error(t, err)
}
