package llo

import (
	reflect "reflect"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_protoOutcomeCodecV1(t *testing.T) {
	t.Run("encode and decode empty struct", func(t *testing.T) {
		outcome := Outcome{}
		outcomeBytes, err := (protoOutcomeCodecV1{}).Encode(outcome)
		require.NoError(t, err)

		outcome2, err := (protoOutcomeCodecV1{}).Decode(outcomeBytes)
		require.NoError(t, err)

		assert.Equal(t, outcome, outcome2)
	})
	t.Run("encode and decode with values", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                  llotypes.LifeCycleStage("staging"),
			ObservationTimestampNanoseconds: 1234567890,
			ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				3: {
					ReportFormat: llotypes.ReportFormatJSON,
					Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorQuote}},
					Opts:         []byte(`{"foo":"bar"}`),
				},
			},

			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
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

		outcomeBytes, err := (protoOutcomeCodecV1{}).Encode(outcome)
		require.NoError(t, err)

		outcome2, err := (protoOutcomeCodecV1{}).Decode(outcomeBytes)
		require.NoError(t, err)

		expectedOutcome := outcome
		delete(expectedOutcome.StreamAggregates, 3) // nils will be dropped

		assert.Equal(t, outcome, outcome2)
	})
	t.Run("encode and decode with compression enabled", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                  llotypes.LifeCycleStage("staging"),
			ObservationTimestampNanoseconds: 1234567890,
			ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				3: {
					ReportFormat: llotypes.ReportFormatJSON,
					Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorQuote}},
					Opts:         []byte(`{"foo":"bar"}`),
				},
			},
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				3: 123,
			},
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(123)),
				},
				2: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(456)),
				},
				4: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorQuote: &Quote{
						Bid:       decimal.NewFromInt(1010),
						Benchmark: decimal.NewFromInt(1011),
						Ask:       decimal.NewFromInt(1012),
					},
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(457)),
				},
			},
		}

		codec, err := NewProtoOutcomeCodecV1(logger.Nop(), true)
		require.NoError(t, err)

		outcomeBytes, err := codec.Encode(outcome)
		require.NoError(t, err)

		outcome2, err := codec.Decode(outcomeBytes)
		require.NoError(t, err)

		expectedOutcome := outcome
		assert.Equal(t, expectedOutcome, outcome2)
	})
	t.Run("compressed and uncompressed data are different", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                  llotypes.LifeCycleStage("staging"),
			ObservationTimestampNanoseconds: 1234567890,
			ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
				3: {
					ReportFormat: llotypes.ReportFormatJSON,
					Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorQuote}},
					Opts:         []byte(`{"foo":"bar"}`),
				},
			},
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				3: 123,
			},
			StreamAggregates: map[llotypes.StreamID]map[llotypes.Aggregator]StreamValue{
				1: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(123)),
				},
				2: map[llotypes.Aggregator]StreamValue{
					llotypes.AggregatorMedian: ToDecimal(decimal.NewFromInt(456)),
				},
			},
		}

		uncompressedCodec := protoOutcomeCodecV1{}
		compressedCodec, err := NewProtoOutcomeCodecV1(logger.Nop(), true)
		require.NoError(t, err)

		uncompressedBytes, err := uncompressedCodec.Encode(outcome)
		require.NoError(t, err)

		compressedBytes, err := compressedCodec.Encode(outcome)
		require.NoError(t, err)

		// Compressed data should be different from uncompressed data
		assert.NotEqual(t, uncompressedBytes, compressedBytes)

		// Both should decode to the same outcome
		outcome1, err := uncompressedCodec.Decode(uncompressedBytes)
		require.NoError(t, err)

		outcome2, err := compressedCodec.Decode(compressedBytes)
		require.NoError(t, err)

		expectedOutcome := outcome
		assert.Equal(t, expectedOutcome, outcome1)
		assert.Equal(t, expectedOutcome, outcome2)
	})
	t.Run("compressed codec can decode uncompressed data", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                  llotypes.LifeCycleStage("staging"),
			ObservationTimestampNanoseconds: 1234567890,
		}

		uncompressedCodec := protoOutcomeCodecV1{}
		compressedCodec, err := NewProtoOutcomeCodecV1(logger.Nop(), true)
		require.NoError(t, err)

		uncompressedBytes, err := uncompressedCodec.Encode(outcome)
		require.NoError(t, err)

		// Compressed codec should decode uncompressed data
		outcome2, err := compressedCodec.Decode(uncompressedBytes)
		require.NoError(t, err)

		assert.Equal(t, outcome, outcome2)
	})
	t.Run("uncompressed codec cannot decode compressed data", func(t *testing.T) {
		outcome := Outcome{
			LifeCycleStage:                  llotypes.LifeCycleStage("staging"),
			ObservationTimestampNanoseconds: 1234567890,
		}

		uncompressedCodec := protoOutcomeCodecV1{}
		compressedCodec, err := NewProtoOutcomeCodecV1(logger.Nop(), true)
		require.NoError(t, err)

		compressedBytes, err := compressedCodec.Encode(outcome)
		require.NoError(t, err)

		// Uncompressed codec should fail to decode compressed data
		_, err = uncompressedCodec.Decode(compressedBytes)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode outcome")
	})
}

func Fuzz_protoOutcomeCodecV1_Decode(f *testing.F) {
	f.Add([]byte("not a protobuf"))
	f.Add([]byte{0x0a, 0x00})             // empty protobuf
	f.Add([]byte{0x0a, 0x02, 0x08, 0x01}) // invalid protobuf
	f.Add(([]byte)(nil))
	f.Add([]byte{})

	outcome := Outcome{}
	emptyPbuf, err := (protoOutcomeCodecV1{}).Encode(outcome)
	require.NoError(f, err)
	f.Add([]byte(emptyPbuf))

	outcome = Outcome{
		LifeCycleStage:                  llotypes.LifeCycleStage("staging"),
		ObservationTimestampNanoseconds: 1234567890,
		ChannelDefinitions: map[llotypes.ChannelID]llotypes.ChannelDefinition{
			3: {
				ReportFormat: llotypes.ReportFormatJSON,
				Streams:      []llotypes.Stream{{StreamID: 1, Aggregator: llotypes.AggregatorMedian}, {StreamID: 2, Aggregator: llotypes.AggregatorQuote}},
				Opts:         []byte(`{"foo":"bar"}`),
			},
		},

		ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
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

	valuesPbuf, err := (protoOutcomeCodecV1{}).Encode(outcome)
	require.NoError(f, err)
	f.Add([]byte(valuesPbuf))

	// Add compressed outcomes
	compressedCodec, err := NewProtoOutcomeCodecV1(logger.Nop(), true)
	require.NoError(f, err)

	emptyCompressed, err := compressedCodec.Encode(Outcome{})
	require.NoError(f, err)
	f.Add([]byte(emptyCompressed))

	valuesCompressed, err := compressedCodec.Encode(outcome)
	require.NoError(f, err)
	f.Add([]byte(valuesCompressed))

	var codec OutcomeCodec = protoOutcomeCodecV1{}
	var compressedCodecForFuzz OutcomeCodec = compressedCodec

	f.Fuzz(func(t *testing.T, data []byte) {
		// test that it doesn't panic, don't care about errors
		codec.Decode(data)                  //nolint:errcheck // test
		compressedCodecForFuzz.Decode(data) //nolint:errcheck // test
	})
}

func Test_protoOutcomeCodecV1_Properties(t *testing.T) {
	properties := gopter.NewProperties(nil)

	codec := protoOutcomeCodecV1{}

	properties.Property("Encode/Decode", prop.ForAll(
		func(outcome Outcome) bool {
			b, err := codec.Encode(outcome)
			require.NoError(t, err)
			outcome2, err := codec.Decode(b)
			require.NoError(t, err)

			return equalOutcomes(t, outcome, outcome2)
		},
		gen.StrictStruct(reflect.TypeOf(&Outcome{}), map[string]gopter.Gen{
			"LifeCycleStage":                  genLifecycleStage(),
			"ObservationTimestampNanoseconds": gen.UInt64(),
			"ChannelDefinitions":              genChannelDefinitions(),
			"ValidAfterNanoseconds":           gen.MapOf(gen.UInt32(), gen.UInt64()),
			"StreamAggregates":                genStreamAggregates(),
		}),
	))

	properties.TestingRun(t)
}

func Test_protoOutcomeCodecV1_Properties_Compressed(t *testing.T) {
	properties := gopter.NewProperties(nil)

	codec, err := NewProtoOutcomeCodecV1(logger.Nop(), true)
	require.NoError(t, err)

	properties.Property("Encode/Decode with compression", prop.ForAll(
		func(outcome Outcome) bool {
			b, err := codec.Encode(outcome)
			require.NoError(t, err)
			outcome2, err := codec.Decode(b)
			require.NoError(t, err)

			return equalOutcomes(t, outcome, outcome2)
		},
		gen.StrictStruct(reflect.TypeOf(&Outcome{}), map[string]gopter.Gen{
			"LifeCycleStage":                  genLifecycleStage(),
			"ObservationTimestampNanoseconds": gen.UInt64(),
			"ChannelDefinitions":              genChannelDefinitions(),
			"ValidAfterNanoseconds":           gen.MapOf(gen.UInt32(), gen.UInt64()),
			"StreamAggregates":                genStreamAggregates(),
		}),
	))

	properties.TestingRun(t)
}
