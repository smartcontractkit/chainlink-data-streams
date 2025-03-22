package llo

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
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
	ctx := tests.Context(t)
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
		DefaultMinReportIntervalNanoseconds: uint64(minReportInterval), //nolint:gosec // time won't be negative
		ProtocolVersion:                     protocolVersion,
	}

	t.Run("ignores seqnr=0", func(t *testing.T) {
		rwi, err := p.Reports(ctx, 0, ocr3types.Outcome{})
		require.NoError(t, err)
		assert.Nil(t, rwi)
	})

	t.Run("does not return reports for initial round", func(t *testing.T) {
		rwi, err := p.Reports(ctx, 1, ocr3types.Outcome{})
		require.NoError(t, err)
		assert.Nil(t, rwi)
	})

	t.Run("returns error if unmarshalling outcome fails", func(t *testing.T) {
		rwi, err := p.Reports(ctx, 2, []byte("invalid"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode outcome: expected protobuf")
		assert.Nil(t, rwi)
	})

	t.Run("emits 'retirement report' if lifecycle state is retired", func(t *testing.T) {
		t.Run("with null ValidAfterNanoseconds", func(t *testing.T) {
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
			assert.Equal(t, fmt.Sprintf(`{"ProtocolVersion":%d,"ValidAfterNanoseconds":{"1":2000000000,"2":3000000000,"3":%s}}`, p.ProtocolVersion, subSecond), string(rwis[0].ReportWithInfo.Report)) //nolint:testifylint // need to verify exact match including order for determinism
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
		require.Empty(t, rwis)
	})

	t.Run("does not produce report if an aggregate is missing", func(t *testing.T) {
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
		require.Empty(t, rwis)
	})

	t.Run("skips reports if codec is missing", func(t *testing.T) {
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
		require.Empty(t, rwis)
	})

	t.Run("generates specimen report for non-production LifeCycleStage", func(t *testing.T) {
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
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":1,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"t":0,"v":"1.1"},{"t":0,"v":"2.2"},{"t":1,"v":"Q{Bid: 5.5, Benchmark: 4.4, Ask: 3.3}"}],"Specimen":true}`, string(rwis[0].ReportWithInfo.Report)) //nolint:testifylint // need to verify exact match including order for determinism
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "staging", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].ReportWithInfo.Info)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"t":0,"v":"1.1"},{"t":0,"v":"2.2"},{"t":1,"v":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":true}`, string(rwis[1].ReportWithInfo.Report)) //nolint:testifylint // need to verify exact match including order for determinism
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "staging", ReportFormat: llotypes.ReportFormatJSON}, rwis[1].ReportWithInfo.Info)
	})

	t.Run("generates non-specimen reports for production", func(t *testing.T) {
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
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":1,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"t":0,"v":"1.1"},{"t":0,"v":"2.2"},{"t":1,"v":"Q{Bid: 5.5, Benchmark: 4.4, Ask: 3.3}"}],"Specimen":false}`, string(rwis[0].ReportWithInfo.Report)) //nolint:testifylint // need to verify exact match including order for determinism
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].ReportWithInfo.Info)
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"t":0,"v":"1.1"},{"t":0,"v":"2.2"},{"t":1,"v":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":false}`, string(rwis[1].ReportWithInfo.Report)) //nolint:testifylint // need to verify exact match including order for determinism
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[1].ReportWithInfo.Info)
	})
	t.Run("does not produce reports with overlapping timestamps (where IsReportable returns false)", func(t *testing.T) {
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
		assert.Equal(t, `{"ConfigDigest":"0000000000000000000000000000000000000000000000000000000000000000","SeqNr":2,"ChannelID":2,"ValidAfterNanoseconds":100000000000,"ObservationTimestampNanoseconds":200000000000,"Values":[{"t":0,"v":"1.1"},{"t":0,"v":"2.2"},{"t":1,"v":"Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}"}],"Specimen":false}`, string(rwis[0].ReportWithInfo.Report)) //nolint:testifylint // need to verify exact match including order for determinism
		assert.Equal(t, llo.ReportInfo{LifeCycleStage: "production", ReportFormat: llotypes.ReportFormatJSON}, rwis[0].ReportWithInfo.Info)
	})
	t.Run("sends telemetry on telemetry channel if set, and does not block on full channel", func(t *testing.T) {
		ch := make(chan *LLOReportTelemetry, 2)
		p.DonID = 1001
		p.ReportTelemetryCh = ch
		p.ConfigDigest = types.ConfigDigest{1, 2, 3}
		seqNr := uint64(42)

		outcome := Outcome{
			LifeCycleStage:                  LifeCycleStageProduction,
			ObservationTimestampNanoseconds: uint64(200 * time.Second),
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				1: uint64(100 * time.Second),
				2: uint64(101 * time.Second),
				3: uint64(102 * time.Second),
				4: uint64(103 * time.Second),
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
		_, err = p.Reports(ctx, seqNr, encoded)
		require.NoError(t, err)
		close(ch)

		var telemetries []*LLOReportTelemetry
		for telemetry := range ch {
			telemetries = append(telemetries, telemetry)
		}
		require.Len(t, telemetries, 2)

		t0 := telemetries[0]
		assert.Equal(t, uint32(1), t0.ChannelId)
		assert.Equal(t, uint64(100*time.Second), t0.ValidAfterNanoseconds)
		assert.Equal(t, uint64(200*time.Second), t0.ObservationTimestampNanoseconds)
		assert.Equal(t, uint32(smallDefinitions[1].ReportFormat), t0.ReportFormat)
		assert.False(t, t0.Specimen)

		require.Len(t, t0.StreamDefinitions, 3)
		for i, s := range smallDefinitions[1].Streams {
			assert.Equal(t, s.StreamID, t0.StreamDefinitions[i].StreamID)
			assert.Equal(t, uint32(s.Aggregator), t0.StreamDefinitions[i].Aggregator)
		}

		require.Len(t, t0.StreamValues, 3)
		assert.Equal(t, "1.1", mustToString(t, t0.StreamValues[0]))
		assert.Equal(t, "2.2", mustToString(t, t0.StreamValues[1]))
		assert.Equal(t, "Q{Bid: 5.5, Benchmark: 4.4, Ask: 3.3}", mustToString(t, t0.StreamValues[2]))

		assert.Equal(t, seqNr, t0.SeqNr)
		assert.Equal(t, p.ConfigDigest[:], t0.ConfigDigest)
		assert.Equal(t, uint32(1001), t0.DonId)

		t1 := telemetries[1]
		assert.Equal(t, uint32(2), t1.ChannelId)
		assert.Equal(t, uint64(101*time.Second), t1.ValidAfterNanoseconds)
		assert.Equal(t, uint64(200*time.Second), t1.ObservationTimestampNanoseconds)
		assert.Equal(t, uint32(smallDefinitions[1].ReportFormat), t1.ReportFormat)
		assert.False(t, t1.Specimen)

		require.Len(t, t1.StreamDefinitions, 3)
		for i, s := range smallDefinitions[2].Streams {
			assert.Equal(t, s.StreamID, t1.StreamDefinitions[i].StreamID)
			assert.Equal(t, uint32(s.Aggregator), t1.StreamDefinitions[i].Aggregator)
		}

		require.Len(t, t1.StreamValues, 3)
		assert.Equal(t, "1.1", mustToString(t, t1.StreamValues[0]))
		assert.Equal(t, "2.2", mustToString(t, t1.StreamValues[1]))
		assert.Equal(t, "Q{Bid: 8.8, Benchmark: 7.7, Ask: 6.6}", mustToString(t, t1.StreamValues[2]))

		assert.Equal(t, seqNr, t1.SeqNr)
		assert.Equal(t, p.ConfigDigest[:], t1.ConfigDigest)
		assert.Equal(t, uint32(1001), t0.DonId)
	})
}

func mustToString(t *testing.T, p *LLOStreamValue) string {
	t.Helper()
	sv, err := UnmarshalProtoStreamValue(p)
	require.NoError(t, err)
	b, err := sv.MarshalText()
	require.NoError(t, err)
	return string(b)
}
