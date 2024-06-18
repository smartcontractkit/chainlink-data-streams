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
	medianBidSpread := bidSpreads[len(bidSpreads)/2]
	prices.Bid = benchmarkDecimal.Mul(medianBidSpread).BigInt()

	sort.Slice(askSpreads, func(i, j int) bool {
		return askSpreads[i].Cmp(askSpreads[j]) < 0
	})
	medianAskSpread := askSpreads[len(askSpreads)/2]
	prices.Ask = benchmarkDecimal.Mul(medianAskSpread).BigInt()

	return prices, nil
}
