package protocol

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	sync "sync"
	"unicode/utf8"

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
// and reused until the channel is updated or removed. Callers share each decoded
// value, so they must not modify it.
type OptsCache struct {
	mu      sync.Mutex
	raw     map[llotypes.ChannelID]llotypes.ChannelOpts
	decoded map[optsCacheKey]any
	// sealed marks a cache owned by a ChannelGeneration: it was built from one
	// immutable set of channel definitions and must never be repointed at
	// another. Mutators are no-ops on a sealed cache; lazy decoding still
	// happens, which is unobservable because decoding fixed raw bytes into a
	// fixed type is deterministic.
	sealed bool
}

func NewOptsCache() *OptsCache {
	return &OptsCache{
		raw:     make(map[llotypes.ChannelID]llotypes.ChannelOpts),
		decoded: make(map[optsCacheKey]any),
	}
}

// newSealedOptsCache builds the immutable opts store for one generation of
// channel definitions. The raw bytes are fixed at construction; only the lazily
// decoded values are filled in afterwards.
func newSealedOptsCache(channelDefinitions llotypes.ChannelDefinitions) *OptsCache {
	c := &OptsCache{
		raw:     make(map[llotypes.ChannelID]llotypes.ChannelOpts, len(channelDefinitions)),
		decoded: make(map[optsCacheKey]any, len(channelDefinitions)),
		sealed:  true,
	}
	for channelID, cd := range channelDefinitions {
		c.raw[channelID] = cd.Opts
	}
	return c
}

// Set stores the raw opts for a channel and invalidates any previously decoded
// values for that channel. It is a no-op when the raw bytes are identical to
// what is already stored.
func (c *OptsCache) Set(channelID llotypes.ChannelID, raw llotypes.ChannelOpts) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sealed {
		return
	}

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

	if c.sealed {
		return
	}

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

	if c.sealed {
		return
	}

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

	if c.sealed {
		return
	}
	c.raw = make(map[llotypes.ChannelID]llotypes.ChannelOpts)
	c.decoded = make(map[optsCacheKey]any)

	for channelID, cd := range channelDefinitions {
		c.raw[channelID] = cd.Opts
	}
}

// GetOpts returns decoded channel opts of type T for the given channel.
// On the first call for a given (channelID, T) after Set, the raw bytes are
// decoded via decodeOpts and the result is cached. Subsequent calls return
// the cached value directly.
//
// The returned value shares its slices, maps and pointers with the cache and
// with every other caller. Treat it as read-only. Copy any field before
// modifying it.
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

	result, err := decodeOpts[T](raw)
	if err != nil {
		return zero, fmt.Errorf("failed to decode opts for channel %d: %w", channelID, err)
	}

	c.decoded[key] = result
	return result, nil
}

// GetOptsOrDecode returns the channel opts of type T from the cache, and
// decodes the definition's raw opts on a cache miss or a nil cache. The
// fallback keeps the result identical across oracles whether or not their
// cache holds the channel. The same read-only rule as GetOpts applies.
func GetOptsOrDecode[T any](c *OptsCache, cd llotypes.ChannelDefinition, channelID llotypes.ChannelID) (T, error) {
	if c != nil {
		if o, err := GetOpts[T](c, channelID); err == nil {
			return o, nil
		}
	}
	return decodeOpts[T](cd.Opts)
}

// decodeOpts is the single decode of raw channel opts. The cache and every
// cache miss fallback go through it, so a definition reads the same whichever
// path decodes it. JSON libraries disagree on some inputs, such as keys that
// only match a field under Unicode case folding.
func decodeOpts[T any](raw []byte) (T, error) {
	var result T
	if len(raw) == 0 {
		return result, nil
	}
	err := json.Unmarshal(raw, &result)
	return result, err
}

// VerifyOptsKeys rejects opts whose object keys, at any depth, are not ASCII or
// repeat under ASCII case folding. JSON decoders resolve such keys differently,
// so the same bytes could decode to different values in different places.
func VerifyOptsKeys(raw []byte) error {
	type object struct {
		seen      map[string]struct{}
		expectKey bool
	}
	var stack []*object

	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) > 0 {
				return fmt.Errorf("failed to scan opts: %w", io.ErrUnexpectedEOF)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to scan opts: %w", err)
		}

		var top *object
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}

		if key, ok := tok.(string); ok && top != nil && top.expectKey {
			for i := 0; i < len(key); i++ {
				if key[i] >= utf8.RuneSelf {
					return fmt.Errorf("opts key %q is not ASCII", key)
				}
			}
			folded := strings.ToLower(key)
			if _, dup := top.seen[folded]; dup {
				return fmt.Errorf("opts key %q is repeated", key)
			}
			top.seen[folded] = struct{}{}
			top.expectKey = false
			continue
		}

		switch tok {
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			continue
		}
		// Any other token starts a value, which completes the pending key.
		if top != nil && top.seen != nil {
			top.expectKey = true
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &object{seen: map[string]struct{}{}, expectKey: true})
		case json.Delim('['):
			stack = append(stack, &object{})
		}
	}
}
