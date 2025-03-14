package llo

import (
	"fmt"
	"slices"
	"sort"

	"github.com/shopspring/decimal"
	"golang.org/x/exp/maps"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

type AggregatorFunc func(values []StreamValue, f int) (StreamValue, error)

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

func MedianAggregator(values []StreamValue, f int) (StreamValue, error) {
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
	if len(observations) <= f {
		// In the worst case, we have 2f+1 observations, of which up to f
		// are allowed to be invalid/missing. If we have less than f+1
		// usable observations, we cannot securely generate a median at
		// all.
		return nil, fmt.Errorf("not enough observations to calculate median, expected at least f+1, got %d", len(observations))
	}
	sort.Slice(observations, func(i, j int) bool { return observations[i].Cmp(observations[j]) < 0 })
	// We use a "rank-k" median here, instead one could average in case of
	// an even number of observations.
	// In the case of an even number, the higher value is chosen.
	// e.g. [1, 2, 3, 4] -> 3
	return ToDecimal(observations[len(observations)/2]), nil
}

// ModeAggregator works on arbitrary StreamValue types
// It picks the most common value
// There must be at least f+1 observations in agreement in order to produce a value
// nil observations are ignored
func ModeAggregator(values []StreamValue, f int) (StreamValue, error) {
	// remove nils
	var observations []StreamValue
	for _, value := range values {
		if value != nil {
			observations = append(observations, value)
		}
	}

	// bucket by type
	buckets := make(map[LLOStreamValue_Type][]StreamValue)
	for _, value := range observations {
		buckets[value.Type()] = append(buckets[value.Type()], value)
	}
	// find the largest bucket
	// tie-break on type alphabetical order
	var largestBucket []StreamValue
	var largestBucketType LLOStreamValue_Type
	for bucketType, bucket := range buckets {
		if len(bucket) > len(largestBucket) || (len(bucket) == len(largestBucket) && bucketType < largestBucketType) {
			largestBucket = bucket
			largestBucketType = bucketType
		}
	}

	// find the most common value in the bucket
	// use serialized representation for comparison/equality
	counts := make(map[string]int)
	for _, value := range largestBucket {
		b, err := value.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal value: %v", err)
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

	if modeCount < f+1 {
		return nil, fmt.Errorf("not enough observations in agreement to calculate mode, expected at least f+1, most common value had %d", modeCount)
	}
	if len(modeSerialized) == 0 {
		return nil, nil
	}
	val, err := UnmarshalProtoStreamValue(&LLOStreamValue{Type: largestBucketType, Value: modeSerialized})
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal value: %v", err)
	}
	return val, nil
}

func QuoteAggregator(values []StreamValue, f int) (StreamValue, error) {
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
	if len(observations) <= f {
		// In the worst case, we have 2f+1 observations, of which up to f
		// are allowed to be invalid/missing. If we have less than f+1
		// usable observations, we cannot securely generate a median at
		// all.
		return nil, fmt.Errorf("not enough valid observations to aggregate quote, expected at least f+1, got %d", len(observations))
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
