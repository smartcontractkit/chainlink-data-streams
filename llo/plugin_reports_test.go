package llo

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Reports(t *testing.T) {
	for _, codec := range []OutcomeCodec{protoOutcomeCodecV0{}, protoOutcomeCodecV1{}} {
		t.Run(fmt.Sprintf("OutcomeCodec: %T", codec), func(t *testing.T) {
			testReports(t, codec)
		})
	}
}

func testReports(t *testing.T, outcomeCodec OutcomeCodec) {
	var protocolVersion uint32 = 1
	minReportInterval := 100 * time.Millisecond
	if _, ok := outcomeCodec.(protoOutcomeCodecV0); ok {
		protocolVersion = 0
		minReportInterval = 0
	}
	p := &Plugin{
		Config:       Config{true},
		OutcomeCodec: outcomeCodec,
		Logger:       logger.Test(t),
		ReportCodecs: map[llotypes.ReportFormat]ReportCodec{
			llotypes.ReportFormatJSON: JSONReportCodec{},
		},
		RetirementReportCodec:               StandardRetirementReportCodec{},
		DefaultMinReportIntervalNanoseconds: uint64(minReportInterval), // nolint:gosec // time won't be negative
		ProtocolVersion:                     protocolVersion,
	}

	t.Run("ignores seqnr=0", func(t *testing.T) {
		ctx := tests.Context(t)
		rwi, err := p.Reports(ctx, 0, ocr3types.Outcome{})
		assert.NoError(t, err)
		assert.Nil(t, rwi)
	})

	t.Run("does not return reports for initial round", func(t *testing.T) {
		ctx := tests.Context(t)
		rwi, err := p.Reports(ctx, 1, ocr3types.Outcome{})
		assert.NoError(t, err)
		assert.Nil(t, rwi)
	})

	t.Run("returns error if unmarshalling outcome fails", func(t *testing.T) {
		ctx := tests.Context(t)
		rwi, err := p.Reports(ctx, 2, []byte("invalid"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode outcome: expected protobuf")
		assert.Nil(t, rwi)
	})

	t.Run("emits 'retirement report' if lifecycle state is retired", func(t *testing.T) {
		t.Run("with null ValidAfterNanoseconds", func(t *testing.T) {
			ctx := tests.Context(t)
			outcome := Outcome{
				LifeCycleStage: LifeCycleStageRetired,
			}
			encoded, err := p.OutcomeCodec.Encode(outcome)
			require.NoError(t, err)
			rwis, err := p.Reports(ctx, 2, encoded)
			require.NoError(t, err)
			require.Len(t, rwis, 1)
			assert.Equal(t, llo.ReportInfo{LifeCycleStage: LifeCycleStageRetired, ReportFormat: llotypes.ReportFormatRetirement}, rwis[0].ReportWithInfo.Info)
			assert.Equal(t, fmt.Sprintf(`{"ProtocolVersion":%d,"ValidAfterNanoseconds":null}`, p.ProtocolVersion), string(rwis[0].ReportWithInfo.Report))
		})
		t.Run("with ValidAfterNanoseconds", func(t *testing.T) {
			ctx := tests.Context(t)
			outcome := Outcome{
				LifeCycleStage: LifeCycleStageRetired,
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: uint64(2 * time.Second),
					2: uint64(3 * time.Second),
					3: uint64(100 * time.Millisecond),
				},
			}
			encoded, err := p.OutcomeCodec.Encode(outcome)
			require.NoError(t, err)
			rwis, err := p.Reports(ctx, 2, encoded)
			require.NoError(t, err)
			require.Len(t, rwis, 1)
			assert.Equal(t, llo.ReportInfo{LifeCycleStage: LifeCycleStageRetired, ReportFormat: llotypes.ReportFormatRetirement}, rwis[0].ReportWithInfo.Info)

			subSecond := "100000000"
			if p.ProtocolVersion == 0 {
				// sub-second values are truncated in outcomes for protocol version 0
				subSecond = "0"
			}
			assert.Equal(t, fmt.Sprintf(`{"ProtocolVersion":%d,"ValidAfterNanoseconds":{"1":2000000000,"2":3000000000,"3":%s}}`, p.ProtocolVersion, subSecond), string(rwis[0].ReportWithInfo.Report))
		})
	})

	smallDefinitions := map[llotypes.ChannelID]llotypes.ChannelDefinition{
		1: {
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorQuote}},
		},
		2: {
			ReportFormat: llotypes.ReportFormatJSON,
			Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorQuote}},
		},
	}

	t.Run("does not report if observations are not valid yet", func(t *testing.T) {
		ctx := tests.Context(t)
		outcome := Outcome{
			ObservationTimestampNanoseconds: 0,
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				1: 0,
			},
			ChannelDefinitions: smallDefinitions,
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(1.1)),
				},
				2: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(2.2)),
				},
				3: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(3.3), Benchmark: decimal.NewFromFloat(4.4), Bid: decimal.NewFromFloat(5.5)},
				},
			},
		}
		encoded, err := p.OutcomeCodec.Encode(outcome)
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, 2, encoded)
		require.NoError(t, err)
		require.Len(t, rwis, 0)
	})

	t.Run("does not produce report if an aggregate is missing", func(t *testing.T) {
		ctx := tests.Context(t)
		outcome := Outcome{
			ObservationTimestampNanoseconds: uint64(200 * time.Second),
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				2: uint64(100 * time.Second),
			},
			ChannelDefinitions: smallDefinitions,
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(1.1)),
				},
				2: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(2.2)),
				},
				3: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(3.3), Benchmark: decimal.NewFromFloat(4.4), Bid: decimal.NewFromFloat(5.5)},
				},
			},
		}
		encoded, err := p.OutcomeCodec.Encode(outcome)
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, 2, encoded)
		require.NoError(t, err)
		require.Len(t, rwis, 0)
	})

	t.Run("skips reports if codec is missing", func(t *testing.T) {
		ctx := tests.Context(t)
		dfns := map[llotypes.ChannelID]llotypes.ChannelDefinition{
			1: {
				ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorQuote}},
			},
		}

		outcome := Outcome{
			ObservationTimestampNanoseconds: uint64(200 * time.Second),
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				2: uint64(100 * time.Second),
			},
			ChannelDefinitions: dfns,
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(1.1)),
				},
				2: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(2.2)),
				},
				3: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(3.3), Benchmark: decimal.NewFromFloat(4.4), Bid: decimal.NewFromFloat(5.5)},
				},
			},
		}
		encoded, err := p.OutcomeCodec.Encode(outcome)
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, 2, encoded)
		require.NoError(t, err)
		require.Len(t, rwis, 0)
	})

	t.Run("generates specimen report for non-production LifeCycleStage", func(t *testing.T) {
		ctx := tests.Context(t)
		outcome := Outcome{
			LifeCycleStage:                  LifeCycleStageStaging,
			ObservationTimestampNanoseconds: uint64(200 * time.Second),
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				1: uint64(100 * time.Second),
				2: uint64(100 * time.Second),
			},
			ChannelDefinitions: smallDefinitions,
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(1.1)),
				},
				2: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(2.2)),
				},
				3: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(3.3), Benchmark: decimal.NewFromFloat(4.4), Bid: decimal.NewFromFloat(5.5)},
				},
				4: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(6.6), Benchmark: decimal.NewFromFloat(7.7), Bid: decimal.NewFromFloat(8.8)},
				},
			},
		}
		encoded, err := p.OutcomeCodec.Encode(outcome)
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, 2, encoded)
		require.NoError(t, err)
		require.Len(t, rwis, 2)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":1,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 5.5, Benchmark: 4.4, Ask: 3.3}"}],"Specimen":true}`, string(rwis[0].ReportWithInfo.Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "staging", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].ReportWithInfo.Info)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":true}`, string(rwis[1].ReportWithInfo.Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "staging", ReportFormat: llotypes.ReportFormatJSON}, rwis[1].ReportWithInfo.Info)
	})

	t.Run("generates non-specimen reports for production", func(t *testing.T) {
		ctx := tests.Context(t)
		outcome := Outcome{
			LifeCycleStage:                  LifeCycleStageProduction,
			ObservationTimestampNanoseconds: uint64(200 * time.Second),
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				1: uint64(100 * time.Second),
				2: uint64(100 * time.Second),
			},
			ChannelDefinitions: smallDefinitions,
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(1.1)),
				},
				2: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(2.2)),
				},
				3: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(3.3), Benchmark: decimal.NewFromFloat(4.4), Bid: decimal.NewFromFloat(5.5)},
				},
				4: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(6.6), Benchmark: decimal.NewFromFloat(7.7), Bid: decimal.NewFromFloat(8.8)},
				},
			},
		}
		encoded, err := p.OutcomeCodec.Encode(outcome)
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, 2, encoded)
		require.NoError(t, err)
		require.Len(t, rwis, 2)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":1,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 5.5, Benchmark: 4.4, Ask: 3.3}"}],"Specimen":false}`, string(rwis[0].ReportWithInfo.Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].ReportWithInfo.Info)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":false}`, string(rwis[1].ReportWithInfo.Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[1].ReportWithInfo.Info)
	})
	t.Run("does not produce reports with overlapping timestamps (where IsReportable returns false)", func(t *testing.T) {
		ctx := tests.Context(t)
		outcome := Outcome{
			LifeCycleStage:                  LifeCycleStageProduction,
			ObservationTimestampNanoseconds: uint64(200 * time.Second),
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				1: uint64(200 * time.Second),
				2: uint64(100 * time.Second),
			},
			ChannelDefinitions: smallDefinitions,
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(1.1)),
				},
				2: {
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromFloat(2.2)),
				},
				3: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(3.3), Benchmark: decimal.NewFromFloat(4.4), Bid: decimal.NewFromFloat(5.5)},
				},
				4: {
					llotypes.AggregatorQuote: &Quote{Ask: decimal.NewFromFloat(6.6), Benchmark: decimal.NewFromFloat(7.7), Bid: decimal.NewFromFloat(8.8)},
				},
			},
		}
		encoded, err := p.OutcomeCodec.Encode(outcome)
		require.NoError(t, err)
		rwis, err := p.Reports(ctx, 2, encoded)
		require.NoError(t, err)

		// Only second channel is reported because first channel is not valid yet
		require.Len(t, rwis, 1)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":false}`, string(rwis[0].ReportWithInfo.Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].ReportWithInfo.Info)
	})
}
