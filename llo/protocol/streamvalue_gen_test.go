package protocol

import (
	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/shopspring/decimal"
)

func genDecimalValue() gopter.Gen {
	return func(p *gopter.GenParameters) *gopter.GenResult {
		var sv StreamValue = ToDecimal(decimal.NewFromFloat(p.Rng.Float64()))
		return gopter.NewGenResult(sv, gopter.NoShrinker)
	}
}

func genQuote() gopter.Gen {
	return func(p *gopter.GenParameters) *gopter.GenResult {
		var sv StreamValue = &Quote{
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
		var sv StreamValue = &TimestampedStreamValue{
			ObservedAtNanoseconds: values[0].(uint64),
			StreamValue:           values[1].(StreamValue),
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
				return gopter.NewGenResult((StreamValue)(nil), gopter.NoShrinker)
			}
		} else {
			switch p.Rng.Intn(3) {
			case 0:
				return genDecimalValue()(p)
			case 1:
				return genQuote()(p)
			case 2:
				return gopter.NewGenResult((StreamValue)(nil), gopter.NoShrinker)
			}
		}
		return nil
	}
}
