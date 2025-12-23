package llo

import (
	"fmt"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/shopspring/decimal"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// ArrowAggregator performs vectorized aggregation on Arrow records.
type ArrowAggregator struct {
	pool   *ArrowBuilderPool
	logger logger.Logger
}

// NewArrowAggregator creates a new Arrow-based aggregator.
// Logger can be nil if logging is not needed.
func NewArrowAggregator(pool *ArrowBuilderPool, lggr logger.Logger) *ArrowAggregator {
	return &ArrowAggregator{pool: pool, logger: lggr}
}

// streamObservations groups observations by stream ID for aggregation.
type streamObservations struct {
	valueType   uint8
	decimals    []decimal.Decimal
	quotes      []*Quote
	timestamps  []uint64
	innerValues []decimal.Decimal // For TimestampedStreamValue
}

// AggregateObservations performs aggregation on merged observations.
// It groups observations by stream ID and applies the appropriate aggregator.
func (a *ArrowAggregator) AggregateObservations(
	record arrow.Record,
	channelDefs llotypes.ChannelDefinitions,
	f int,
) (StreamAggregates, error) {
	if record == nil || record.NumRows() == 0 {
		return nil, nil
	}

	// Extract columns
	streamIDArr := record.Column(ObsColStreamID).(*array.Uint32)
	valueTypeArr := record.Column(ObsColValueType).(*array.Uint8)
	decimalArr := record.Column(ObsColDecimalValue).(*array.Binary)
	bidArr := record.Column(ObsColQuoteBid).(*array.Binary)
	benchmarkArr := record.Column(ObsColQuoteBenchmark).(*array.Binary)
	askArr := record.Column(ObsColQuoteAsk).(*array.Binary)
	observedAtArr := record.Column(ObsColObservedAtNs).(*array.Uint64)

	// Group observations by stream ID
	grouped := make(map[llotypes.StreamID]*streamObservations)

	for i := 0; i < int(record.NumRows()); i++ {
		streamID := streamIDArr.Value(i)
		if valueTypeArr.IsNull(i) {
			continue
		}

		obs, exists := grouped[streamID]
		if !exists {
			obs = &streamObservations{
				valueType: valueTypeArr.Value(i),
			}
			grouped[streamID] = obs
		}

		valueType := valueTypeArr.Value(i)

		switch valueType {
		case StreamValueTypeDecimal:
			if !decimalArr.IsNull(i) {
				d, err := BytesToDecimal(decimalArr.Value(i))
				if err == nil {
					obs.decimals = append(obs.decimals, d)
				}
			}

		case StreamValueTypeQuote:
			if !bidArr.IsNull(i) && !benchmarkArr.IsNull(i) && !askArr.IsNull(i) {
				bid, err1 := BytesToDecimal(bidArr.Value(i))
				benchmark, err2 := BytesToDecimal(benchmarkArr.Value(i))
				ask, err3 := BytesToDecimal(askArr.Value(i))
				// Skip quotes with parsing errors to avoid injecting zero/corrupt values
				if err1 != nil || err2 != nil || err3 != nil {
					continue
				}
				q := &Quote{Bid: bid, Benchmark: benchmark, Ask: ask}
				if q.IsValid() {
					obs.quotes = append(obs.quotes, q)
				}
			}

		case StreamValueTypeTimestampd:
			if !observedAtArr.IsNull(i) {
				obs.timestamps = append(obs.timestamps, observedAtArr.Value(i))
			}
			if !decimalArr.IsNull(i) {
				d, err := BytesToDecimal(decimalArr.Value(i))
				if err == nil {
					obs.innerValues = append(obs.innerValues, d)
				}
			}
		}
	}

	// Determine required aggregators for each stream from channel definitions
	streamAggregators := make(map[llotypes.StreamID]llotypes.Aggregator)
	for _, cd := range channelDefs {
		for _, stream := range cd.Streams {
			streamAggregators[stream.StreamID] = stream.Aggregator
		}
	}

	// Apply aggregators
	result := make(StreamAggregates)

	for streamID, obs := range grouped {
		aggregator, exists := streamAggregators[streamID]
		if !exists {
			aggregator = llotypes.AggregatorMedian // Default
		}

		var sv StreamValue
		var err error

		switch aggregator {
		case llotypes.AggregatorMedian:
			sv, err = a.medianAggregate(obs, f)
		case llotypes.AggregatorMode:
			sv, err = a.modeAggregate(obs, f)
		case llotypes.AggregatorQuote:
			sv, err = a.quoteAggregate(obs, f)
		default:
			sv, err = a.medianAggregate(obs, f)
		}

		if err != nil {
			if a.logger != nil {
				a.logger.Debugw("Aggregation failed for stream", "streamID", streamID, "aggregator", aggregator, "err", err)
			}
			continue // Skip streams that fail aggregation
		}

		if sv != nil {
			if result[streamID] == nil {
				result[streamID] = make(map[llotypes.Aggregator]StreamValue)
			}
			result[streamID][aggregator] = sv
		}
	}

	return result, nil
}

// medianAggregate computes the median for a stream's observations.
func (a *ArrowAggregator) medianAggregate(obs *streamObservations, f int) (StreamValue, error) {
	switch obs.valueType {
	case StreamValueTypeDecimal:
		if len(obs.decimals) <= f {
			return nil, fmt.Errorf("not enough observations: %d <= %d", len(obs.decimals), f)
		}
		// Sort decimals
		sorted := make([]decimal.Decimal, len(obs.decimals))
		copy(sorted, obs.decimals)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Cmp(sorted[j]) < 0
		})
		return ToDecimal(sorted[len(sorted)/2]), nil

	case StreamValueTypeQuote:
		// For quotes, use benchmark for median calculation
		if len(obs.quotes) <= f {
			return nil, fmt.Errorf("not enough observations: %d <= %d", len(obs.quotes), f)
		}
		benchmarks := make([]decimal.Decimal, len(obs.quotes))
		for i, q := range obs.quotes {
			benchmarks[i] = q.Benchmark
		}
		sort.Slice(benchmarks, func(i, j int) bool {
			return benchmarks[i].Cmp(benchmarks[j]) < 0
		})
		return ToDecimal(benchmarks[len(benchmarks)/2]), nil

	case StreamValueTypeTimestampd:
		if len(obs.innerValues) <= f {
			return nil, fmt.Errorf("not enough observations: %d <= %d", len(obs.innerValues), f)
		}
		// Sort inner values
		sorted := make([]decimal.Decimal, len(obs.innerValues))
		copy(sorted, obs.innerValues)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Cmp(sorted[j]) < 0
		})

		// Sort timestamps
		sortedTs := make([]uint64, len(obs.timestamps))
		copy(sortedTs, obs.timestamps)
		sort.Slice(sortedTs, func(i, j int) bool {
			return sortedTs[i] < sortedTs[j]
		})

		return &TimestampedStreamValue{
			ObservedAtNanoseconds: sortedTs[len(sortedTs)/2],
			StreamValue:           ToDecimal(sorted[len(sorted)/2]),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported value type for median: %d", obs.valueType)
	}
}

// modeAggregate computes the mode (most common value) for a stream's observations.
func (a *ArrowAggregator) modeAggregate(obs *streamObservations, f int) (StreamValue, error) {
	switch obs.valueType {
	case StreamValueTypeDecimal:
		if len(obs.decimals) == 0 {
			return nil, fmt.Errorf("no observations")
		}

		// Count occurrences using string representation
		counts := make(map[string]int)
		valueMap := make(map[string]decimal.Decimal)
		for _, d := range obs.decimals {
			key := d.String()
			counts[key]++
			valueMap[key] = d
		}

		// Find mode
		var modeKey string
		var modeCount int
		for key, count := range counts {
			if count > modeCount || (count == modeCount && key < modeKey) {
				modeKey = key
				modeCount = count
			}
		}

		if modeCount < f+1 {
			return nil, fmt.Errorf("not enough observations in agreement: %d < %d", modeCount, f+1)
		}

		return ToDecimal(valueMap[modeKey]), nil

	case StreamValueTypeQuote:
		if len(obs.quotes) == 0 {
			return nil, fmt.Errorf("no observations")
		}

		// Count occurrences using string representation
		counts := make(map[string]int)
		valueMap := make(map[string]*Quote)
		for _, q := range obs.quotes {
			key := fmt.Sprintf("%s|%s|%s", q.Bid.String(), q.Benchmark.String(), q.Ask.String())
			counts[key]++
			valueMap[key] = q
		}

		// Find mode
		var modeKey string
		var modeCount int
		for key, count := range counts {
			if count > modeCount || (count == modeCount && key < modeKey) {
				modeKey = key
				modeCount = count
			}
		}

		if modeCount < f+1 {
			return nil, fmt.Errorf("not enough observations in agreement: %d < %d", modeCount, f+1)
		}

		return valueMap[modeKey], nil

	default:
		return nil, fmt.Errorf("unsupported value type for mode: %d", obs.valueType)
	}
}

// quoteAggregate computes the median for each component of quote observations.
func (a *ArrowAggregator) quoteAggregate(obs *streamObservations, f int) (StreamValue, error) {
	if obs.valueType != StreamValueTypeQuote {
		return nil, fmt.Errorf("quote aggregator requires quote observations")
	}

	if len(obs.quotes) <= f {
		return nil, fmt.Errorf("not enough observations: %d <= %d", len(obs.quotes), f)
	}

	// Sort and get median for each component
	bids := make([]decimal.Decimal, len(obs.quotes))
	benchmarks := make([]decimal.Decimal, len(obs.quotes))
	asks := make([]decimal.Decimal, len(obs.quotes))

	for i, q := range obs.quotes {
		bids[i] = q.Bid
		benchmarks[i] = q.Benchmark
		asks[i] = q.Ask
	}

	sort.Slice(bids, func(i, j int) bool { return bids[i].Cmp(bids[j]) < 0 })
	sort.Slice(benchmarks, func(i, j int) bool { return benchmarks[i].Cmp(benchmarks[j]) < 0 })
	sort.Slice(asks, func(i, j int) bool { return asks[i].Cmp(asks[j]) < 0 })

	mid := len(obs.quotes) / 2
	return &Quote{
		Bid:       bids[mid],
		Benchmark: benchmarks[mid],
		Ask:       asks[mid],
	}, nil
}

// MedianDecimalBatch computes median for a slice of decimals.
// This is optimized for batch processing.
func MedianDecimalBatch(values []decimal.Decimal, f int) (decimal.Decimal, error) {
	if len(values) <= f {
		return decimal.Decimal{}, fmt.Errorf("not enough values: %d <= %d", len(values), f)
	}

	sorted := make([]decimal.Decimal, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Cmp(sorted[j]) < 0
	})

	return sorted[len(sorted)/2], nil
}

// AggregateResult holds the result of Arrow-based aggregation for a single stream.
type AggregateResult struct {
	StreamID   llotypes.StreamID
	Aggregator llotypes.Aggregator
	Value      StreamValue
}

// BatchAggregateToRecord converts aggregation results to an Arrow record.
func (a *ArrowAggregator) BatchAggregateToRecord(results []AggregateResult) (arrow.Record, error) {
	builder := a.pool.GetAggregatesBuilder()
	defer a.pool.PutAggregatesBuilder(builder)

	streamIDBuilder := builder.Field(AggColStreamID).(*array.Uint32Builder)
	aggregatorBuilder := builder.Field(AggColAggregator).(*array.Uint32Builder)
	valueTypeBuilder := builder.Field(AggColValueType).(*array.Uint8Builder)
	decimalBuilder := builder.Field(AggColDecimalValue).(*array.BinaryBuilder)
	bidBuilder := builder.Field(AggColQuoteBid).(*array.BinaryBuilder)
	benchmarkBuilder := builder.Field(AggColQuoteBenchmark).(*array.BinaryBuilder)
	askBuilder := builder.Field(AggColQuoteAsk).(*array.BinaryBuilder)
	observedAtBuilder := builder.Field(AggColObservedAtNs).(*array.Uint64Builder)

	for _, result := range results {
		streamIDBuilder.Append(result.StreamID)
		aggregatorBuilder.Append(uint32(result.Aggregator))

		_, err := StreamValueToArrow(result.Value, valueTypeBuilder, decimalBuilder,
			bidBuilder, benchmarkBuilder, askBuilder, observedAtBuilder)
		if err != nil {
			return nil, err
		}
	}

	return builder.NewRecord(), nil
}
