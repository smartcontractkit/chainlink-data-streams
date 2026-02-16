// Package llo: outcome_golden_cases.go defines golden outcome cases for outcome serialization tests.
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

// fullChannelDefinitions is shared between the "full" and "from_full" golden cases.
var fullChannelDefinitions = map[llotypes.ChannelID]llotypes.ChannelDefinition{
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
}

// GoldenOutcomeCases returns the canonical set of outcome golden cases.
// "initial" and "from_full" are outcomes the plugin produces (Plugin.Outcome); "full" is a fixture used as previous outcome to produce "from_full".
func GoldenOutcomeCases() []GoldenOutcomeCase {
	return []GoldenOutcomeCase{
		{
			Name: "initial",
			Outcome: Outcome{
				LifeCycleStage:                  LifeCycleStageProduction,
				ObservationTimestampNanoseconds: 0,
				ChannelDefinitions:              nil,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			},
			OutputFile: "testdata/outcome_serialization/initial.bin",
		},
		{
			Name: "full",
			Outcome: Outcome{
				LifeCycleStage:                  "production",
				ObservationTimestampNanoseconds: 9876543210,
				ChannelDefinitions:              fullChannelDefinitions,
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
		{
			Name: "from_full",
			Outcome: Outcome{
				LifeCycleStage:                  "production",
				ObservationTimestampNanoseconds: 9876543210 + uint64(1e9), // median of 4 obs with this timestamp
				ChannelDefinitions:             fullChannelDefinitions,
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: 5000000000,  // tombstone channel, kept from previous
					2: 9876543210,  // reportable channel, set to previous ObservationTimestampNanoseconds
				},
				StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
					3: {}, // no observations for stream 3, aggregation failed → empty map
				},
			},
			OutputFile: "testdata/outcome_serialization/from_full.bin",
		},
	}
}
