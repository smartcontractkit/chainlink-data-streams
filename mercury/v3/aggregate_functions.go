package v3

import (
	"fmt"
	"sort"

	"github.com/shopspring/decimal"
)

func GetConsensusPrices(paos []PAO, f int) (Prices, error) {
	var validPrices []Prices
	for _, pao := range paos {
		prices, valid := pao.GetPrices()
		if valid {
			validPrices = append(validPrices, prices)
		}
	}
	// FIXME: This should actually check for < 2*f+1, but we can't do that in
	// this mercury plugin because it doesn't support ValidateObservation
	if len(validPrices) < f+1 {
		return Prices{}, fmt.Errorf("fewer than f+1 observations have a valid price (got: %d/%d)", len(validPrices), len(paos))
	}

	bidSpreads := make([]decimal.Decimal, len(validPrices))
	askSpreads := make([]decimal.Decimal, len(validPrices))
	for i, p := range validPrices {
		bid := decimal.NewFromBigInt(p.Bid, 0)
		ask := decimal.NewFromBigInt(p.Ask, 0)
		benchmark := decimal.NewFromBigInt(p.Benchmark, 0)

		bidSpreads[i] = bid.Div(benchmark)
		askSpreads[i] = ask.Div(benchmark)
	}

	prices := Prices{}

	sort.Slice(validPrices, func(i, j int) bool {
		return validPrices[i].Benchmark.Cmp(validPrices[j].Benchmark) < 0
	})
	prices.Benchmark = validPrices[len(validPrices)/2].Benchmark
	benchmarkDecimal := decimal.NewFromBigInt(prices.Benchmark, 0)

	sort.Slice(bidSpreads, func(i, j int) bool {
		return bidSpreads[i].Cmp(bidSpreads[j]) < 0
	})
	// We started with at least 2f+1 observations. There are at most f
	// dishonest participants. Suppose with threw out m observations for
	// disordered prices. Then we are left with 2f+1-m observations, f-m of
	// which could still have come from dishonest participants. But
	// 2f+1-m=2(f-m)+(m+1), so the median must come from an honest participant.
	medianBidSpread := bidSpreads[len(bidSpreads)/2]

	prices.Bid = benchmarkDecimal.Mul(medianBidSpread).BigInt()
	if prices.Bid.Cmp(prices.Benchmark) > 0 {
		// Cannot happen unless > f nodes are inverted which is by assumption undefined behavior
		return Prices{}, fmt.Errorf("invariant violation: bid price is greater than benchmark price (bid: %s, benchmark: %s)", prices.Bid.String(), benchmarkDecimal.String())
	}

	sort.Slice(askSpreads, func(i, j int) bool {
		return askSpreads[i].Cmp(askSpreads[j]) < 0
	})
	medianAskSpread := askSpreads[len(askSpreads)/2]
	prices.Ask = benchmarkDecimal.Mul(medianAskSpread).BigInt()
	if prices.Ask.Cmp(prices.Benchmark) < 0 {
		// Cannot happen unless > f nodes are inverted which is by assumption undefined behavior
		return Prices{}, fmt.Errorf("invariant violation: ask price is less than benchmark price (ask: %s, benchmark: %s)", prices.Ask.String(), benchmarkDecimal.String())
	}

	return prices, nil
}
