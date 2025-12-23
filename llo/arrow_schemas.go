package llo

import (
	"github.com/apache/arrow-go/v18/arrow"
)

// StreamValueType constants for Arrow arrays
const (
	StreamValueTypeDecimal    uint8 = 0
	StreamValueTypeQuote      uint8 = 1
	StreamValueTypeTimestampd uint8 = 2
)

// ObservationSchema defines the Arrow schema for merged observations from multiple nodes.
// This columnar format enables efficient batch aggregation across all streams.
//
// Fields:
//   - observer_id: The node that produced this observation (0-255)
//   - stream_id: The stream identifier (uint32)
//   - value_type: Type discriminator (0=Decimal, 1=Quote, 2=TimestampedStreamValue)
//   - decimal_value: Binary-encoded decimal value (shopspring/decimal format)
//   - quote_bid/benchmark/ask: Binary-encoded quote components
//   - observed_at_ns: Timestamp from provider (for TimestampedStreamValue)
//   - timestamp_ns: Node's observation timestamp
var ObservationSchema = arrow.NewSchema(
	[]arrow.Field{
		{Name: "observer_id", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		{Name: "stream_id", Type: arrow.PrimitiveTypes.Uint32, Nullable: false},
		{Name: "value_type", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		{Name: "decimal_value", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_bid", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_benchmark", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_ask", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "observed_at_ns", Type: arrow.PrimitiveTypes.Uint64, Nullable: true},
		{Name: "timestamp_ns", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
	},
	nil, // no metadata
)

// StreamAggregatesSchema defines the Arrow schema for aggregated stream values.
// Used as output from aggregation and input to report generation.
var StreamAggregatesSchema = arrow.NewSchema(
	[]arrow.Field{
		{Name: "stream_id", Type: arrow.PrimitiveTypes.Uint32, Nullable: false},
		{Name: "aggregator", Type: arrow.PrimitiveTypes.Uint32, Nullable: false},
		{Name: "value_type", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		{Name: "decimal_value", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_bid", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_benchmark", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_ask", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "observed_at_ns", Type: arrow.PrimitiveTypes.Uint64, Nullable: true},
	},
	nil,
)

// CacheSchema defines the Arrow schema for the observation cache.
// This is optimized for fast lookups by stream_id with TTL-based expiration.
var CacheSchema = arrow.NewSchema(
	[]arrow.Field{
		{Name: "stream_id", Type: arrow.PrimitiveTypes.Uint32, Nullable: false},
		{Name: "value_type", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		{Name: "decimal_value", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_bid", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_benchmark", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "quote_ask", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "observed_at_ns", Type: arrow.PrimitiveTypes.Uint64, Nullable: true},
		{Name: "expires_at_ns", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	},
	nil,
)

// TransmissionSchema defines the Arrow schema for batched report transmissions.
// Used for efficient batch encoding with Arrow IPC and compression.
//
// TODO: This schema is defined for future use in batched transmission encoding.
// Implementation pending integration with the transmission subsystem.
var TransmissionSchema = arrow.NewSchema(
	[]arrow.Field{
		{Name: "server_url", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "config_digest", Type: &arrow.FixedSizeBinaryType{ByteWidth: 32}, Nullable: false},
		{Name: "seq_nr", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "report_data", Type: arrow.BinaryTypes.LargeBinary, Nullable: false},
		{Name: "lifecycle_stage", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "report_format", Type: arrow.PrimitiveTypes.Uint32, Nullable: false},
		{Name: "signatures", Type: arrow.ListOf(arrow.BinaryTypes.Binary), Nullable: false},
		{Name: "signers", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint8), Nullable: false},
		{Name: "transmission_hash", Type: &arrow.FixedSizeBinaryType{ByteWidth: 32}, Nullable: false},
		{Name: "created_at_ns", Type: arrow.FixedWidthTypes.Timestamp_ns, Nullable: false},
	},
	nil,
)

// Column indices for ObservationSchema - for efficient column access
const (
	ObsColObserverID = iota
	ObsColStreamID
	ObsColValueType
	ObsColDecimalValue
	ObsColQuoteBid
	ObsColQuoteBenchmark
	ObsColQuoteAsk
	ObsColObservedAtNs
	ObsColTimestampNs
)

// Column indices for StreamAggregatesSchema
const (
	AggColStreamID = iota
	AggColAggregator
	AggColValueType
	AggColDecimalValue
	AggColQuoteBid
	AggColQuoteBenchmark
	AggColQuoteAsk
	AggColObservedAtNs
)

// Column indices for CacheSchema
const (
	CacheColStreamID = iota
	CacheColValueType
	CacheColDecimalValue
	CacheColQuoteBid
	CacheColQuoteBenchmark
	CacheColQuoteAsk
	CacheColObservedAtNs
	CacheColExpiresAtNs
)
