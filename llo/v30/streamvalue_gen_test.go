package llo

import (
	"bytes"
	"reflect"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/shopspring/decimal"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
)

// StreamValue gopter generators and comparison helpers used by the v3.0 codec
// property/fuzz tests. Mirrors the equivalent helpers in the root llo package's
// json_report_codec_test.go (test helpers cannot be shared across packages).

func equalStreamValues(sv, sv2 llocommon.StreamValue) bool {
	if sv.Type() != sv2.Type() {
		return false
	}
	m1, err := sv.MarshalBinary()
	if err != nil {
		// should be impossible
		panic(err)
	}
	m2, err := sv2.MarshalBinary()
	if err != nil {
		// should be impossible
		panic(err)
	}
	return bytes.Equal(m1, m2)
}

func genDecimalValue() gopter.Gen {
	return func(p *gopter.GenParameters) *gopter.GenResult {
		var sv llocommon.StreamValue = llocommon.ToDecimal(decimal.NewFromFloat(p.Rng.Float64()))
		return gopter.NewGenResult(sv, gopter.NoShrinker)
	}
}

func genQuote() gopter.Gen {
	return func(p *gopter.GenParameters) *gopter.GenResult {
		var sv llocommon.StreamValue = &llocommon.Quote{
			Bid:       decimal.NewFromFloat(p.Rng.Float64()),
			Benchmark: decimal.NewFromFloat(p.Rng.Float64()),
			Ask:       decimal.NewFromFloat(p.Rng.Float64()),
		}
		return gopter.NewGenResult(sv, gopter.NoShrinker)
	}
}

func genTimestampedStreamValue() gopter.Gen {
	return gopter.CombineGens(
		gen.UInt64(),
		genStreamValue(false), // must disallow nesting here to avoid infinite loops
	).Map(func(values []any) any {
		var sv llocommon.StreamValue = &llocommon.TimestampedStreamValue{
			ObservedAtNanoseconds: values[0].(uint64),
			StreamValue:           values[1].(llocommon.StreamValue),
		}
		return gopter.NewGenResult(sv, gopter.NoShrinker)
	})
}

func genStreamValue(allowNesting bool) gopter.Gen {
	return func(p *gopter.GenParameters) *gopter.GenResult {
		if allowNesting {
			switch p.Rng.Intn(4) {
			case 0:
				return genDecimalValue()(p)
			case 1:
				return genQuote()(p)
			case 2:
				return genTimestampedStreamValue()(p)
			case 3:
				return gopter.NewGenResult((llocommon.StreamValue)(nil), gopter.NoShrinker)
			}
		} else {
			switch p.Rng.Intn(3) {
			case 0:
				return genDecimalValue()(p)
			case 1:
				return genQuote()(p)
			case 2:
				return gopter.NewGenResult((llocommon.StreamValue)(nil), gopter.NoShrinker)
			}
		}
		return nil
	}
}

var streamValueSliceType = reflect.TypeOf((*llocommon.StreamValue)(nil)).Elem()

func genStreamValues(allowNesting bool) gopter.Gen {
	return gen.SliceOf(genStreamValue(allowNesting), streamValueSliceType)
}
