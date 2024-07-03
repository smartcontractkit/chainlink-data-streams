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
)

func Test_Outcome(t *testing.T) {
	p := &Plugin{
		Config:           Config{true},
		OutcomeCodec:     protoOutcomeCodec{},
		Logger:           logger.Test(t),
		ObservationCodec: protoObservationCodec{},
	}

	t.Run("if number of observers < 2f+1, errors", func(t *testing.T) {
		_, err := p.Outcome(ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{})
		assert.EqualError(t, err, "invariant violation: expected at least 2f+1 attributed observations, got 0 (f: 0)")
		p.F = 1
		_, err = p.Outcome(ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{{}, {}})
		assert.EqualError(t, err, "invariant violation: expected at least 2f+1 attributed observations, got 2 (f: 1)")
	})

	t.Run("if seqnr == 1, and has enough observers, emits initial outcome with 'production' LifeCycleStage", func(t *testing.T) {
		outcome, err := p.Outcome(ocr3types.OutcomeContext{SeqNr: 1}, types.Query{}, []types.AttributedObservation{
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
			outcome, err := p.Outcome(ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, aos)
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

			outcome, err := p.Outcome(ocr3types.OutcomeContext{PreviousOutcome: previousOutcome, SeqNr: 2}, types.Query{}, aos)
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
			outcome, err := p.Outcome(ocr3types.OutcomeContext{SeqNr: 2}, types.Query{}, aos)
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
			outcome, err := p.Outcome(outctx, types.Query{}, aos)
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
