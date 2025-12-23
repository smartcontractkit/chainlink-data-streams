package llo

import (
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/types"
)

// ArrowObservationMerger converts multiple node observations into a single Arrow record.
// This enables efficient batch aggregation across all streams from all observers.
type ArrowObservationMerger struct {
	pool   *ArrowBuilderPool
	codec  ObservationCodec
	logger logger.Logger
}

// NewArrowObservationMerger creates a new observation merger.
// Logger can be nil if logging is not needed.
func NewArrowObservationMerger(pool *ArrowBuilderPool, codec ObservationCodec, lggr logger.Logger) *ArrowObservationMerger {
	return &ArrowObservationMerger{
		pool:   pool,
		codec:  codec,
		logger: lggr,
	}
}

// MergeObservations converts attributed observations from multiple nodes into a single Arrow record.
// The record contains all stream values from all observers, ready for aggregation.
//
// Returns an Arrow record that must be released by the caller when done.
func (m *ArrowObservationMerger) MergeObservations(
	aos []types.AttributedObservation,
) (arrow.Record, error) {
	builder := m.pool.GetObservationBuilder()

	// Get typed builders for each column
	observerIDBuilder := builder.Field(ObsColObserverID).(*array.Uint8Builder)
	streamIDBuilder := builder.Field(ObsColStreamID).(*array.Uint32Builder)
	valueTypeBuilder := builder.Field(ObsColValueType).(*array.Uint8Builder)
	decimalBuilder := builder.Field(ObsColDecimalValue).(*array.BinaryBuilder)
	bidBuilder := builder.Field(ObsColQuoteBid).(*array.BinaryBuilder)
	benchmarkBuilder := builder.Field(ObsColQuoteBenchmark).(*array.BinaryBuilder)
	askBuilder := builder.Field(ObsColQuoteAsk).(*array.BinaryBuilder)
	observedAtBuilder := builder.Field(ObsColObservedAtNs).(*array.Uint64Builder)
	timestampBuilder := builder.Field(ObsColTimestampNs).(*array.Uint64Builder)

	// Estimate capacity: assume each observation has ~100 stream values on average
	estimatedRows := len(aos) * 100
	observerIDBuilder.Reserve(estimatedRows)
	streamIDBuilder.Reserve(estimatedRows)

	for _, ao := range aos {
		// Validate observer ID bounds before casting to uint8
		if ao.Observer > 255 {
			if m.logger != nil {
				m.logger.Warnw("Observer ID exceeds uint8 bounds", "observer", ao.Observer)
			}
			continue
		}

		// Decode the observation
		obs, err := m.codec.Decode(ao.Observation)
		if err != nil {
			if m.logger != nil {
				m.logger.Debugw("Failed to decode observation", "observer", ao.Observer, "err", err)
			}
			continue
		}

		observerID := uint8(ao.Observer)
		timestamp := obs.UnixTimestampNanoseconds

		// Add each stream value
		for streamID, sv := range obs.StreamValues {
			if sv == nil {
				continue
			}

			observerIDBuilder.Append(observerID)
			streamIDBuilder.Append(streamID)
			timestampBuilder.Append(timestamp)

			// Append the stream value to appropriate columns
			_, err := StreamValueToArrow(sv, valueTypeBuilder, decimalBuilder,
				bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
			if err != nil {
				if m.logger != nil {
					m.logger.Debugw("Failed to convert stream value", "streamID", streamID, "observer", observerID, "err", err)
				}
				continue
			}
		}
	}

	record := builder.NewRecord()
	m.pool.PutObservationBuilder(builder)
	return record, nil
}

// MergeStreamValues merges stream values from multiple sources into a single Arrow record.
// This is useful for merging cached values with new observations.
func (m *ArrowObservationMerger) MergeStreamValues(
	valuesByObserver map[uint8]StreamValues,
	timestamps map[uint8]uint64,
) (arrow.Record, error) {
	builder := m.pool.GetObservationBuilder()

	observerIDBuilder := builder.Field(ObsColObserverID).(*array.Uint8Builder)
	streamIDBuilder := builder.Field(ObsColStreamID).(*array.Uint32Builder)
	valueTypeBuilder := builder.Field(ObsColValueType).(*array.Uint8Builder)
	decimalBuilder := builder.Field(ObsColDecimalValue).(*array.BinaryBuilder)
	bidBuilder := builder.Field(ObsColQuoteBid).(*array.BinaryBuilder)
	benchmarkBuilder := builder.Field(ObsColQuoteBenchmark).(*array.BinaryBuilder)
	askBuilder := builder.Field(ObsColQuoteAsk).(*array.BinaryBuilder)
	observedAtBuilder := builder.Field(ObsColObservedAtNs).(*array.Uint64Builder)
	timestampBuilder := builder.Field(ObsColTimestampNs).(*array.Uint64Builder)

	for observerID, values := range valuesByObserver {
		timestamp := timestamps[observerID]

		for streamID, sv := range values {
			if sv == nil {
				continue
			}

			observerIDBuilder.Append(observerID)
			streamIDBuilder.Append(streamID)
			timestampBuilder.Append(timestamp)

			_, err := StreamValueToArrow(sv, valueTypeBuilder, decimalBuilder,
				bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
			if err != nil {
				continue
			}
		}
	}

	record := builder.NewRecord()
	m.pool.PutObservationBuilder(builder)
	return record, nil
}

// FilterByStreamIDs creates a new record containing only the specified stream IDs.
// This is useful for extracting relevant streams for a specific channel.
// If pool is nil, a new temporary pool will be created (less efficient).
func FilterByStreamIDs(record arrow.Record, streamIDs []uint32, pool *LLOMemoryPool) arrow.Record {
	if record == nil || record.NumRows() == 0 || len(streamIDs) == 0 {
		return nil
	}

	// Build a set for fast lookup
	streamIDSet := make(map[uint32]struct{}, len(streamIDs))
	for _, id := range streamIDs {
		streamIDSet[id] = struct{}{}
	}

	streamIDArr := record.Column(ObsColStreamID).(*array.Uint32)

	// Find matching indices
	indices := make([]int, 0, record.NumRows())
	for i := 0; i < int(record.NumRows()); i++ {
		if _, ok := streamIDSet[streamIDArr.Value(i)]; ok {
			indices = append(indices, i)
		}
	}

	if len(indices) == 0 {
		return nil
	}

	// Build filtered record
	// Note: In production, you'd use Arrow's Take kernel for efficiency
	return takeRows(record, indices, pool)
}

// takeRows creates a new record with only the specified row indices.
// This is a simplified implementation; production code should use Arrow compute.
// If pool is nil, a new temporary pool will be created (less efficient).
func takeRows(record arrow.Record, indices []int, pool *LLOMemoryPool) arrow.Record {
	if len(indices) == 0 {
		return nil
	}

	// Use provided pool or create a temporary one
	if pool == nil {
		pool = NewLLOMemoryPool(0)
	}

	// Create new builders for each column
	schema := record.Schema()
	builders := make([]array.Builder, schema.NumFields())

	for i := 0; i < schema.NumFields(); i++ {
		builders[i] = array.NewBuilder(pool, schema.Field(i).Type)
	}

	// Copy selected rows
	for _, idx := range indices {
		for colIdx := 0; colIdx < int(record.NumCols()); colIdx++ {
			col := record.Column(colIdx)
			appendValue(builders[colIdx], col, idx)
		}
	}

	// Build arrays
	arrays := make([]arrow.Array, len(builders))
	for i, b := range builders {
		arrays[i] = b.NewArray()
		b.Release()
	}

	return array.NewRecord(schema, arrays, int64(len(indices)))
}

// appendValue appends a single value from an array to a builder.
// Uses safe type assertions to prevent panics on type mismatches.
func appendValue(builder array.Builder, arr arrow.Array, idx int) {
	if arr.IsNull(idx) {
		builder.AppendNull()
		return
	}

	switch b := builder.(type) {
	case *array.Uint8Builder:
		if a, ok := arr.(*array.Uint8); ok {
			b.Append(a.Value(idx))
		} else {
			builder.AppendNull()
		}
	case *array.Uint32Builder:
		if a, ok := arr.(*array.Uint32); ok {
			b.Append(a.Value(idx))
		} else {
			builder.AppendNull()
		}
	case *array.Uint64Builder:
		if a, ok := arr.(*array.Uint64); ok {
			b.Append(a.Value(idx))
		} else {
			builder.AppendNull()
		}
	case *array.Int64Builder:
		if a, ok := arr.(*array.Int64); ok {
			b.Append(a.Value(idx))
		} else {
			builder.AppendNull()
		}
	case *array.BinaryBuilder:
		if a, ok := arr.(*array.Binary); ok {
			b.Append(a.Value(idx))
		} else {
			builder.AppendNull()
		}
	case *array.StringBuilder:
		if a, ok := arr.(*array.String); ok {
			b.Append(a.Value(idx))
		} else {
			builder.AppendNull()
		}
	default:
		builder.AppendNull()
	}
}

// CountByStreamID returns the count of observations per stream ID.
// Useful for validation and debugging.
func CountByStreamID(record arrow.Record) map[uint32]int {
	if record == nil || record.NumRows() == 0 {
		return nil
	}

	streamIDArr := record.Column(ObsColStreamID).(*array.Uint32)
	counts := make(map[uint32]int)

	for i := 0; i < int(record.NumRows()); i++ {
		counts[streamIDArr.Value(i)]++
	}

	return counts
}

// CountByObserver returns the count of observations per observer.
// Useful for validation and debugging.
func CountByObserver(record arrow.Record) map[uint8]int {
	if record == nil || record.NumRows() == 0 {
		return nil
	}

	observerIDArr := record.Column(ObsColObserverID).(*array.Uint8)
	counts := make(map[uint8]int)

	for i := 0; i < int(record.NumRows()); i++ {
		counts[observerIDArr.Value(i)]++
	}

	return counts
}
