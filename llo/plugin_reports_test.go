package llo

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Reports(t *testing.T) {
	p := &Plugin{
		Config:       Config{true},
		OutcomeCodec: protoOutcomeCodec{},
		Logger:       logger.Test(t),
		Codecs: map[llotypes.ReportFormat]ReportCodec{
			llotypes.ReportFormatJSON: JSONReportCodec{},
		},
	}

	t.Run("ignores seqnr=0", func(t *testing.T) {
		rwi, err := p.Reports(0, ocr3types.Outcome{})
		assert.NoError(t, err)
		assert.Nil(t, rwi)
	})

	t.Run("does not return reports for initial round", func(t *testing.T) {
		rwi, err := p.Reports(1, ocr3types.Outcome{})
		assert.NoError(t, err)
		assert.Nil(t, rwi)
	})

	t.Run("returns error if unmarshalling outcome fails", func(t *testing.T) {
		rwi, err := p.Reports(2, ocr3types.Outcome([]byte("invalid")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode outcome: expected protobuf")
		assert.Nil(t, rwi)
	})

	t.Run("emits 'retirement report' if lifecycle state is retired", func(t *testing.T) {
		t.Run("with null ValidAfterSeconds", func(t *testing.T) {
			outcome := Outcome{
				LifeCycleStage: LifeCycleStageRetired,
			}
			encoded, err := p.OutcomeCodec.Encode(outcome)
			require.NoError(t, err)
			rwis, err := p.Reports(2, encoded)
			require.NoError(t, err)
			require.Len(t, rwis, 1)
			assert.Equal(t, llo.ReportInfo{LifeCycleStage: LifeCycleStageRetired, ReportFormat: llotypes.ReportFormatJSON}, rwis[0].Info)
			assert.Equal(t, "{\"ValidAfterSeconds\":null}", string(rwis[0].Report))
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
		outcome := Outcome{
			ObservationsTimestampNanoseconds: 0,
			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
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
		rwis, err := p.Reports(2, encoded)
		require.NoError(t, err)
		require.Len(t, rwis, 0)
	})

	t.Run("does not produce report if an aggregate is missing", func(t *testing.T) {
		outcome := Outcome{
			ObservationsTimestampNanoseconds: int64(200 * time.Second),
			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				2: 100,
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
		rwis, err := p.Reports(2, ocr3types.Outcome(encoded))
		require.NoError(t, err)
		require.Len(t, rwis, 0)
	})

	t.Run("skips reports if codec is missing", func(t *testing.T) {
		dfns := map[llotypes.ChannelID]llotypes.ChannelDefinition{
			1: {
				ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorQuote}},
			},
		}

		outcome := Outcome{
			ObservationsTimestampNanoseconds: int64(200 * time.Second),
			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				2: 100,
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
		rwis, err := p.Reports(2, ocr3types.Outcome(encoded))
		require.NoError(t, err)
		require.Len(t, rwis, 0)
	})

	t.Run("generates specimen report for non-production LifeCycleStage", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                   LifeCycleStageStaging,
			ObservationsTimestampNanoseconds: int64(200 * time.Second),
			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				1: 100,
				2: 100,
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
		rwis, err := p.Reports(2, ocr3types.Outcome(encoded))
		require.NoError(t, err)
		require.Len(t, rwis, 2)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":1,"ValidAfterSeconds":100,"ObservationTimestampSeconds":200,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 5.5, Benchmark: 4.4, Ask: 3.3}"}],"Specimen":true}`, string(rwis[0].Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "staging", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].Info)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterSeconds":100,"ObservationTimestampSeconds":200,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":true}`, string(rwis[1].Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "staging", ReportFormat: llotypes.ReportFormatJSON}, rwis[1].Info)
	})

	t.Run("generates non-specimen reports for production", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                   LifeCycleStageProduction,
			ObservationsTimestampNanoseconds: int64(200 * time.Second),
			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				1: 100,
				2: 100,
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
		rwis, err := p.Reports(2, encoded)
		require.NoError(t, err)
		require.Len(t, rwis, 2)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":1,"ValidAfterSeconds":100,"ObservationTimestampSeconds":200,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 5.5, Benchmark: 4.4, Ask: 3.3}"}],"Specimen":false}`, string(rwis[0].Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].Info)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterSeconds":100,"ObservationTimestampSeconds":200,"Values":[{"Type":0,"Value":"1.1"},{"Type":0,"Value":"2.2"},{"Type":1,"Value":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":false}`, string(rwis[1].Report))
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[1].Info)
	})
}
