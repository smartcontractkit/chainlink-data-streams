package llo

import (
	"fmt"
	"math"
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
	ctx := tests.Context(t)
	p := &Plugin{
		Config:           Config{true},
		OutcomeCodec:     protoOutcomeCodec{},
		Logger:           logger.Test(t),
		ObservationCodec: protoObservationCodec{},
	}

	t.Run("if number of observers < 2f+1, errors", func(t *testing.T) {
		_, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{})
		assert.EqualError(t, err, "invariant violation: expected at least 2f+1 attributed observations, got 0 (f: 0)")
		p.F = 1
		_, err = p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{{}, {}})
		assert.EqualError(t, err, "invariant violation: expected at least 2f+1 attributed observations, got 2 (f: 1)")
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
			for i := 0; i < 4; i++ {
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
			for i := 0; i < 4; i++ {
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

		t.Run("does not add channels beyond MaxOutcomeChannelDefinitionsLength", func(t *testing.T) {
			newCd := llotypes.ChannelDefinition{
				ReportFormat: llotypes.ReportFormat(2),
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorMedian}, {StreamID: 3, Aggregator: llotypes.AggregatorMedian}},
			}
			obs := Observation{UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{}}
			for i := 0; i < MaxOutcomeChannelDefinitionsLength+10; i++ {
				obs.UpdateChannelDefinitions[llotypes.ChannelID(i)] = newCd
			}
			encoded, err := p.ObservationCodec.Encode(obs)
			require.NoError(t, err)
			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
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
		testStartTS := time.Now()
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
		cdc := &mockChannelDefinitionCache{definitions: smallDefinitions}

		t.Run("aggregates values when all stream values are present from all observers", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                   llotypes.LifeCycleStage("test"),
				ObservationsTimestampNanoseconds: testStartTS.UnixNano(),
				ChannelDefinitions:               cdc.definitions,
				ValidAfterSeconds:                nil,
				StreamAggregates:                 nil,
			}
			encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
			require.NoError(t, err)
			outctx := ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}
			aos := []types.AttributedObservation{}
			for i := 0; i < 4; i++ {
				obs := Observation{
					UnixTimestampNanoseconds: testStartTS.UnixNano() + int64(time.Second) + int64(i*100)*int64(time.Millisecond),
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(100 + i*10))),
						2: ToDecimal(decimal.NewFromInt(int64(200 + i*10))),
						3: &Quote{Bid: decimal.NewFromInt(int64(300 + i*10)), Benchmark: decimal.NewFromInt(int64(310 + i*10)), Ask: decimal.NewFromInt(int64(320 + i*10))},
					}}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i),
					})
			}
			outcome, err := p.Outcome(ctx, outctx, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			observationsTs := decoded.ObservationsTimestampNanoseconds
			assert.GreaterOrEqual(t, observationsTs, testStartTS.UnixNano()+1_200_000_000)

			assert.Equal(t, Outcome{
				LifeCycleStage:                   "test",
				ObservationsTimestampNanoseconds: observationsTs,
				ChannelDefinitions:               cdc.definitions,
				ValidAfterSeconds: map[llotypes.ChannelID]uint32{
					1: uint32(observationsTs / int64(time.Second)), // set to median observation timestamp
					2: uint32(observationsTs / int64(time.Second)),
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
			}, decoded)
		})
		t.Run("unreportable channels from the previous outcome re-use the same previous ValidAfterSeconds", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                   llotypes.LifeCycleStage("test"),
				ObservationsTimestampNanoseconds: int64(102030410 * time.Second),
				ChannelDefinitions:               nil, // nil channel definitions makes all channels unreportable
				ValidAfterSeconds: map[llotypes.ChannelID]uint32{
					1: uint32(102030405),
					2: uint32(102030400),
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
					UnixTimestampNanoseconds: int64(102030415 * time.Second),
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(120))),
						2: ToDecimal(decimal.NewFromInt(int64(220))),
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					},
				}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i),
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, int64(102030415*time.Second), decoded.ObservationsTimestampNanoseconds)
			require.Len(t, decoded.ValidAfterSeconds, 2)
			assert.Equal(t, int64(102030405), int64(decoded.ValidAfterSeconds[1]))
			assert.Equal(t, int64(102030400), int64(decoded.ValidAfterSeconds[2]))
		})
		t.Run("ValidAfterSeconds is set based on the previous observation timestamp such that reports never overlap", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                   llotypes.LifeCycleStage("test"),
				ObservationsTimestampNanoseconds: int64(102030410 * time.Second),
				ChannelDefinitions:               cdc.definitions,
				ValidAfterSeconds: map[llotypes.ChannelID]uint32{
					1: uint32(102030405),
					2: uint32(102030400),
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
					UnixTimestampNanoseconds: int64(102030415 * time.Second),
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(120))),
						2: ToDecimal(decimal.NewFromInt(int64(220))),
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					},
				}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i),
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, int64(102030415*time.Second), decoded.ObservationsTimestampNanoseconds)
			require.Len(t, decoded.ValidAfterSeconds, 2)
			assert.Equal(t, int64(102030410), int64(decoded.ValidAfterSeconds[1]))
			assert.Equal(t, int64(102030410), int64(decoded.ValidAfterSeconds[2]))
		})
		t.Run("does generate outcome for reports that would overlap on a seconds-basis (allows duplicate reports)", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                   llotypes.LifeCycleStage("test"),
				ObservationsTimestampNanoseconds: int64(102030410 * time.Second),
				ChannelDefinitions:               cdc.definitions,
				ValidAfterSeconds: map[llotypes.ChannelID]uint32{
					1: uint32(102030409),
					2: uint32(102030409),
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
					UnixTimestampNanoseconds: int64((102030410 * time.Second) + 100*time.Millisecond), // 100ms after previous outcome
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: ToDecimal(decimal.NewFromInt(int64(120))),
						2: ToDecimal(decimal.NewFromInt(int64(220))),
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					},
				}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i),
					})
			}
			outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
			require.NoError(t, err)

			decoded, err := p.OutcomeCodec.Decode(outcome)
			require.NoError(t, err)

			assert.Equal(t, int64(102030410*time.Second+100*time.Millisecond), decoded.ObservationsTimestampNanoseconds)
			require.Len(t, decoded.ValidAfterSeconds, 2)
			assert.Equal(t, int64(102030410), int64(decoded.ValidAfterSeconds[1]))
			assert.Equal(t, int64(102030410), int64(decoded.ValidAfterSeconds[2]))
		})
		t.Run("aggregation function returns error", func(t *testing.T) {
			previousOutcome := Outcome{
				LifeCycleStage:                   llotypes.LifeCycleStage("test"),
				ObservationsTimestampNanoseconds: testStartTS.UnixNano(),
				ChannelDefinitions:               cdc.definitions,
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
					UnixTimestampNanoseconds: testStartTS.UnixNano() + int64(time.Second) + int64(i*100)*int64(time.Millisecond),
					StreamValues: map[llotypes.StreamID]StreamValue{
						1: sv,
						// 2 and 3 ok
						2: ToDecimal(decimal.NewFromInt(int64(220))),
						3: &Quote{Bid: decimal.NewFromInt(int64(320)), Benchmark: decimal.NewFromInt(int64(330)), Ask: decimal.NewFromInt(int64(340))},
					}}
				encoded, err2 := p.ObservationCodec.Encode(obs)
				require.NoError(t, err2)
				aos = append(aos,
					types.AttributedObservation{
						Observation: encoded,
						Observer:    commontypes.OracleID(i),
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
				llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(220)),
			}, decoded.StreamAggregates[2])
			assert.Equal(t, map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorQuote: &Quote{Bid: decimal.NewFromInt(320), Benchmark: decimal.NewFromInt(330), Ask: decimal.NewFromInt(340)},
			}, decoded.StreamAggregates[3])
		})
	})
	t.Run("if previousOutcome is retired, returns outcome as normal", func(t *testing.T) {
		previousOutcome := Outcome{
			LifeCycleStage: llotypes.LifeCycleStage("retired"),
			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				1: uint32(102030409),
				2: uint32(102030409),
			},
		}
		encodedPreviousOutcome, err := p.OutcomeCodec.Encode(previousOutcome)
		require.NoError(t, err)

		aos := []types.AttributedObservation{}
		for i := 0; i < 4; i++ {
			obs := Observation{
				UnixTimestampNanoseconds: int64(102030415 * time.Second),
			}
			encoded, err2 := p.ObservationCodec.Encode(obs)
			require.NoError(t, err2)
			aos = append(aos,
				types.AttributedObservation{
					Observation: encoded,
					Observer:    commontypes.OracleID(i),
				})
		}
		outcome, err := p.Outcome(ctx, ocr3types.OutcomeContext{SeqNr: 2, PreviousOutcome: encodedPreviousOutcome}, types.Query{}, aos)
		require.NoError(t, err)

		decoded, err := p.OutcomeCodec.Decode(outcome)
		require.NoError(t, err)

		assert.Equal(t, int64(102030415000000000), decoded.ObservationsTimestampNanoseconds)
		require.Len(t, decoded.ValidAfterSeconds, 2)
		assert.Equal(t, int64(102030409), int64(decoded.ValidAfterSeconds[1]))
		assert.Equal(t, int64(102030409), int64(decoded.ValidAfterSeconds[2]))
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

func Test_Outcome_Methods(t *testing.T) {
	t.Run("IsReportable", func(t *testing.T) {
		outcome := Outcome{}
		cid := llotypes.ChannelID(1)

		// Not reportable if retired
		outcome.LifeCycleStage = LifeCycleStageRetired
		assert.EqualError(t, outcome.IsReportable(cid), "ChannelID: 1; Reason: IsReportable=false; retired channel")

		// Timestamp overflow
		outcome.LifeCycleStage = LifeCycleStageProduction
		outcome.ObservationsTimestampNanoseconds = time.Unix(math.MaxInt64, 0).UnixNano()
		outcome.ChannelDefinitions = map[llotypes.ChannelID]llotypes.ChannelDefinition{}
		assert.EqualError(t, outcome.IsReportable(cid), "ChannelID: 1; Reason: IsReportable=false; invalid observations timestamp; Err: timestamp doesn't fit into uint32: -1")

		// No channel definition with ID
		outcome.LifeCycleStage = LifeCycleStageProduction
		outcome.ObservationsTimestampNanoseconds = time.Unix(1726670490, 0).UnixNano()
		outcome.ChannelDefinitions = map[llotypes.ChannelID]llotypes.ChannelDefinition{}
		assert.EqualError(t, outcome.IsReportable(cid), "ChannelID: 1; Reason: IsReportable=false; no channel definition with this ID")

		// No ValidAfterSeconds yet
		outcome.ChannelDefinitions[cid] = llotypes.ChannelDefinition{}
		assert.EqualError(t, outcome.IsReportable(cid), "ChannelID: 1; Reason: IsReportable=false; no validAfterSeconds entry yet, this must be a new channel")

		// ValidAfterSeconds is in the future
		outcome.ValidAfterSeconds = map[llotypes.ChannelID]uint32{cid: uint32(1726670491)}
		assert.EqualError(t, outcome.IsReportable(cid), "ChannelID: 1; Reason: IsReportable=false; not valid yet (observationsTimestampSeconds=1726670490 < validAfterSeconds=1726670491)")
	})
	t.Run("ReportableChannels", func(t *testing.T) {
		outcome := Outcome{
			ObservationsTimestampNanoseconds: time.Unix(1726670490, 0).UnixNano(),
			ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				1: {},
				2: {},
				3: {},
			},
			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				1: 1726670489,
				3: 1726670489,
			},
		}
		reportable, unreportable := outcome.ReportableChannels()
		assert.Equal(t, []llotypes.ChannelID{1, 3}, reportable)
		require.Len(t, unreportable, 1)
		assert.Equal(t, "ChannelID: 2; Reason: IsReportable=false; no validAfterSeconds entry yet, this must be a new channel", unreportable[0].Error())
	})
}
