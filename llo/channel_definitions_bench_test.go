package llo

import (
	"fmt"
	"testing"

	"github.com/goccy/go-json"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// Benchmark the cost of parsing JSON vs using the cache.
//
// Run with:
//   go test . -bench=BenchmarkChannelOptsCache -benchmem -run=NONE
//
// Expected results (approximate):
//
//   DirectParse:  ~600-800 ns/op  |  640 B/op  |  6 allocs
//   CacheGet:     ~15-25 ns/op    |  0 B/op    |  0 allocs
//   Speedup:      ~40-50x faster, zero allocations
//
// =============================================================================

// Example opts for existing codecs that use JSON opts format
var benchmarkOptsJSON = []byte(`{"feedID":"0x0001020304050607080910111213141516171819202122232425262728293031","baseUSDFee":"1.5","expirationWindow":3600,"timeResolution":"ns","abi":[{"type":"int192","expression":"Sum(s1,s2)","expressionStreamId":100},{"type":"int192"},{"type":"uint256"}]}`)

type benchParsedOpts struct {
	FeedID           string     `json:"feedID"`
	BaseUSDFee       string     `json:"baseUSDFee"`
	ExpirationWindow uint32     `json:"expirationWindow"`
	TimeResolution   string     `json:"timeResolution,omitempty"`
	ABI              []abiEntry `json:"abi"`
}

type abiEntry struct {
	Type               string `json:"type"`
	Expression         string `json:"expression,omitempty"`
	ExpressionStreamID uint32 `json:"expressionStreamId,omitempty"`
}

type benchMockCodec struct{}

func (benchMockCodec) Encode(Report, llotypes.ChannelDefinition, any) ([]byte, error) {
	return nil, nil
}
func (benchMockCodec) Verify(llotypes.ChannelDefinition) error { return nil }
func (benchMockCodec) ParseOpts(opts []byte) (any, error) {
	var parsed benchParsedOpts
	if err := json.Unmarshal(opts, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse opts: %w", err)
	}
	return parsed, nil
}

func BenchmarkChannelOptsCache(b *testing.B) {
	b.Run("DirectParse", func(b *testing.B) {
		// Measures the cost of parsing JSON directly.
		// This is the usage pattern without caching.
		for i := 0; i < b.N; i++ {
			var opts benchParsedOpts
			_ = json.Unmarshal(benchmarkOptsJSON, &opts)
		}
	})

	b.Run("CacheGet", func(b *testing.B) {
		// Measures the cost of cache lookup + type assertion.
		// This is the required usage pattern for the cache.
		cache := NewChannelDefinitionOptsCache()
		codec := benchMockCodec{}

		if err := cache.Set(1, benchmarkOptsJSON, codec); err != nil {
			b.Fatal(err)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			val, ok := cache.Get(1)
			if ok {
				_ = val.(benchParsedOpts)
			}
		}
	})
}
