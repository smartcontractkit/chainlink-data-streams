package llo

import (
	"fmt"
	"testing"

	"github.com/goccy/go-json"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// =============================================================================
// Benchmark: Direct JSON Parsing vs Cache Lookup
// =============================================================================
//
// These benchmarks demonstrate why we cache parsed channel opts instead of
// parsing JSON on every access. Run with:
//
//   go test . -bench=BenchmarkChannelOptsCache -benchmem -run=NONE
//
// Expected results (approximate):
//
//   DirectParse (JSON):  ~600-800 ns/op  |  640 B/op  |  6 allocs
//   CacheGet:            ~15-25 ns/op    |  0 B/op    |  0 allocs
//   Speedup:             ~40-50x faster, zero allocations
//
// =============================================================================

// Realistic opts JSON for existing codecs that use JSON opts format
var benchmarkOptsJSON = []byte(`{"feedID":"0x0001020304050607080910111213141516171819202122232425262728293031","baseUSDFee":"1.5","expirationWindow":3600,"timeResolution":"ns","abi":[{"type":"int192","expression":"Sum(s1,s2)","expressionStreamId":100},{"type":"int192"},{"type":"uint256"}]}`)

// benchParsedOpts mirrors existing codec structures
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

// benchMockCodec implements OptsParser for benchmarks
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

// BenchmarkChannelOptsCache_DirectParse measures the cost of parsing opts directly.
// Note: actual parsing cost depends on codec implementation (JSON, protobuf, etc.)
// This benchmark uses JSON as representative of current codecs.
func BenchmarkChannelOptsCache_DirectParse(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var opts benchParsedOpts
		_ = json.Unmarshal(benchmarkOptsJSON, &opts)
	}
}

// BenchmarkChannelOptsCache_CacheGet measures the cost of a cache lookup + type assertion.
// This is the required usage pattern for the cache.
func BenchmarkChannelOptsCache_CacheGet(b *testing.B) {
	cache := NewChannelDefinitionOptsCache()
	codec := benchMockCodec{}

	if err := cache.Set(1, benchmarkOptsJSON, codec); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	// Usage pattern is 1) Get and 2) type assertion
	for i := 0; i < b.N; i++ {
		val, ok := cache.Get(1)
		if ok {
			_ = val.(benchParsedOpts)
		}
	}
}
