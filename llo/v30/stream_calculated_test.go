package llo

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llocommon "github.com/smartcontractkit/chainlink-data-streams/llo/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessStreamCalculated(t *testing.T) {
	tests := []struct {
		name           string
		outcome        Outcome
		expectedValues map[llotypes.StreamID]llocommon.StreamValue
	}{
		{
			name: "simple addition",
			outcome: Outcome{
				ChannelDefinitions: llotypes.ChannelDefinitions{
					1: {
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
							{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(`{"abi":[{"type":"int256","expression":"Sum(s1, s2)","expressionStreamID":3}]}`),
					},
				},
				StreamAggregates: llocommon.StreamAggregates{
					1: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(5))},
					2: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(3))},
				},
			},
			expectedValues: map[llotypes.StreamID]llocommon.StreamValue{
				3: llocommon.ToDecimal(decimal.NewFromInt(8)),
			},
		},
		{
			name: "complex expression",
			outcome: Outcome{
				ChannelDefinitions: llotypes.ChannelDefinitions{
					1: {
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
							{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
							{StreamID: 3, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(`{"abi":[{"type":"int256","expression":"Mul(Sum(s1, s2), s3)","expressionStreamID":4}]}`),
					},
				},
				StreamAggregates: llocommon.StreamAggregates{
					1: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(2))},
					2: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(3))},
					3: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(4))},
				},
			},
			expectedValues: map[llotypes.StreamID]llocommon.StreamValue{
				4: llocommon.ToDecimal(decimal.NewFromInt(20)),
			},
		},
		{
			name: "quote stream values",
			outcome: Outcome{
				ChannelDefinitions: llotypes.ChannelDefinitions{
					1: {
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
							{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(`{"abi":[{"type":"int256","expression":"Sum(s1_benchmark, s2_benchmark)","expressionStreamID":3}]}`),
					},
				},
				StreamAggregates: llocommon.StreamAggregates{
					1: {llotypes.AggregatorMedian: &llocommon.Quote{
						Bid:       decimal.NewFromInt(1),
						Benchmark: decimal.NewFromInt(2),
						Ask:       decimal.NewFromInt(3),
					}},
					2: {llotypes.AggregatorMedian: &llocommon.Quote{
						Bid:       decimal.NewFromInt(4),
						Benchmark: decimal.NewFromInt(5),
						Ask:       decimal.NewFromInt(6),
					}},
				},
			},
			expectedValues: map[llotypes.StreamID]llocommon.StreamValue{
				3: llocommon.ToDecimal(decimal.NewFromInt(7)),
			},
		},
		{
			name: "invalid expression",
			outcome: Outcome{
				ChannelDefinitions: llotypes.ChannelDefinitions{
					1: {
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(`{"abi":[{"type":"int256","expression":"Sum(s1)","expressionStreamID":2}]}`),
					},
				},
			},
		},
		{
			name: "empty expression",
			outcome: Outcome{
				ChannelDefinitions: llotypes.ChannelDefinitions{
					1: {
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(`{"abi":[{"type":"int256","expression":"","expressionStreamID":2}]}`),
					},
				},
			},
		},
		{
			name: "zero expression stream ID",
			outcome: Outcome{
				ChannelDefinitions: llotypes.ChannelDefinitions{
					1: {
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(`{"abi":[{"type":"int256","expression":"s1","expressionStreamID":0}]}`),
					},
				},
			},
		},
		{
			name: "invalid JSON options",
			outcome: Outcome{
				ChannelDefinitions: llotypes.ChannelDefinitions{
					1: {
						ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
						Streams: []llotypes.Stream{
							{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
						},
						Opts: []byte(`invalid json`),
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lggr, err := logger.New()
			require.NoError(t, err)
			p := &Plugin{Logger: lggr, OptsCache: llocommon.NewOptsCache()}
			for cid, cd := range tt.outcome.ChannelDefinitions {
				p.OptsCache.Set(cid, cd.Opts)
			}
			p.ProcessCalculatedStreams(&tt.outcome)

			for streamID, expectedValue := range tt.expectedValues {
				actualValue := tt.outcome.StreamAggregates[streamID][llotypes.AggregatorCalculated]
				assert.Equal(t, expectedValue, actualValue)
			}

			if len(tt.expectedValues) > 0 {
				assert.Equal(t, len(tt.outcome.StreamAggregates), len(tt.outcome.ChannelDefinitions[1].Streams))
			}
		})
	}
}

func BenchmarkProcessCalculatedStreams(b *testing.B) {
	aggr := llocommon.StreamAggregates{
		1: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(2))},
		2: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(3))},
		3: {llotypes.AggregatorMedian: llocommon.ToDecimal(decimal.NewFromInt(4))},
	}
	outcome := Outcome{
		ObservationTimestampNanoseconds: 1750169759775700000,
		ChannelDefinitions: llotypes.ChannelDefinitions{
			1: {
				ReportFormat: llotypes.ReportFormatEVMABIEncodeUnpackedExpr,
				Streams: []llotypes.Stream{
					{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
					{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
					{StreamID: 3, Aggregator: llotypes.AggregatorMedian},
				},
				Opts: []byte(`{"abi":[{"type":"int256","expression":"Mul(Sum(s1, s2), s3)","expressionStreamID":4}]}`),
			},
		},
		StreamAggregates: aggr,
	}

	p := &Plugin{Logger: logger.Nop(), OptsCache: llocommon.NewOptsCache()}
	for cid, cd := range outcome.ChannelDefinitions {
		p.OptsCache.Set(cid, cd.Opts)
	}

	for i := 0; i < b.N; i++ {
		p.ProcessCalculatedStreams(&outcome)
		if _, ok := outcome.StreamAggregates[4]; !ok {
			b.Fatal("stream aggregate is not set")
		}
		outcome.StreamAggregates = aggr
	}
}

func TestProcessCalculatedStreamsDryRun(t *testing.T) {
	tests := []struct {
		name        string
		expression  string
		expectError bool
	}{
		{
			name:        "simple addition",
			expression:  "Sum(s1, s2)",
			expectError: false,
		},
		{
			name:        "complex expression with functions",
			expression:  "Mul(Sum(s1, s2), s3)",
			expectError: false,
		},
		{
			name:        "expression with stream properties",
			expression:  "Div(Sum(s1_bid, s2_ask), s3_timestamp)",
			expectError: false,
		},
		{
			name:        "single stream",
			expression:  "Abs(s1_timestamp)",
			expectError: false,
		},
		{
			name:        "empty expression",
			expression:  "",
			expectError: true,
		},
		{
			name:        "invalid expression",
			expression:  "invalid + syntax",
			expectError: true,
		},
		{
			name:        "expression with non-existent function",
			expression:  "NonExistentFunc(s1, s2)",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lggr, err := logger.New()
			require.NoError(t, err)
			p := &Plugin{Logger: lggr}

			err = p.ProcessCalculatedStreamsDryRun(tt.expression)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
