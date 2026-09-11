package protocol

import (
	"bytes"
	"fmt"
	"reflect"
	sync "sync"

	"github.com/goccy/go-json"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type optsCacheKey struct {
	channelID llotypes.ChannelID
	optsType  reflect.Type
}

// OptsCache caches decoded channel definition options keyed by (ChannelID, target type).
// Raw opts bytes are stored via Set during channel definition changes in Outcome().
// Decoded values are produced lazily on the first GetOpts call for a given (channelID, type)
// and reused until the channel is updated or removed.
type OptsCache struct {
	mu      sync.Mutex
	raw     map[llotypes.ChannelID]llotypes.ChannelOpts
	decoded map[optsCacheKey]any
}

func NewOptsCache() *OptsCache {
	return &OptsCache{
		raw:     make(map[llotypes.ChannelID]llotypes.ChannelOpts),
		decoded: make(map[optsCacheKey]any),
	}
}

// Set stores the raw opts for a channel and invalidates any previously decoded
// values for that channel. It is a no-op when the raw bytes are identical to
// what is already stored.
func (c *OptsCache) Set(channelID llotypes.ChannelID, raw llotypes.ChannelOpts) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.raw[channelID]; ok && bytes.Equal(existing, raw) {
		return
	}
	c.raw[channelID] = raw
	c.invalidateDecoded(channelID)
}

// Len returns the number of channels in the cache.
func (c *OptsCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.raw)
}

// Remove removes all raw and decoded data for a channel.
func (c *OptsCache) Remove(channelID llotypes.ChannelID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.raw, channelID)
	c.invalidateDecoded(channelID)
}

// invalidateDecoded drops every decoded value for a channel. Callers must hold c.mu.
func (c *OptsCache) invalidateDecoded(channelID llotypes.ChannelID) {
	for key := range c.decoded {
		if key.channelID == channelID {
			delete(c.decoded, key)
		}
	}
}

// SyncTo makes the cache's contents match channelDefinitions exactly.
//
// Unlike ResetTo it compares raw opts by content, so already decoded values
// survive for channels whose opts are unchanged. That makes it cheap enough to
// call on every round in order to guarantee the cache agrees with a given set
// of channel definitions, rather than relying on incremental Set/Remove calls
// having kept it in step.
func (c *OptsCache) SyncTo(channelDefinitions llotypes.ChannelDefinitions) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for channelID, cd := range channelDefinitions {
		if existing, ok := c.raw[channelID]; ok && bytes.Equal(existing, cd.Opts) {
			continue
		}
		c.raw[channelID] = cd.Opts
		c.invalidateDecoded(channelID)
	}

	// Drop channels that are no longer defined. Deleting during a range over
	// the same map is safe in Go.
	for channelID := range c.raw {
		if _, ok := channelDefinitions[channelID]; !ok {
			delete(c.raw, channelID)
			c.invalidateDecoded(channelID)
		}
	}
}

// ResetTo resets the cache to the given channel definitions.
func (c *OptsCache) ResetTo(channelDefinitions llotypes.ChannelDefinitions) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw = make(map[llotypes.ChannelID]llotypes.ChannelOpts)
	c.decoded = make(map[optsCacheKey]any)

	for channelID, cd := range channelDefinitions {
		c.raw[channelID] = cd.Opts
	}
}

// GetOpts returns decoded channel opts of type T for the given channel.
// On the first call for a given (channelID, T) after Set, the raw bytes are
// decoded via json.Unmarshal and the result is cached. Subsequent calls return
// the cached value directly.
//
// Returns an error if the channel is not in the cache or decoding fails.
// The caller must pass a valid opts cache.
func GetOpts[T any](c *OptsCache, channelID llotypes.ChannelID) (T, error) {
	var zero T
	c.mu.Lock()
	defer c.mu.Unlock()

	key := optsCacheKey{
		channelID: channelID,
		optsType:  reflect.TypeFor[T](),
	}

	if entry, ok := c.decoded[key]; ok {
		return entry.(T), nil
	}

	raw, ok := c.raw[channelID]
	if !ok {
		return zero, fmt.Errorf("channel %d not in opts cache", channelID)
	}

	var result T
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return zero, fmt.Errorf("failed to decode opts for channel %d: %w", channelID, err)
		}
	}

	c.decoded[key] = result
	return result, nil
}
