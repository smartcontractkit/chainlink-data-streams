// Package outcome_serialization - outcome_golden_cases.go defines golden outcome cases for serialization tests.
package llo

import (
	"github.com/shopspring/decimal"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

// GoldenOutcomeCase defines a single outcome serialization golden test case.
// Used by llo/tools/generate_golden to emit golden files and by outcome_codec_v1_test for comparison.
type GoldenOutcomeCase struct {
	Name       string
	Outcome    Outcome
	OutputFile string
}

// GoldenOutcomeCases returns the canonical set of outcome golden cases.
func GoldenOutcomeCases() []GoldenOutcomeCase {
	return []GoldenOutcomeCase{
		{
			Name: "empty",
			Outcome: Outcome{
				LifeCycleStage:                  "",
				ObservationTimestampNanoseconds: 0,
				ChannelDefinitions:              nil,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			},
			OutputFile: "testdata/outcome_serialization/empty.bin",
		},
		{
			Name: "full",
			Outcome: Outcome{
				LifeCycleStage:                  "production",
				ObservationTimestampNanoseconds: 9876543210,
				ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
					1: {
						ReportFormat: llotypes.ReportFormatJSON,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
							{StreamID: 2, Aggregator: llotypes.AggregatorQuote},
						},
						Opts:      []byte(`{"foo":"bar"}`),
						Tombstone: true,
						Source:    0,
					},
					2: {
						ReportFormat: llotypes.ReportFormatJSON,
						Streams: []llotypes.Stream{
							{StreamID: 3, Aggregator: llotypes.AggregatorMedian},
						},
						Opts:      []byte(`{"baz":"qux"}`),
						Source:    1001,
						Tombstone: false,
					},
				},
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: 5000000000,
					2: 6000000000,
				},
				StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
					1: {
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(12345)),
					},
					2: {
						llotypes.AggregatorQuote: &Quote{
							Bid:       decimal.NewFromInt(1010),
							Benchmark: decimal.NewFromInt(1011),
							Ask:       decimal.NewFromInt(1012),
						},
					},
					3: {
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(123.456)),
					},
				},
			},
			OutputFile: "testdata/outcome_serialization/full.bin",
		},
	}
}
