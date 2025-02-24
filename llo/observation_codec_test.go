package llo

import (
	"encoding/base64"
	reflect "reflect"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

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
	t.Run("legacy compatibility", func(t *testing.T) {
		codec := protoObservationCodec{}
		t.Run("encode includes both unixTimestampNanosecondsLegacy and unixTimestampNanoseconds", func(t *testing.T) {
			encoded, err := codec.Encode(Observation{UnixTimestampNanoseconds: uint64(12345 * time.Second)})
			require.NoError(t, err)

			pbuf := &LLOObservationProto{}
			require.NoError(t, proto.Unmarshal(encoded, pbuf))

			assert.Equal(t, uint64(12345*time.Second), pbuf.UnixTimestampNanoseconds)
			assert.Equal(t, int64(12345*time.Second), pbuf.UnixTimestampNanosecondsLegacy)
		})
		t.Run("decode converts int64 unixTimestampNanosecondsLegacy into uint64 observationTimestampNanoseconds", func(t *testing.T) {
			// Hardcoded binary encoded in legacy format:
			// It has UnixTimestampNanoseconds=1234567890
			b, err := base64.StdEncoding.DecodeString(`CgMBAgMQARjShdjMBCICAQIqIQgDEh0IAhIECAMQARIECAQQAxoNeyJmb28iOiJiYXIifTIaCAkSFggBEhIKBAAAAAASBAAAAAAaBAAAAAAyDAgEEggSBgAAAAACezINCAUSCRIHAAAAAAIByDIjCAgSHwgBEhsKBwAAAAACA/ISBwAAAAACA/MaBwAAAAACA/Q=`)
			require.NoError(t, err)

			obs, err := codec.Decode(b)
			require.NoError(t, err)

			assert.Equal(t, uint64(1234567890), obs.UnixTimestampNanoseconds)
		})
		t.Run("decoding negative value fails", func(t *testing.T) {
			pbuf := &LLOObservationProto{
				UnixTimestampNanosecondsLegacy: -1,
			}
			b, err := proto.Marshal(pbuf)
			require.NoError(t, err)
			_, err = codec.Decode(b)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to decode observation; cannot accept negative unix timestamp: -1")
		})
	})
}

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
			"UnixTimestampNanoseconds":      gen.UInt64(),
			"RemoveChannelIDs":              genRemoveChannelIDs(),
			"UpdateChannelDefinitions":      genChannelDefinitions(),
			"StreamValues":                  genStreamValuesMap(),
		}),
	))

	properties.TestingRun(t)
}
