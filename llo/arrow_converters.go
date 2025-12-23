package llo

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/shopspring/decimal"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// maxDecimalBytes is the maximum size of a binary-encoded decimal.
// This limit prevents potential DoS from oversized input data.
const maxDecimalBytes = 256

// DecimalToBytes converts a shopspring decimal to its binary representation.
// This uses the decimal's native MarshalBinary for consistent serialization.
func DecimalToBytes(d decimal.Decimal) ([]byte, error) {
	return d.MarshalBinary()
}

// BytesToDecimal converts bytes back to a shopspring decimal.
// Returns an error if the input exceeds maxDecimalBytes.
func BytesToDecimal(b []byte) (decimal.Decimal, error) {
	if len(b) > maxDecimalBytes {
		return decimal.Decimal{}, fmt.Errorf("decimal bytes exceed max size: %d > %d", len(b), maxDecimalBytes)
	}
	var d decimal.Decimal
	err := d.UnmarshalBinary(b)
	return d, err
}

// StreamValueToArrow appends a StreamValue to the appropriate Arrow builder fields.
// Returns the value type that was appended.
func StreamValueToArrow(
	sv StreamValue,
	valueTypeBuilder *array.Uint8Builder,
	decimalBuilder *array.BinaryBuilder,
	bidBuilder *array.BinaryBuilder,
	benchmarkBuilder *array.BinaryBuilder,
	askBuilder *array.BinaryBuilder,
	observedAtBuilder *array.Uint64Builder,
) (uint8, error) {
	if sv == nil {
		// Null value - append nulls to all fields
		valueTypeBuilder.AppendNull()
		decimalBuilder.AppendNull()
		bidBuilder.AppendNull()
		benchmarkBuilder.AppendNull()
		askBuilder.AppendNull()
		observedAtBuilder.AppendNull()
		return 0, nil
	}

	switch v := sv.(type) {
	case *Decimal:
		valueTypeBuilder.Append(StreamValueTypeDecimal)
		b, err := DecimalToBytes(v.Decimal())
		if err != nil {
			return 0, err
		}
		decimalBuilder.Append(b)
		bidBuilder.AppendNull()
		benchmarkBuilder.AppendNull()
		askBuilder.AppendNull()
		observedAtBuilder.AppendNull()
		return StreamValueTypeDecimal, nil

	case *Quote:
		valueTypeBuilder.Append(StreamValueTypeQuote)
		decimalBuilder.AppendNull()

		bidBytes, err := DecimalToBytes(v.Bid)
		if err != nil {
			return 0, err
		}
		bidBuilder.Append(bidBytes)

		benchmarkBytes, err := DecimalToBytes(v.Benchmark)
		if err != nil {
			return 0, err
		}
		benchmarkBuilder.Append(benchmarkBytes)

		askBytes, err := DecimalToBytes(v.Ask)
		if err != nil {
			return 0, err
		}
		askBuilder.Append(askBytes)

		observedAtBuilder.AppendNull()
		return StreamValueTypeQuote, nil

	case *TimestampedStreamValue:
		valueTypeBuilder.Append(StreamValueTypeTimestampd)
		observedAtBuilder.Append(v.ObservedAtNanoseconds)

		// Handle the inner stream value (usually Decimal)
		if inner, ok := v.StreamValue.(*Decimal); ok {
			b, err := DecimalToBytes(inner.Decimal())
			if err != nil {
				return 0, err
			}
			decimalBuilder.Append(b)
		} else {
			decimalBuilder.AppendNull()
		}
		bidBuilder.AppendNull()
		benchmarkBuilder.AppendNull()
		askBuilder.AppendNull()
		return StreamValueTypeTimestampd, nil

	default:
		// Unknown type - append nulls
		valueTypeBuilder.AppendNull()
		decimalBuilder.AppendNull()
		bidBuilder.AppendNull()
		benchmarkBuilder.AppendNull()
		askBuilder.AppendNull()
		observedAtBuilder.AppendNull()
		return 0, nil
	}
}

// ArrowToStreamValue extracts a StreamValue from Arrow arrays at the given index.
func ArrowToStreamValue(
	idx int,
	valueTypeArr *array.Uint8,
	decimalArr *array.Binary,
	bidArr *array.Binary,
	benchmarkArr *array.Binary,
	askArr *array.Binary,
	observedAtArr *array.Uint64,
) (StreamValue, error) {
	if valueTypeArr.IsNull(idx) {
		return nil, nil
	}

	valueType := valueTypeArr.Value(idx)

	switch valueType {
	case StreamValueTypeDecimal:
		if decimalArr.IsNull(idx) {
			return nil, nil
		}
		d, err := BytesToDecimal(decimalArr.Value(idx))
		if err != nil {
			return nil, err
		}
		return ToDecimal(d), nil

	case StreamValueTypeQuote:
		if bidArr.IsNull(idx) || benchmarkArr.IsNull(idx) || askArr.IsNull(idx) {
			return nil, nil
		}
		bid, err := BytesToDecimal(bidArr.Value(idx))
		if err != nil {
			return nil, err
		}
		benchmark, err := BytesToDecimal(benchmarkArr.Value(idx))
		if err != nil {
			return nil, err
		}
		ask, err := BytesToDecimal(askArr.Value(idx))
		if err != nil {
			return nil, err
		}
		return &Quote{Bid: bid, Benchmark: benchmark, Ask: ask}, nil

	case StreamValueTypeTimestampd:
		observedAt := uint64(0)
		if !observedAtArr.IsNull(idx) {
			observedAt = observedAtArr.Value(idx)
		}

		var innerValue StreamValue
		if !decimalArr.IsNull(idx) {
			d, err := BytesToDecimal(decimalArr.Value(idx))
			if err != nil {
				return nil, err
			}
			innerValue = ToDecimal(d)
		}

		return &TimestampedStreamValue{
			ObservedAtNanoseconds: observedAt,
			StreamValue:           innerValue,
		}, nil

	default:
		return nil, nil
	}
}

// StreamValuesToArrowRecord converts a map of StreamValues to an Arrow Record.
// This is useful for batch operations on cached stream values.
func StreamValuesToArrowRecord(
	values map[llotypes.StreamID]StreamValue,
	pool *ArrowBuilderPool,
) (arrow.Record, error) {
	builder := pool.GetCacheBuilder()

	streamIDBuilder := builder.Field(CacheColStreamID).(*array.Uint32Builder)
	valueTypeBuilder := builder.Field(CacheColValueType).(*array.Uint8Builder)
	decimalBuilder := builder.Field(CacheColDecimalValue).(*array.BinaryBuilder)
	bidBuilder := builder.Field(CacheColQuoteBid).(*array.BinaryBuilder)
	benchmarkBuilder := builder.Field(CacheColQuoteBenchmark).(*array.BinaryBuilder)
	askBuilder := builder.Field(CacheColQuoteAsk).(*array.BinaryBuilder)
	observedAtBuilder := builder.Field(CacheColObservedAtNs).(*array.Uint64Builder)
	expiresAtBuilder := builder.Field(CacheColExpiresAtNs).(*array.Int64Builder)

	// Pre-allocate capacity
	streamIDBuilder.Reserve(len(values))

	for streamID, sv := range values {
		streamIDBuilder.Append(streamID)
		_, err := StreamValueToArrow(sv, valueTypeBuilder, decimalBuilder,
			bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
		if err != nil {
			pool.PutCacheBuilder(builder)
			return nil, err
		}
		expiresAtBuilder.Append(0) // Caller should set expiration
	}

	record := builder.NewRecord()
	pool.PutCacheBuilder(builder)
	return record, nil
}

// ArrowRecordToStreamValues converts an Arrow Record back to a map of StreamValues.
func ArrowRecordToStreamValues(record arrow.Record) (map[llotypes.StreamID]StreamValue, error) {
	if record == nil || record.NumRows() == 0 {
		return nil, nil
	}

	streamIDArr := record.Column(CacheColStreamID).(*array.Uint32)
	valueTypeArr := record.Column(CacheColValueType).(*array.Uint8)
	decimalArr := record.Column(CacheColDecimalValue).(*array.Binary)
	bidArr := record.Column(CacheColQuoteBid).(*array.Binary)
	benchmarkArr := record.Column(CacheColQuoteBenchmark).(*array.Binary)
	askArr := record.Column(CacheColQuoteAsk).(*array.Binary)
	observedAtArr := record.Column(CacheColObservedAtNs).(*array.Uint64)

	result := make(map[llotypes.StreamID]StreamValue, record.NumRows())

	for i := 0; i < int(record.NumRows()); i++ {
		streamID := streamIDArr.Value(i)
		sv, err := ArrowToStreamValue(i, valueTypeArr, decimalArr, bidArr,
			benchmarkArr, askArr, observedAtArr)
		if err != nil {
			return nil, err
		}
		if sv != nil {
			result[streamID] = sv
		}
	}

	return result, nil
}

// ExtractDecimalColumn extracts all decimal values from an Arrow record column.
// Useful for aggregation operations that need to work with decimal arrays.
func ExtractDecimalColumn(arr *array.Binary) ([]decimal.Decimal, error) {
	result := make([]decimal.Decimal, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		if arr.IsNull(i) {
			continue
		}
		d, err := BytesToDecimal(arr.Value(i))
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}
