# Arrow Columnar Data Processing

This document explains the Apache Arrow implementation introduced for efficient batch aggregation of oracle observations in the LLO (Low-Latency Oracle) system.

## Overview

### Why Arrow?

The Arrow implementation addresses key performance challenges:

1. **Memory Efficiency** - Replaces the 1GB static memory ballast with controlled, bounded allocation
2. **Batch Processing** - Enables efficient columnar operations on thousands of observations simultaneously
3. **Reduced Allocations** - Builder pooling minimizes GC pressure during repeated aggregation cycles

### Dependencies

```go
github.com/apache/arrow-go/v18 v18.3.1
```

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Arrow Data Pipeline                                │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Node Observations                                                           │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐                                     │
│  │ Observer │ │ Observer │ │ Observer │  ...                                │
│  │    0     │ │    1     │ │    N     │                                     │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘                                     │
│       │            │            │                                            │
│       └────────────┼────────────┘                                            │
│                    ▼                                                         │
│       ┌────────────────────────┐                                            │
│       │ ArrowObservationMerger │                                            │
│       │   MergeObservations()  │                                            │
│       └───────────┬────────────┘                                            │
│                   ▼                                                          │
│       ┌────────────────────────┐                                            │
│       │    Arrow Record        │  (ObservationSchema)                       │
│       │  [observer_id, stream_id, value_type, values...]                    │
│       └───────────┬────────────┘                                            │
│                   ▼                                                          │
│       ┌────────────────────────┐                                            │
│       │   ArrowAggregator      │                                            │
│       │ AggregateObservations()│                                            │
│       └───────────┬────────────┘                                            │
│                   ▼                                                          │
│       ┌────────────────────────┐                                            │
│       │   StreamAggregates     │  (per-stream aggregated values)            │
│       └────────────────────────┘                                            │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Schemas

Four Arrow schemas are defined in `llo/arrow_schemas.go`:

### 1. ObservationSchema

Stores merged observations from all nodes for batch aggregation.

| Column | Type | Description |
|--------|------|-------------|
| `observer_id` | uint8 | Node that produced the observation (0-255) |
| `stream_id` | uint32 | Stream identifier |
| `value_type` | uint8 | Type discriminator (see Value Types) |
| `decimal_value` | binary | Encoded decimal value |
| `quote_bid` | binary | Quote bid component |
| `quote_benchmark` | binary | Quote benchmark component |
| `quote_ask` | binary | Quote ask component |
| `observed_at_ns` | uint64 | Provider timestamp (nanoseconds) |
| `timestamp_ns` | uint64 | Node observation timestamp |

### 2. StreamAggregatesSchema

Output from aggregation, input to report generation.

| Column | Type | Description |
|--------|------|-------------|
| `stream_id` | uint32 | Stream identifier |
| `aggregator` | uint32 | Aggregator type used |
| `value_type` | uint8 | Type discriminator |
| `decimal_value` | binary | Aggregated decimal |
| `quote_*` | binary | Aggregated quote components |
| `observed_at_ns` | uint64 | Observation timestamp |

### 3. CacheSchema

Observation cache with TTL-based expiration.

| Column | Type | Description |
|--------|------|-------------|
| `stream_id` | uint32 | Stream identifier |
| `value_type` | uint8 | Type discriminator |
| `decimal_value` | binary | Cached decimal |
| `quote_*` | binary | Cached quote components |
| `observed_at_ns` | uint64 | Observation timestamp |
| `expires_at_ns` | int64 | TTL expiration timestamp |

### 4. TransmissionSchema

Batched report transmissions with Arrow IPC compression.

| Column | Type | Description |
|--------|------|-------------|
| `server_url` | string | Destination server |
| `config_digest` | fixed[32] | Configuration hash |
| `seq_nr` | uint64 | Sequence number |
| `report_data` | large_binary | Encoded report |
| `lifecycle_stage` | string | Report lifecycle stage |
| `report_format` | uint32 | Format identifier |
| `signatures` | list<binary> | Report signatures |
| `signers` | list<uint8> | Signer indices |
| `transmission_hash` | fixed[32] | Transmission hash |
| `created_at_ns` | timestamp[ns] | Creation time |

## Value Types

Three value types are supported, identified by `value_type` column:

```go
const (
    StreamValueTypeDecimal    uint8 = 0  // Single decimal value
    StreamValueTypeQuote      uint8 = 1  // Quote with bid/benchmark/ask
    StreamValueTypeTimestampd uint8 = 2  // Decimal with observation timestamp
)
```

### Decimal Encoding

Values use `shopspring/decimal` binary encoding for precise representation:

```go
// Encode
bytes, _ := decimal.MarshalBinary()

// Decode
var d decimal.Decimal
d.UnmarshalBinary(bytes)
```

## Core Components

### arrow_schemas.go

Defines all four Arrow schemas and column index constants for type-safe access:

```go
// Column indices for ObservationSchema
const (
    ObsColObserverID = iota
    ObsColStreamID
    ObsColValueType
    // ...
)
```

### arrow_pool.go

Memory management with two pool types:

**LLOMemoryPool** - Wraps Arrow's allocator with metrics and optional bounds:

```go
pool := NewLLOMemoryPool(maxBytes)  // 0 for unlimited
allocated, allocs, releases := pool.Metrics()
```

**ArrowBuilderPool** - Unified pool for all schema builders:

```go
builderPool := NewArrowBuilderPool(maxMemoryBytes)

// Get a builder for observations
builder := builderPool.GetObservationBuilder()
// ... use builder ...
builderPool.PutObservationBuilder(builder)
```

### arrow_converters.go

Type conversion between Go types and Arrow columns:

```go
// Write StreamValue to Arrow builders
StreamValueToArrow(sv, valueTypeBuilder, decimalBuilder, bidBuilder, ...)

// Read StreamValue from Arrow arrays
sv, _ := ArrowToStreamValue(idx, valueTypeArr, decimalArr, bidArr, ...)

// Batch conversion for cache operations
record, _ := StreamValuesToArrowRecord(values, pool)
values, _ := ArrowRecordToStreamValues(record)
```

### arrow_observation_merger.go

Merges observations from multiple nodes into a single Arrow record:

```go
merger := NewArrowObservationMerger(pool, codec)

// Merge attributed observations from consensus
record, _ := merger.MergeObservations(attributedObservations)
defer record.Release()

// Utility functions
counts := CountByStreamID(record)   // {streamID: count}
counts := CountByObserver(record)   // {observerID: count}
```

### arrow_aggregators.go

Performs vectorized aggregation on Arrow records:

```go
aggregator := NewArrowAggregator(pool)

// Aggregate with channel definitions providing aggregator type per stream
results, _ := aggregator.AggregateObservations(record, channelDefs, f)
// f = fault tolerance threshold (observations must exceed f)
```

**Supported Aggregators:**

| Aggregator | Description |
|------------|-------------|
| `Median` | Sorts values, returns middle element |
| `Mode` | Most common value (requires f+1 agreement) |
| `Quote` | Median of each quote component separately |

## Data Flow Example

```go
// 1. Create pools
builderPool := NewArrowBuilderPool(0)
codec := &StandardObservationCodec{}

// 2. Merge observations from all nodes
merger := NewArrowObservationMerger(builderPool, codec)
record, _ := merger.MergeObservations(attributedObservations)
defer record.Release()

// 3. Aggregate using channel definitions
aggregator := NewArrowAggregator(builderPool)
streamAggregates, _ := aggregator.AggregateObservations(record, channelDefs, f)

// 4. Use aggregated values for report generation
for streamID, aggregatorValues := range streamAggregates {
    for aggregatorType, value := range aggregatorValues {
        // Build reports...
    }
}
```

## Memory Management Best Practices

1. **Always release records** when done:
   ```go
   record, _ := merger.MergeObservations(...)
   defer record.Release()
   ```

2. **Return builders to pool** after use:
   ```go
   builder := pool.GetObservationBuilder()
   // ... use builder ...
   pool.PutObservationBuilder(builder)
   ```

3. **Set memory limits** in production:
   ```go
   pool := NewArrowBuilderPool(500 * 1024 * 1024)  // 500MB limit
   ```

4. **Monitor allocation metrics**:
   ```go
   allocated, allocs, releases := pool.MemoryStats()
   ```

## Testing

### Unit Tests

`llo/arrow_aggregators_test.go` - Validates aggregation logic for all value types and aggregator combinations.

### Benchmarks

`llo/arrow_bench_test.go` - Performance benchmarks for:
- Median aggregation
- Quote aggregation
- Type conversion operations
- Builder pool efficiency

Run benchmarks:
```bash
cd llo
go test -bench=. -benchmem ./...
```

### Comparison Tests

`llo/arrow_comparison_test.go` - Compares Arrow implementation against the original implementation at various scales:
- 10, 100, 1000, 5000, 10000 observations

Run comparison:
```bash
go test -run=Comparison -v ./llo/
```

## File Reference

| File | Purpose |
|------|---------|
| `llo/arrow_schemas.go` | Schema definitions and column constants |
| `llo/arrow_pool.go` | Memory pool and builder pool management |
| `llo/arrow_converters.go` | Go type <-> Arrow conversion utilities |
| `llo/arrow_observation_merger.go` | Multi-node observation merging |
| `llo/arrow_aggregators.go` | Vectorized aggregation algorithms |
| `llo/arrow_aggregators_test.go` | Unit tests |
| `llo/arrow_bench_test.go` | Performance benchmarks |
| `llo/arrow_comparison_test.go` | Before/after comparison tests |
