package llo

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3types"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"
)

func Test_Outcome(t *testing.T) {
	for _, codec := range []OutcomeCodec{protoOutcomeCodecV0{}, protoOutcomeCodecV1{}} {
		t.Run(fmt.Sprintf("OutcomeCodec: %T", codec), func(t *testing.T) {
			testOutcome(t, codec)
		})
	}
}

func testOutcome(t *testing.T, outcomeCodec OutcomeCodec) {
	ctx := tests.Context(t)

	obsCodec, err := NewProtoObservationCodec(logger.Nop(), true)
	require.NoError(t, err)
	p := &Plugin{
		Config:           Config{true},
		OutcomeCodec:     outcomeCodec,
		Logger:           logger.Test(t),
		ObservationCodec: obsCodec,
		DonID:            10000043,
		ConfigDigest:     types.ConfigDigest{1, 2, 3, 4},
		ReportCodecs: map[llotypes.ReportFormat]ReportCodec{
			llotypes.ReportFormatJSON:             mockCodec{timeResolution: ResolutionNanoseconds},
			llotypes.ReportFormatEVMPremiumLegacy: mockCodec{timeResolution: ResolutionSeconds},
		},
		ChannelDefinitionOptsCache: NewChannelDefinitionOptsCache(),
	}
	testStartTS := time.Now()
	testStartNanos := uint64(testStartTS.UnixNano()) //nolint:gosec // safe cast in tests

	t.Run("if number of observers < 2f+1, errors", func(t *testing.T) {
		_, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{})
		require.EqualError(t, err, "invariant violation: expected at least 2f+1 attributed observations, got 0 (f: 0)")
		p.F = 1
		_, err = p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{{}, {}})
		require.EqualError(t, err, "invariant violation: expected at least 2f+1 attributed observations, got 2 (f: 1)")
	})

	t.Run("if seqnr == 1, and has enough observers, emits initial outcome with 'production' LifeCycleStage", func(t *testing.T) {
		outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{
			{
				Observation: []byte{},
				Observer:    commontypes.OracleID(0),
			},
			{
				Observation: []byte{},
				Observer:    commontypes.OracleID(1),
			},
			{
				Observation: []byte{},
				Observer:    commontypes.OracleID(2),
			},
			{
				Observation: []byte{},
				Observer:    commontypes.OracleID(3),
			},
		})
		require.NoError(t, err)

		decoded, err := p.OutcomeCodec.Decode(outcome)
		require.NoError(t, err)

		assert.Equal(t, Outcome{
			LifeCycleStage: "production",
		}, decoded)
	})

	t.Run("channel definitions", func(t *testing.T) {
		t.Run("adds a new channel definition if there are enough votes", func(t *testing.T) {
			newCd := llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormat(2),
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
			}
			obs, err := p.ObservationCodec.Encode(Observation{
				UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
					42: newCd,
				},
			})
			require.NoError(t, err)
			aos := []types.AttributedObservation{}
			for i := uint8(0); i < 4; i++ {
				aos = append(aos,
					types.AttributedObservation{
						Observation: obs,
						Observer:    commontypes.OracleID(i),
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, newCd, decoded.ChannelDefinitions[42])
		})

		t.Run("replaces an existing channel definition if there are enough votes", func(t *testing.T) {
			newCd := llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormat(2),
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorQuote}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
			}
			obs, err := p.ObservationCodec.Encode(Observation{
				UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
					42: newCd,
				},
			})
			require.NoError(t, err)
			aos := []types.AttributedObservation{}
			for i := uint8(0); i < 4; i++ {
				aos = append(aos,
					types.AttributedObservation{
						Observation: obs,
						Observer:    commontypes.OracleID(i),
					})
			}

			previousOutcome, err := p.OutcomeCodec.Encode(Outcome{
				ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
					42: {
						ReportFormat: llotypes.ReportFormat(1),
						Streams:      []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorMedian}},
					},
				},
			})
			require.NoError(t, err)

			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{PreviousOutcome: previousOutcome, SeqNr: 2}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, newCd, decoded.ChannelDefinitions[42])
		})

		t.Run("removes a channel definition if there are enough votes", func(t *testing.T) {
			t.Skip("removal votes are not implemented yet")
			newCd := llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormat(2),
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
			}
			obs, err := p.ObservationCodec.Encode(Observation{
				UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
					42: newCd,
				},
			})
			require.NoError(t, err)
			aos := []types.AttributedObservation{}
			for i := uint8(0); i < 4; i++ {
				aos = append(aos,
					types.AttributedObservation{
						Observation: obs,
						Observer:    commontypes.OracleID(i),
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, newCd, decoded.ChannelDefinitions[42])
		})

		t.Run("does not add channels beyond MaxOutcomeChannelDefinitionsLength", func(t *testing.T) {
			newCd := llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormat(2),
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
			}
			obs := Observation{UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{}}
			for i := uint32(0); i < MaxOutcomeChannelDefinitionsLength+10; i++ {
				obs.UpdateChannelDefinitions[i] = newCd
			}
			encoded, err := p.ObservationCodec.Encode(obs)
			require.NoError(t, err)
			aos := []types.AttributedObservation{}
			for i := uint8(0); i < 4; i++ {
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i),
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Len(t, decoded.ChannelDefinitions, MaxOutcomeChannelDefinitionsLength)

			// should contain channels 0 thru 999
			assert.Contains(t, decoded.ChannelDefinitions, llotypes.ChannelID(0))
			assert.Contains(t, decoded.ChannelDefinitions, llotypes.ChannelID(MaxOutcomeChannelDefinitionsLength-1))
			assert.NotContains(t, decoded.ChannelDefinitions, llotypes.ChannelID(MaxOutcomeChannelDefinitionsLength))
			assert.NotContains(t, decoded.ChannelDefinitions, llotypes.ChannelID(MaxOutcomeChannelDefinitionsLength+1))
		})
	})

	t.Run("stream observations", func(t *testing.T) {
		smallDefinitions := map[llotypes.ChannelID]llotypes.ChannelDefinition{
			1: {
				ReportFormat: llotypes.ReportFormatJSON,
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorQuote}},
			},
			2: {
				ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorQuote}},
			},
		}

		t.Run("aggregates values when all stream values are present from all observers", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartNanos,
				ChannelDefinitions:              smallDefinitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)
			outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
				obs := Observation{
					UnixTimestampNanoseconds: testStartNanos + uint64(time.Second) + uint64(i*100)*uint64(time.Millisecond), //nolint:gosec // safe cast in tests
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(100 + i*10))),
						2: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(int64(200 + i*10)))},
						3: &Quote{Bid: decimal.NewFromInt(int64(300 + i*10)), Benchmark: decimal.NewFromInt(int64(310 + i*10)), Ask: decimal.NewFromInt(int64(320 + i*10))},
					}}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
					})
			}
			outcome, err := p.Outcome(ctx, outctx, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			observationsTs := decoded.ObservationTimestampNanoseconds
			assert.GreaterOrEqual(t, observationsTs, uint64(testStartTS.UnixNano()+1_200_000_000)) //nolint:gosec // time won't be negative

			// NOTE: In protoOutcomeCodecV0 precision is lost on timestamp
			// serialization, so validAfterNanoseconds will be truncated to
			// seconds
			expectedValidAfterSeconds := observationsTs
			if _, ok := p.OutcomeCodec.(protoOutcomeCodecV0); ok {
				expectedValidAfterSeconds = (observationsTs / 1e9) * 1e9
			}

			assert.Equal(t, Outcome{
				LifeCycleStage:                  "test",
				ObservationTimestampNanoseconds: observationsTs,
				ChannelDefinitions:              smallDefinitions,
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: expectedValidAfterSeconds, // set to median observation timestamp
					2: expectedValidAfterSeconds,
				},
				StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
					1: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(120)),
					},
					2: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(220))},
					},
					3: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorQuote: &Quote{Bid: decimal.NewFromInt(320), Benchmark: decimal.NewFromInt(330), Ask: decimal.NewFromInt(340)},
					},
				},
			}, decoded)
		})
		t.Run("unreportable channels from the previous outcome re-use the same previous ValidAfterNanoseconds", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: uint64(102030410 * time.Second),
				ChannelDefinitions:              nil, // nil channel definitions makes all channels unreportable
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: uint64(102030405 * time.Second),
					2: uint64(102030400 * time.Second),
				},
				StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
					1: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(120)),
					},
					2: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(220)),
					},
					3: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorQuote: &Quote{Bid: decimal.NewFromInt(320), Benchmark: decimal.NewFromInt(330), Ask: decimal.NewFromInt(340)},
					},
				},
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
				obs := Observation{
					UnixTimestampNanoseconds: uint64(102030415 * time.Second),
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(120))),
						2: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(int64(220)))},
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					},
				}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, uint64(102030415*time.Second), decoded.ObservationTimestampNanoseconds)
			require.Len(t, decoded.ValidAfterNanoseconds, 2)
			assert.Equal(t, uint64(102030405*time.Second), decoded.ValidAfterNanoseconds[1])
			assert.Equal(t, uint64(102030400*time.Second), decoded.ValidAfterNanoseconds[2])
		})
		t.Run("ValidAfterNanoseconds is set based on the previous observation timestamp such that reports never overlap", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: uint64(102030410 * time.Second),
				ChannelDefinitions:              smallDefinitions,
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: uint64(102030405 * time.Second),
					2: uint64(102030400 * time.Second),
				},
				StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
					1: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(120)),
					},
					2: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(220)),
					},
					3: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorQuote: &Quote{Bid: decimal.NewFromInt(320), Benchmark: decimal.NewFromInt(330), Ask: decimal.NewFromInt(340)},
					},
				},
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
				obs := Observation{
					UnixTimestampNanoseconds: uint64(102030415 * time.Second),
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(120))),
						2: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(int64(220)))},
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					},
				}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, uint64(102030415*time.Second), decoded.ObservationTimestampNanoseconds)
			require.Len(t, decoded.ValidAfterNanoseconds, 2)
			assert.Equal(t, uint64(102030410*time.Second), decoded.ValidAfterNanoseconds[1])
			assert.Equal(t, uint64(102030410*time.Second), decoded.ValidAfterNanoseconds[2])
		})
		t.Run("does generate outcome for reports that would overlap on a seconds-basis (allows duplicate reports)", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: uint64(102030410 * time.Second),
				ChannelDefinitions:              smallDefinitions,
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: uint64(102030409 * time.Second),
					2: uint64(102030409 * time.Second),
				},
				StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
					1: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(120)),
					},
					2: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(220)),
					},
					3: map[llotypes.Aggregator]StreamValue{
						llotypes.AggregatorQuote: &Quote{Bid: decimal.NewFromInt(320), Benchmark: decimal.NewFromInt(330), Ask: decimal.NewFromInt(340)},
					},
				},
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)

			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
				obs := Observation{
					UnixTimestampNanoseconds: uint64((102030410 * time.Second) + 100*time.Millisecond), // 100ms after previous outcome
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(120))),
						2: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(int64(220)))},
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					},
				}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, uint64(102030410*time.Second+100*time.Millisecond), decoded.ObservationTimestampNanoseconds)
			require.Len(t, decoded.ValidAfterNanoseconds, 2)
			assert.Equal(t, uint64(102030410*time.Second), decoded.ValidAfterNanoseconds[1])
			assert.Equal(t, uint64(102030410*time.Second), decoded.ValidAfterNanoseconds[2])
		})
		t.Run("aggregation function returns error", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartNanos,
				ChannelDefinitions:              smallDefinitions,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)
			outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
				var sv StreamValue
				// only one reported a value; not enough
				if i == 0 {
					sv = ToDecimal(decimal.NewFromInt(100))
				}
				obs := Observation{
					UnixTimestampNanoseconds: testStartNanos + uint64(time.Second) + uint64(i*100)*uint64(time.Millisecond), //nolint:gosec // safe cast in tests
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: sv,
						// 2 and 3 ok
						2: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(int64(220)))},
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					}}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
					})
			}
			outcome, err := p.Outcome(ctx, outctx, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			// NOTE: `1` is missing because of insufficient observations
			assert.Len(t, decoded.StreamAggregates, 2)
			assert.Contains(t, decoded.StreamAggregates, llotypes.StreamID(2))
			assert.Contains(t, decoded.StreamAggregates, llotypes.StreamID(3))
			assert.Equal(t, map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorMedian: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(220))},
			}, decoded.StreamAggregates[2])
			assert.Equal(t, map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorQuote: &Quote{Bid: decimal.NewFromInt(320), Benchmark: decimal.NewFromInt(330), Ask: decimal.NewFromInt(340)},
			}, decoded.StreamAggregates[3])
		})
		t.Run("sends outcome telemetry if channel is specified", func(t *testing.T) {
			ch := make(chan *LLOOutcomeTelemetry, 10000)
			p.OutcomeTelemetryCh = ch
			previousOutcome := Outcome{
				LifeCycleStage:                  llotypes.LifeCycleStage("test"),
				ObservationTimestampNanoseconds: testStartNanos,
				ChannelDefinitions:              smallDefinitions,
				ValidAfterNanoseconds:           nil,
				StreamAggregates:                nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)
			outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
				obs := Observation{
					UnixTimestampNanoseconds: testStartNanos + uint64(time.Second) + uint64(i*100)*uint64(time.Millisecond), //nolint:gosec // safe cast in tests
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(100 + i*10))),
						2: &TimestampedStreamValue{ObservedAtNanoseconds: 123456789, StreamValue: ToDecimal(decimal.NewFromInt(int64(200 + i*10)))},
						3: &Quote{Bid: decimal.NewFromInt(int64(300 + i*10)), Benchmark: decimal.NewFromInt(int64(310 + i*10)), Ask: decimal.NewFromInt(int64(320 + i*10))},
					}}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
					})
			}
			outcome, err := p.Outcome(ctx, outctx, types.Query{}, aos)
			require.NoError(t, err)
			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			telem := <-ch
			assert.Equal(t, string(decoded.LifeCycleStage), telem.LifeCycleStage)
			assert.Equal(t, decoded.ObservationTimestampNanoseconds, telem.ObservationTimestampNanoseconds)
			assert.Equal(t, len(decoded.ChannelDefinitions), len(telem.ChannelDefinitions))
			assert.Equal(t, len(decoded.ValidAfterNanoseconds), len(telem.ValidAfterNanoseconds))
			assert.Equal(t, len(decoded.StreamAggregates), len(telem.StreamAggregates))
			assert.Equal(t, uint64(2), telem.SeqNr)
			assert.Equal(t, p.ConfigDigest[:], telem.ConfigDigest)
			assert.Equal(t, p.DonID, telem.DonId)
		})
		t.Run("handles TimestampedStreamValue correctly", func(t *testing.T) {
			timestamped := map[llotypes.ChannelID]llotypes.ChannelDefinition{
				1: {
					ReportFormat: llotypes.ReportFormatJSON,
					Streams: []llotypes.Stream{
						{StreamID: 1, Aggregator: llotypes.AggregatorMedian},
						{StreamID: 2, Aggregator: llotypes.AggregatorMedian},
						{StreamID: 3, Aggregator: llotypes.AggregatorMedian},
					},
				},
			}
			t.Run("writes values in if its a brand new stream", func(t *testing.T) {
				previousOutcome := Outcome{
					LifeCycleStage:                  llotypes.LifeCycleStage("test"),
					ObservationTimestampNanoseconds: testStartNanos,
					ChannelDefinitions:              timestamped,
					ValidAfterNanoseconds:           nil,
					StreamAggregates:                nil,
				}
				encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
				require.NoError(t, err)
				outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
				aos := []types.AttributedObservation{}
				for i := 0; i < 4; i++ {
					obs := Observation{
						UnixTimestampNanoseconds: testStartNanos + uint64(time.Second) + uint64(i*100)*uint64(time.Millisecond), //nolint:gosec // safe cast in tests
						StreamValues: map[llotypes.StreamID]StreamValue{
							1: &TimestampedStreamValue{ObservedAtNanoseconds: 100000000 + uint64(i), StreamValue: ToDecimal(decimal.NewFromInt(int64(100 + i)))}, //nolint:gosec // will never be > 4
							2: &TimestampedStreamValue{ObservedAtNanoseconds: 200000000 + uint64(i), StreamValue: ToDecimal(decimal.NewFromInt(int64(200 + i)))}, //nolint:gosec // will never be > 4
							3: &TimestampedStreamValue{ObservedAtNanoseconds: 300000000 + uint64(i), StreamValue: ToDecimal(decimal.NewFromInt(int64(300 + i)))}, //nolint:gosec // will never be > 4
						}}
					encoded, err2 := p.ObservationCodec.Encode(obs)
					require.NoError(t, err2)
					aos = append(aos,
						types.AttributedObservation{
							Observation: encoded,
							Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
						})
				}
				outcome, err := p.Outcome(ctx, outctx, types.Query{}, aos)
				require.NoError(t, err)

				decoded, err := p.OutcomeCodec.Decode(outcome)
				require.NoError(t, err)

				require.Len(t, decoded.StreamAggregates, 3)
				assert.Equal(t, &TimestampedStreamValue{ObservedAtNanoseconds: 100000002, StreamValue: ToDecimal(decimal.NewFromInt(int64(102)))}, decoded.StreamAggregates[1][llotypes.AggregatorMedian])
				assert.Equal(t, &TimestampedStreamValue{ObservedAtNanoseconds: 200000002, StreamValue: ToDecimal(decimal.NewFromInt(int64(202)))}, decoded.StreamAggregates[2][llotypes.AggregatorMedian])
				assert.Equal(t, &TimestampedStreamValue{ObservedAtNanoseconds: 300000002, StreamValue: ToDecimal(decimal.NewFromInt(int64(302)))}, decoded.StreamAggregates[3][llotypes.AggregatorMedian])
			})
			t.Run("copies forwards values from the last outcome if aggregation fails", func(t *testing.T) {
			})
			t.Run("does not copy forwards values from the last outcome that are no longer in channel definitions", func(t *testing.T) {
			})
			t.Run("copies forwards values from last outcome if old value was a different type", func(t *testing.T) {
			})
			t.Run("copies forwards values from last outcome if old value had a newer timestamp", func(t *testing.T) {
			})
			t.Run("replaces value with new aggregation output if timestamp is newer", func(t *testing.T) {
			})
		})
	})
	t.Run("if previousOutcome is retired, returns outcome as normal", func(t *testing.T) {
		previousOutcome := Outcome{
			LifeCycleStage: llotypes.LifeCycleStage("retired"),
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				1: uint64(102030409 * time.Second),
				2: uint64(102030409 * time.Second),
			},
		}
		encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
		require.NoError(t, err)

		aos := []types.AttributedObservation{}
		for i := 0; i < 4; i++ {
			obs := Observation{
				UnixTimestampNanoseconds: uint64(102030415 * time.Second),
			}
			encoded, err2 := p.ObservationCodec.Encode(obs)
			require.NoError(t, err2)
			aos = append(aos,
				types.AttributedObservation{
					Observation: encoded,
					Observer:    commontypes.OracleID(i), //nolint:gosec // will never be > 4
				})
		}
		outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
		require.NoError(t, err)

		decoded, err := p.OutcomeCodec.Decode(outcome)
		require.NoError(t, err)

		assert.Equal(t, uint64(102030415000000000), decoded.ObservationTimestampNanoseconds)
		require.Len(t, decoded.ValidAfterNanoseconds, 2)
		assert.Equal(t, uint64(102030409*time.Second), decoded.ValidAfterNanoseconds[1])
		assert.Equal(t, uint64(102030409*time.Second), decoded.ValidAfterNanoseconds[2])
	})
}

func Test_MakeChannelHash(t *testing.T) {
	t.Run("hashes channel definitions", func(t *testing.T) {
		defs := ChannelDefinitionWithID{
			ChannelID: 1,
			ChannelDefinition: llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormat(1),
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
				Opts:         []byte(`{}`),
			},
		}
		hash := MakeChannelHash(defs)
		// NOTE: Breaking this test by changing the hash below may break existing running instances
		assert.Equal(t, "c0b72f4acb79bb8f5075f979f86016a30159266a96870b1c617b44426337162a", fmt.Sprintf("%x", hash))
	})

	t.Run("different channelID makes different hash", func(t *testing.T) {
		def1 := ChannelDefinitionWithID{ChannelID: 1}
		def2 := ChannelDefinitionWithID{ChannelID: 2}

		assert.NotEqual(t, MakeChannelHash(def1), MakeChannelHash(def2))
	})

	t.Run("different report format makes different hash", func(t *testing.T) {
		def1 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormatJSON,
			},
		}
		def2 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormatEVMPremiumLegacy,
			},
		}

		assert.NotEqual(t, MakeChannelHash(def1), MakeChannelHash(def2))
	})

	t.Run("different streamIDs makes different hash", func(t *testing.T) {
		def1 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			},
		}
		def2 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{{StreamID: 2, Aggregator: llotypes.AggregatorMedian}},
			},
		}

		assert.NotEqual(t, MakeChannelHash(def1), MakeChannelHash(def2))
	})

	t.Run("different aggregators makes different hash", func(t *testing.T) {
		def1 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}},
			},
		}
		def2 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				Streams: []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorQuote}},
			},
		}

		assert.NotEqual(t, MakeChannelHash(def1), MakeChannelHash(def2))
	})

	t.Run("different opts makes different hash", func(t *testing.T) {
		def1 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				Opts: []byte(`{"foo":"bar"}`),
			},
		}
		def2 := ChannelDefinitionWithID{
			ChannelDefinition: llotypes.ChannelDefinition{
				Opts: []byte(`{"foo":"baz"}`),
			},
		}

		assert.NotEqual(t, MakeChannelHash(def1), MakeChannelHash(def2))
	})
}

type mockCodec struct {
	timeResolution TimeResolution
}

var (
	_ ReportCodec = mockCodec{}
	_ OptsParser  = mockCodec{}
)

func (mockCodec) Encode(Report, llotypes.ChannelDefinition) ([]byte, error) {
	return nil, nil
}

func (mockCodec) Verify(llotypes.ChannelDefinition) error {
	return nil
}

func (c mockCodec) ParseOpts(opts []byte) (interface{}, error) {
	// TODO do we need to parse opts in anyway here?
	return c, nil
}

func (c mockCodec) TimeResolution(parsedOpts interface{}) (TimeResolution, error) {
	if tc, ok := parsedOpts.(mockCodec); ok {
		return tc.timeResolution, nil
	}
	return c.timeResolution, nil
}

func Test_Outcome_Methods(t *testing.T) {
	// Cache for parsed channel definition opts
	optsCache := NewChannelDefinitionOptsCache()
	// Use test codecs that mimic real codec behavior
	codecs := map[llotypes.ReportFormat]ReportCodec{
		llotypes.ReportFormat(0):                      mockCodec{timeResolution: ResolutionNanoseconds},
		llotypes.ReportFormatEVMPremiumLegacy:         mockCodec{timeResolution: ResolutionSeconds},
		llotypes.ReportFormatEVMABIEncodeUnpacked:     mockCodec{timeResolution: ResolutionNanoseconds},
		llotypes.ReportFormatEVMABIEncodeUnpackedExpr: mockCodec{timeResolution: ResolutionNanoseconds},
	}

	t.Run("protocol version 0", func(t *testing.T) {
		t.Run("IsReportable", func(t *testing.T) {
			outcome := Outcome{}
			cid := llotypes.ChannelID(1)

			// Not reportable if retired
			outcome.LifeCycleStage = LifeCycleStageRetired
			require.EqualError(t, outcome.IsReportable(cid, 0, 0, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; retired channel")

			// No channel definition with ID
			outcome.LifeCycleStage = LifeCycleStageProduction
			outcome.ObservationTimestampNanoseconds = uint64(time.Unix(1726670490, 0).UnixNano()) //nolint:gosec // time won't be negative
			outcome.ChannelDefinitions = map[llotypes.ChannelID]llotypes.ChannelDefinition{}
			require.EqualError(t, outcome.IsReportable(cid, 0, 0, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; no channel definition with this ID")

			// No ValidAfterNanoseconds yet
			outcome.ChannelDefinitions = map[llotypes.ChannelID]llotypes.ChannelDefinition{
				cid: {},
			}
			require.EqualError(t, outcome.IsReportable(cid, 0, 0, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; no ValidAfterNanoseconds entry yet, this must be a new channel")

			// ValidAfterNanoseconds is in the future
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: uint64(1726670491 * time.Second)}
			require.EqualError(t, outcome.IsReportable(cid, 0, 0, codecs, optsCache), "ChannelID: 1; Reason: ChannelID: 1; Reason: IsReportable=false; not valid yet (observationsTimestampSeconds=1726670490, validAfterSeconds=1726670491)")

			// ValidAfterSeconds=ObservationTimestampSeconds; IsReportable=false
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: uint64(1726670490 * time.Second)}
			require.EqualError(t, outcome.IsReportable(cid, 0, 0, codecs, optsCache), "ChannelID: 1; Reason: ChannelID: 1; Reason: IsReportable=false; not valid yet (observationsTimestampSeconds=1726670490, validAfterSeconds=1726670490)")

			// ValidAfterSeconds<ObservationTimestampSeconds; IsReportable=false
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: uint64(1726670489 * time.Second)}
			require.Nil(t, outcome.IsReportable(cid, 0, 0, codecs, optsCache))
		})
		t.Run("ReportableChannels", func(t *testing.T) {
			outcome := Outcome{
				ObservationTimestampNanoseconds: uint64(time.Unix(1726670490, 0).UnixNano()), //nolint:gosec // time won't be negative
				ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
					1: {},
					2: {},
					3: {},
				},
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: uint64(1726670489 * time.Second),
					3: uint64(1726670489 * time.Second),
				},
			}
			reportable, unreportable := outcome.ReportableChannels(0, 0, codecs, optsCache)
			assert.Equal(t, []llotypes.ChannelID{1, 3}, reportable)
			require.Len(t, unreportable, 1)
			assert.Equal(t, "ChannelID: 2; Reason: IsReportable=false; no ValidAfterNanoseconds entry yet, this must be a new channel", unreportable[0].Error())
		})
	})
	t.Run("protocol version > 0", func(t *testing.T) {
		t.Run("IsReportable", func(t *testing.T) {
			defaultMinReportInterval := uint64(100 * time.Millisecond)

			outcome := Outcome{}
			cid := llotypes.ChannelID(1)

			// Not reportable if retired
			outcome.LifeCycleStage = LifeCycleStageRetired
			require.EqualError(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; retired channel")

			obsTSNanos := uint64(time.Unix(1726670490, 1000).UnixNano()) //nolint:gosec // time won't be negative

			// No channel definition with ID
			outcome.LifeCycleStage = LifeCycleStageProduction
			outcome.ObservationTimestampNanoseconds = obsTSNanos
			outcome.ChannelDefinitions = map[llotypes.ChannelID]llotypes.ChannelDefinition{}
			require.EqualError(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; no channel definition with this ID")

			// No ValidAfterNanoseconds yet
			outcome.ChannelDefinitions[cid] = llotypes.ChannelDefinition{}
			require.EqualError(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; no ValidAfterNanoseconds entry yet, this must be a new channel")

			// ValidAfterNanoseconds is 1ns in the future; IsReportable=false
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: obsTSNanos + 1}
			require.EqualError(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; not valid yet (ObservationTimestampNanoseconds=1726670490000001000, validAfterNanoseconds=1726670490000001001, minReportInterval=100000000); 0.100000 seconds (100000001ns) until reportable")

			// ValidAfterNanoseconds is 1s in the future; IsReportable=false
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: obsTSNanos + uint64(1*time.Second)}
			require.EqualError(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; not valid yet (ObservationTimestampNanoseconds=1726670490000001000, validAfterNanoseconds=1726670491000001000, minReportInterval=100000000); 1.100000 seconds (1100000000ns) until reportable")

			// ValidAfterNanoseconds is 100ms-1ns in the past; IsReportable=false
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: obsTSNanos - uint64(100*time.Millisecond) + 1}
			require.EqualError(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; not valid yet (ObservationTimestampNanoseconds=1726670490000001000, validAfterNanoseconds=1726670489900001001, minReportInterval=100000000); 0.000000 seconds (1ns) until reportable")

			// ValidAfterNanoseconds is exactly 100ms in the past; IsReportable=true
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: obsTSNanos - uint64(100*time.Millisecond)}
			require.Nil(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache))

			// ValidAfterNanoseconds is 100ms+1ns in the past; IsReportable=true
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: obsTSNanos - uint64(100*time.Millisecond) - 1}
			require.Nil(t, outcome.IsReportable(cid, 1, defaultMinReportInterval, codecs, optsCache))

			// zero report cadence allows overlaps (but still respects seconds resolution boundary)
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{cid: obsTSNanos - uint64(1*time.Second)}
			require.Nil(t, outcome.IsReportable(cid, 1, 0, codecs, optsCache))
		})
		t.Run("IsReportable with seconds resolution", func(t *testing.T) {
			outcome := Outcome{}
			cid := llotypes.ChannelID(1)

			obsTSNanos := uint64(time.Unix(1726670490, 1e9-1).UnixNano()) //nolint:gosec // time won't be negative

			outcome.LifeCycleStage = LifeCycleStageProduction
			outcome.ObservationTimestampNanoseconds = obsTSNanos
			outcome.ChannelDefinitions = map[llotypes.ChannelID]llotypes.ChannelDefinition{
				cid: {ReportFormat: llotypes.ReportFormatEVMPremiumLegacy},
			}
			outcome.ValidAfterNanoseconds = map[llotypes.ChannelID]uint64{
				cid: obsTSNanos - uint64(500*time.Millisecond),
			}

			// OptsCache should be populated after successful channel voting 
			cd := outcome.ChannelDefinitions[cid]
			_ = optsCache.Set(cid, cd.Opts, codecs[cd.ReportFormat])

			// if cadence is 0, but time is < 1s, does not report to avoid overlap
			require.EqualError(t, outcome.IsReportable(cid, 1, uint64(0), codecs, optsCache), "ChannelID: 1; Reason: ChannelID: 1; Reason: IsReportable=false; not valid yet (observationsTimestampSeconds=1726670490, validAfterSeconds=1726670490)")
			// if cadence is < 1s, if time is < 1s, does not report to avoid overlap
			require.EqualError(t, outcome.IsReportable(cid, 1, uint64(100*time.Millisecond), codecs, optsCache), "ChannelID: 1; Reason: ChannelID: 1; Reason: IsReportable=false; not valid yet (observationsTimestampSeconds=1726670490, validAfterSeconds=1726670490)")
			// if cadence is < 1s, if time is >= 1s, does report
			outcome.ValidAfterNanoseconds[cid] = obsTSNanos - uint64(1*time.Second)
			assert.Nil(t, outcome.IsReportable(cid, 1, uint64(100*time.Millisecond), codecs, optsCache))
			// if cadence is exactly 1s, if time is >= 1s, does report
			assert.Nil(t, outcome.IsReportable(cid, 1, uint64(1*time.Second), codecs, optsCache))
			// if cadence is 5s, if time is < 5s, does not report because cadence hasn't elapsed
			require.EqualError(t, outcome.IsReportable(cid, 1, uint64(5*time.Second), codecs, optsCache), "ChannelID: 1; Reason: IsReportable=false; not valid yet (ObservationTimestampNanoseconds=1726670490999999999, validAfterNanoseconds=1726670489999999999, minReportInterval=5000000000); 4.000000 seconds (4000000000ns) until reportable")
		})
		t.Run("ReportableChannels", func(t *testing.T) {
			defaultMinReportInterval := uint64(1 * time.Second)

			outcome := Outcome{
				ObservationTimestampNanoseconds: uint64(time.Unix(1726670490, 0).UnixNano()), //nolint:gosec // time won't be negative
				ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
					1: {},
					2: {},
					3: {},
				},
				ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
					1: uint64(1726670489 * time.Second),
					3: uint64(1726670489 * time.Second),
				},
			}
			reportable, unreportable := outcome.ReportableChannels(1, defaultMinReportInterval, codecs, optsCache)
			assert.Equal(t, []llotypes.ChannelID{1, 3}, reportable)
			require.Len(t, unreportable, 1)
			assert.Equal(t, "ChannelID: 2; Reason: IsReportable=false; no ValidAfterNanoseconds entry yet, this must be a new channel", unreportable[0].Error())
		})
	})
}
