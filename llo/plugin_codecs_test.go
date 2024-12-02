package llo

import (
	"bytes"
	reflect "reflect"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Fuzz_protoObservationCodec_Decode(f *testing.F) {
	f.Add([]byte("not a protobuf"))
	f.Add([]byte{0x0a, 0x00})             // empty protobuf
	f.Add([]byte{0x0a, 0x02, 0x08, 0x01}) // invalid protobuf
	f.Add(([]byte)(nil))
	f.Add([]byte{})

	obs := Observation{}
	emptyPbuf, err := (protoObservationCodec{}).Encode(obs)
	require.NoError(f, err)
	f.Add([]byte(emptyPbuf))

	obs = Observation{
		AttestedPredecessorRetirement: []byte{1, 2, 3},
		ShouldRetire:                  true,
		UnixTimestampNanoseconds:      1234567890,
		RemoveChannelIDs: map[llotypes.ChannelID]struct{}{
			1: {},
			2: {},
		},
		UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
			3: {
				ReportFormat: llotypes.ReportFormatJSON,
				Streams:      []llotypes.Stream{{StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorQuote}},
				Opts:         []byte(`{"foo":"bar"}`),
			},
		},
		StreamValues: map[llotypes.StreamID]StreamValue{
			4: ToDecimal(decimal.NewFromInt(123)),
			5: ToDecimal(decimal.NewFromInt(456)),
			6: (*Decimal)(nil),
			7: nil,
			8: &Quote{
				Bid:       decimal.NewFromInt(1010),
				Benchmark: decimal.NewFromInt(1011),
				Ask:       decimal.NewFromInt(1012),
			},
			9:  &Quote{},
			10: (*Quote)(nil),
		},
	}

	valuesPbuf, err := (protoObservationCodec{}).Encode(obs)
	require.NoError(f, err)
	f.Add([]byte(valuesPbuf))

	var codec ObservationCodec = protoObservationCodec{}
	f.Fuzz(func(t *testing.T, data []byte) {
		// test that it doesn't panic, don't care about errors
		codec.Decode(data) //nolint:errcheck
	})
}

func Fuzz_protoOutcomeCodec_Decode(f *testing.F) {
	f.Add([]byte("not a protobuf"))
	f.Add([]byte{0x0a, 0x00})             // empty protobuf
	f.Add([]byte{0x0a, 0x02, 0x08, 0x01}) // invalid protobuf
	f.Add(([]byte)(nil))
	f.Add([]byte{})

	outcome := Outcome{}
	emptyPbuf, err := (protoOutcomeCodec{}).Encode(outcome)
	require.NoError(f, err)
	f.Add([]byte(emptyPbuf))

	outcome = Outcome{
		LifeCycleStage:                   llotypes.LifeCycleStage("staging"),
		ObservationsTimestampNanoseconds: 1234567890,
		ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
			3: {
				ReportFormat: llotypes.ReportFormatJSON,
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorQuote}},
				Opts:         []byte(`{"foo":"bar"}`),
			},
		},

		ValidAfterSeconds: map[llotypes.ChannelID]uint32{
			3: 123,
		},
		StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
			1: map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(123)),
			},
			2: map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(456)),
			},
			3: map[llotypes.Aggregator]StreamValue{},
			4: map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorQuote: &Quote{
					Bid:       decimal.NewFromInt(1010),
					Benchmark: decimal.NewFromInt(1011),
					Ask:       decimal.NewFromInt(1012),
				},
				llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(457)),
			},
			5: map[llotypes.Aggregator]StreamValue{
				llotypes.AggregatorQuote: &Quote{
					Bid:       decimal.NewFromInt(1013),
					Benchmark: decimal.NewFromInt(1014),
					Ask:       decimal.NewFromInt(1015),
				},
			},
		},
	}

	valuesPbuf, err := (protoOutcomeCodec{}).Encode(outcome)
	require.NoError(f, err)
	f.Add([]byte(valuesPbuf))

	var codec OutcomeCodec = protoOutcomeCodec{}

	f.Fuzz(func(t *testing.T, data []byte) {
		// test that it doesn't panic, don't care about errors
		codec.Decode(data) //nolint:errcheck
	})
}

func Test_protoObservationCodec_Properties(t *testing.T) {
	properties := gopter.NewProperties(nil)

	codec := protoObservationCodec{}

	properties.Property("Encode/Decode", prop.ForAll(
		func(obs Observation) bool {
			b, err := codec.Encode(obs)
			require.NoError(t, err)
			obs2, err := codec.Decode(b)
			require.NoError(t, err)

			return equalObservations(obs, obs2)
		},
		gen.StrictStruct(reflect.TypeOf(&Observation{}), map[string]gopter.Gen{
			"AttestedPredecessorRetirement": genAttestedPredecessorRetirement(),
			"ShouldRetire":                  gen.Bool(),
			"UnixTimestampNanoseconds":      gen.Int64(),
			"RemoveChannelIDs":              genRemoveChannelIDs(),
			"UpdateChannelDefinitions":      genChannelDefinitions(),
			"StreamValues":                  genStreamValuesMap(),
		}),
	))

	properties.TestingRun(t)
}

func Test_protoOutcomeCodec_Properties(t *testing.T) {
	properties := gopter.NewProperties(nil)

	codec := protoOutcomeCodec{}

	properties.Property("Encode/Decode", prop.ForAll(
		func(outcome Outcome) bool {
			b, err := codec.Encode(outcome)
			require.NoError(t, err)
			outcome2, err := codec.Decode(b)
			require.NoError(t, err)

			return equalOutcomes(outcome, outcome2)
		},
		gen.StrictStruct(reflect.TypeOf(&Outcome{}), map[string]gopter.Gen{
			"LifeCycleStage":                   genLifecycleStage(),
			"ObservationsTimestampNanoseconds": gen.Int64(),
			"ChannelDefinitions":               genChannelDefinitions(),
			"ValidAfterSeconds":                gen.MapOf(gen.UInt32(), gen.UInt32()),
			"StreamAggregates":                 genStreamAggregates(),
		}),
	))

	properties.TestingRun(t)
}

func genLifecycleStage() gopter.Gen {
	return gen.AnyString().Map(func(s string) llotypes.LifeCycleStage {
		return llotypes.LifeCycleStage(s)
	})
}

func genAttestedPredecessorRetirement() gopter.Gen {
	return gen.SliceOf(gen.UInt8())
}

func genRemoveChannelIDs() gopter.Gen {
	return gen.MapOf(gen.UInt32(), gen.Const(struct{}{}))
}

func genChannelDefinitions() gopter.Gen {
	return gen.MapOf(gen.UInt32(), genChannelDefinition())
}

func genStreamAggregates() gopter.Gen {
	return gen.MapOf(gen.UInt32(), genMapOfAggregatorStreamValue()).Map(func(m map[uint32]map[uint32]StreamValue) map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue {
		m2 := make(map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue)
		for k, v := range m {
			m3 := make(map[llotypes.Aggregator]StreamValue)
			for k2, v2 := range v {
				m3[llotypes.Aggregator(k2)] = v2
			}
			m2[k] = m3
		}
		return m2
	})
}

func genMapOfAggregatorStreamValue() gopter.Gen {
	return genStreamValuesMap()
}

func genStreamValuesMap() gopter.Gen {
	return genStreamValues().Map(func(values []StreamValue) map[llotypes.StreamID]StreamValue {
		m := make(map[llotypes.StreamID]StreamValue)
		for i, v := range values {
			m[llotypes.StreamID(i)] = v
		}
		return m
	})
}

func genChannelDefinition() gopter.Gen {
	return gen.StrictStruct(reflect.TypeOf(llotypes.ChannelDefinition{}), map[string]gopter.Gen{
		"ReportFormat": genReportFormat(),
		"Streams":      gen.SliceOf(genStream()),
		"Opts":         gen.SliceOf(gen.UInt8()),
	})
}

func genReportFormat() gopter.Gen {
	return gen.UInt32().Map(func(i uint32) llotypes.ReportFormat {
		return llotypes.ReportFormat(i)
	})
}

func genStream() gopter.Gen {
	return gen.StrictStruct(reflect.TypeOf(llotypes.Stream{}), map[string]gopter.Gen{
		"StreamID":   gen.UInt32(),
		"Aggregator": genAggregator(),
	})
}

func genAggregator() gopter.Gen {
	return gen.UInt32().Map(func(i uint32) llotypes.Aggregator {
		return llotypes.Aggregator(i)
	})
}

func equalObservations(obs, obs2 Observation) bool {
	if !bytes.Equal(obs.AttestedPredecessorRetirement, obs2.AttestedPredecessorRetirement) {
		return false
	}
	if obs.ShouldRetire != obs2.ShouldRetire {
		return false
	}
	if obs.UnixTimestampNanoseconds != obs2.UnixTimestampNanoseconds {
		return false
	}
	if len(obs.RemoveChannelIDs) != len(obs2.RemoveChannelIDs) {
		return false
	}
	for k := range obs.RemoveChannelIDs {
		if _, ok := obs2.RemoveChannelIDs[k]; !ok {
			return false
		}
	}

	if len(obs.UpdateChannelDefinitions) != len(obs2.UpdateChannelDefinitions) {
		return false
	}
	for k, v := range obs.UpdateChannelDefinitions {
		v2, ok := obs2.UpdateChannelDefinitions[k]
		if !ok {
			return false
		}
		if v.ReportFormat != v2.ReportFormat {
			return false
		}
		if !reflect.DeepEqual(v.Streams, v2.Streams) {
			return false
		}
		if !bytes.Equal(v.Opts, v2.Opts) {
			return false
		}
	}

	if len(obs.StreamValues) != len(obs2.StreamValues) {
		return false
	}
	for k, v := range obs.StreamValues {
		v2, ok := obs2.StreamValues[k]
		if !ok {
			return false
		}
		if !equalStreamValues(v, v2) {
			return false
		}
	}
	return true
}

func equalOutcomes(outcome, outcome2 Outcome) bool {
	if outcome.LifeCycleStage != outcome2.LifeCycleStage {
		return false
	}
	if outcome.ObservationsTimestampNanoseconds != outcome2.ObservationsTimestampNanoseconds {
		return false
	}
	if len(outcome.ChannelDefinitions) != len(outcome2.ChannelDefinitions) {
		return false
	}
	for k, v := range outcome.ChannelDefinitions {
		v2, ok := outcome2.ChannelDefinitions[k]
		if !ok {
			return false
		}
		if v.ReportFormat != v2.ReportFormat {
			return false
		}
		if !reflect.DeepEqual(v.Streams, v2.Streams) {
			return false
		}
		if !bytes.Equal(v.Opts, v2.Opts) {
			return false
		}
	}

	if len(outcome.ValidAfterSeconds) != len(outcome2.ValidAfterSeconds) {
		return false
	}
	for k, v := range outcome.ValidAfterSeconds {
		v2, ok := outcome2.ValidAfterSeconds[k]
		if !ok {
			return false
		}
		if v != v2 {
			return false
		}
	}

	// filter out nils
	saggs1 := maps.Clone(outcome.StreamAggregates)
	saggs2 := maps.Clone(outcome2.StreamAggregates)
	for k, v := range saggs1 {
		if len(v) == 0 {
			delete(saggs1, k)
		}
	}
	for k, v := range saggs2 {
		if len(v) == 0 {
			delete(saggs2, k)
		}
	}

	if len(saggs1) != len(saggs2) {
		return false
	}
	for k, v := range saggs1 {
		v2, ok := saggs2[k]
		if !ok {
			return false
		}
		if !equalStreamAggregates(v, v2) {
			return false
		}
	}
	return true
}

func equalStreamAggregates(m1, m2 map[llotypes.Aggregator]StreamValue) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v := range m1 {
		v2, ok := m2[k]
		if !ok {
			return false
		}
		if !equalStreamValues(v, v2) {
			return false
		}
	}
	return true
}

func Test_protoObservationCodec(t *testing.T) {
	t.Run("encode and decode empty struct", func(t *testing.T) {
		obs := Observation{}
		obsBytes, err := (protoObservationCodec{}).Encode(obs)
		require.NoError(t, err)

		obs2, err := (protoObservationCodec{}).Decode(obsBytes)
		require.NoError(t, err)

		assert.Equal(t, obs, obs2)
	})
	t.Run("encode and decode with values", func(t *testing.T) {
		obs := Observation{
			AttestedPredecessorRetirement: []byte{1, 2, 3},
			ShouldRetire:                  true,
			UnixTimestampNanoseconds:      1234567890,
			RemoveChannelIDs: map[llotypes.ChannelID]struct{}{
				1: {},
				2: {},
			},
			UpdateChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				3: {
					ReportFormat: llotypes.ReportFormatJSON,
					Streams:      []llotypes.Stream{{StreamID: 3, Aggregator: llotypes.AggregatorMedian}, {StreamID: 4, Aggregator: llotypes.AggregatorQuote}},
					Opts:         []byte(`{"foo":"bar"}`),
				},
			},
			StreamValues: map[llotypes.StreamID]StreamValue{
				4: ToDecimal(decimal.NewFromInt(123)),
				5: ToDecimal(decimal.NewFromInt(456)),
				6: (*Decimal)(nil),
				7: nil,
			},
		}

		obsBytes, err := (protoObservationCodec{}).Encode(obs)
		require.NoError(t, err)

		obs2, err := (protoObservationCodec{}).Decode(obsBytes)
		require.NoError(t, err)

		expectedObs := obs
		delete(expectedObs.StreamValues, 6) // nils will be dropped
		delete(expectedObs.StreamValues, 7) // nils will be dropped

		assert.Equal(t, expectedObs, obs2)
	})
	t.Run("decoding with invalid data", func(t *testing.T) {
		t.Run("not a protobuf", func(t *testing.T) {
			_, err := (protoObservationCodec{}).Decode([]byte("not a protobuf"))
			require.Error(t, err)

			assert.Contains(t, err.Error(), "cannot parse invalid wire-format data")
		})
		t.Run("duplicate RemoveChannelIDs", func(t *testing.T) {
			pbuf := &LLOObservationProto{
				RemoveChannelIDs: []uint32{1, 1},
			}

			obsBytes, err := proto.Marshal(pbuf)
			require.NoError(t, err)

			_, err = (protoObservationCodec{}).Decode(obsBytes)
			require.EqualError(t, err, "failed to decode observation; duplicate channel ID in RemoveChannelIDs: 1")
		})
		t.Run("invalid LLOStreamValue", func(t *testing.T) {
			t.Run("nil/missing value", func(t *testing.T) {
				pbuf := &LLOObservationProto{
					StreamValues: map[uint32]*LLOStreamValue{
						1: &LLOStreamValue{Type: LLOStreamValue_Decimal, Value: nil},
					},
				}

				obsBytes, err := proto.Marshal(pbuf)
				require.NoError(t, err)

				_, err = (protoObservationCodec{}).Decode(obsBytes)
				require.EqualError(t, err, "failed to decode observation; invalid stream value for stream ID: 1; error decoding binary []: expected at least 4 bytes, got 0")
			})
			t.Run("unsupported type", func(t *testing.T) {
				pbuf := &LLOObservationProto{
					StreamValues: map[uint32]*LLOStreamValue{
						1: &LLOStreamValue{Type: 1000001, Value: []byte("foo")},
					},
				}

				obsBytes, err := proto.Marshal(pbuf)
				require.NoError(t, err)

				_, err = (protoObservationCodec{}).Decode(obsBytes)
				require.EqualError(t, err, "failed to decode observation; invalid stream value for stream ID: 1; cannot unmarshal protobuf stream value; unknown StreamValueType 1000001")
			})
		})
	})
}

func Test_protoOutcomeCodec(t *testing.T) {
	t.Run("encode and decode empty struct", func(t *testing.T) {
		outcome := Outcome{}
		outcomeBytes, err := (protoOutcomeCodec{}).Encode(outcome)
		require.NoError(t, err)

		outcome2, err := (protoOutcomeCodec{}).Decode(outcomeBytes)
		require.NoError(t, err)

		assert.Equal(t, outcome, outcome2)
	})
	t.Run("encode and decode with values", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                   llotypes.LifeCycleStage("staging"),
			ObservationsTimestampNanoseconds: 1234567890,
			ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				3: {
					ReportFormat: llotypes.ReportFormatJSON,
					Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorQuote}},
					Opts:         []byte(`{"foo":"bar"}`),
				},
			},

			ValidAfterSeconds: map[llotypes.ChannelID]uint32{
				3: 123,
			},
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(123)),
				},
				2: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(456)),
				},
				3: map[llotypes.Aggregator]StreamValue{},
				4: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorQuote: &Quote{
						Bid:       decimal.NewFromInt(1010),
						Benchmark: decimal.NewFromInt(1011),
						Ask:       decimal.NewFromInt(1012),
					},
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(457)),
				},
				5: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorQuote: &Quote{
						Bid:       decimal.NewFromInt(1013),
						Benchmark: decimal.NewFromInt(1014),
						Ask:       decimal.NewFromInt(1015),
					},
				},
			},
		}

		outcomeBytes, err := (protoOutcomeCodec{}).Encode(outcome)
		require.NoError(t, err)

		outcome2, err := (protoOutcomeCodec{}).Decode(outcomeBytes)
		require.NoError(t, err)

		expectedOutcome := outcome
		delete(expectedOutcome.StreamAggregates, 3) // nils will be dropped

		assert.Equal(t, outcome, outcome2)
	})
}
