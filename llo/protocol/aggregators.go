package protocol

import (
	"fmt"
	"slices"
	"sort"

	"github.com/shopspring/decimal"
	"golang.org/x/exp/maps"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// AggregatorFunc aggregates contributions, requiring at least
// minContributions usable values of the selected type.
type AggregatorFunc func(contributions []StreamValue, minContributions int) (StreamValue, error)

func GetAggregatorFunc(a llotypes.Aggregator) AggregatorFunc {
	switch a {
	case llotypes.AggregatorMedian:
		return MedianAggregator
	case llotypes.AggregatorMode:
		return ModeAggregator
	case llotypes.AggregatorQuote:
		return QuoteAggregator
	default:
		return nil
	}
}

func MedianAggregator(values []StreamValue, minContributions int) (StreamValue, error) {
	typ, typValues := mostCommonType(values)

	switch typ {
	case LLOStreamValue_TimestampedStreamValue:
		svalues := make([]StreamValue, len(typValues))
		timestamps := make([]uint64, len(typValues))
		for i, value := range typValues {
			v, is := value.(*TimestampedStreamValue)
			if !is {
				return nil, fmt.Errorf("invariant violation, expected TimestampedStreamValue, got %T", value)
			}
			if v.StreamValue.Type() != LLOStreamValue_Decimal {
				// Only decimal is allowed for now, for simplicity.
				//
				// NOTE: This could get into an infinite loop if a malicious node sends
				// a TimestampedStreamValue with nested TimestampedStreamValues inside
				// it, so you definitely want to discard those.
				continue
			}
			svalues[i] = v.StreamValue
			timestamps[i] = v.ObservedAtNanoseconds
		}

		medianValue, err := MedianAggregator(svalues, minContributions)
		if err != nil {
			return nil, err
		}
		sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })
		return &TimestampedStreamValue{
			ObservedAtNanoseconds: (timestamps[len(timestamps)/2]),
			StreamValue:           medianValue,
		}, nil
	case LLOStreamValue_Decimal, LLOStreamValue_Quote:
		observations := make([]decimal.Decimal, 0, len(values))
		for _, value := range values {
			switch v := value.(type) {
			case *Decimal:
				observations = append(observations, v.Decimal())
			case *Quote:
				observations = append(observations, v.Benchmark)
			default:
				// Unexpected type, skip
				continue
			}
		}
		if len(observations) < minContributions {
			// Below the contribution floor the median is not guaranteed to sit
			// inside the honest value range, so refuse to produce one.
			return nil, fmt.Errorf("not enough contributions to calculate median: got %d, need %d", len(observations), minContributions)
		}
		sort.Slice(observations, func(i, j int) bool { return observations[i].Cmp(observations[j]) < 0 })
		// We use a "rank-k" median here, instead one could average in case of
		// an even number of observations.
		// In the case of an even number, the higher value is chosen.
		// e.g. [1, 2, 3, 4] -> 3
		return ToDecimal(observations[len(observations)/2]), nil
	default:
		return nil, fmt.Errorf("cannot take median of unsupported StreamValue type %v", typ)
	}
}

// ModeAggregator works on arbitrary StreamValue types
// It picks the most common value
// There must be at least minContributions contributions in agreement in order
// to produce a value
// nil contributions are ignored
func ModeAggregator(values []StreamValue, minContributions int) (StreamValue, error) {
	largestBucketType, largestBucket := mostCommonType(values)

	// find the most common value in the bucket
	// use serialized representation for comparison/equality
	counts := make(map[string]int)
	for _, value := range largestBucket {
		b, err := value.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal value: %w", err)
		}
		counts[string(b)]++
	}
	var modeSerialized []byte
	var modeCount int
	// tie-break selecting lowest "key"
	keys := maps.Keys(counts)
	slices.Sort(keys)
	for _, value := range keys {
		count := counts[value]
		if count > modeCount {
			modeSerialized = []byte(value)
			modeCount = count
		}
	}

	if modeCount < minContributions {
		return nil, fmt.Errorf("not enough contributions in agreement to calculate mode: most common value had %d, need %d", modeCount, minContributions)
	}
	if len(modeSerialized) == 0 {
		return nil, nil
	}
	val, err := UnmarshalProtoStreamValue(&LLOStreamValue{Type: largestBucketType, Value: modeSerialized})
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal value: %w", err)
	}
	return val, nil
}

func mostCommonType(values []StreamValue) (LLOStreamValue_Type, []StreamValue) {
	// Initialize variables to track the most common type
	buckets := make(map[LLOStreamValue_Type][]StreamValue)
	var mostCommonType LLOStreamValue_Type
	var largestBucket []StreamValue

	// Remove nils, bucket by type, and find the most common type
	for _, value := range values {
		if value != nil {
			bucketType := value.Type()
			buckets[bucketType] = append(buckets[bucketType], value)
			if len(buckets[bucketType]) > len(largestBucket) || (len(buckets[bucketType]) == len(largestBucket) && bucketType < mostCommonType) {
				largestBucket = buckets[bucketType]
				mostCommonType = bucketType
			}
		}
	}

	return mostCommonType, largestBucket
}

func QuoteAggregator(values []StreamValue, minContributions int) (StreamValue, error) {
	var observations []*Quote
	for _, value := range values {
		if v, ok := value.(*Quote); !ok {
			// Unexpected type, skip
			continue
		} else if v.IsValid() {
			observations = append(observations, v)
		}
		// Exclude Quotes that violate bid<=mid<=ask
	}
	if len(observations) < minContributions {
		// Below the contribution floor the rank medians below are not
		// guaranteed to sit inside the honest value range.
		return nil, fmt.Errorf("not enough valid contributions to aggregate quote: got %d, need %d", len(observations), minContributions)
	}
	// Calculate "rank-k" median for benchmark, bid and ask separately.
	// This is guaranteed not to return values that violate bid<=mid<=ask due
	// to the filter of observations above.
	q := Quote{}
	sort.Slice(observations, func(i, j int) bool { return observations[i].Benchmark.Cmp(observations[j].Benchmark) < 0 })
	q.Benchmark = observations[len(observations)/2].Benchmark
	sort.Slice(observations, func(i, j int) bool { return observations[i].Bid.Cmp(observations[j].Bid) < 0 })
	q.Bid = observations[len(observations)/2].Bid
	sort.Slice(observations, func(i, j int) bool { return observations[i].Ask.Cmp(observations[j].Ask) < 0 })
	q.Ask = observations[len(observations)/2].Ask
	return &q, nil
}
