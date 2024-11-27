package llo

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"
	ocr2types "github.com/smartcontractkit/libocr/offchainreporting2/types"

	"github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
	"github.com/smartcontractkit/chainlink-common/pkg/utils/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_JSONCodec(t *testing.T) {
	t.Run("Encode=>Decode", func(t *testing.T) {
		ctx := tests.Context(t)
		r := Report{
			ConfigDigest:                types.ConfigDigest([32]byte{1, 2, 3}),
			SeqNr:                       43,
			ChannelID:                   llotypes.ChannelID(46),
			ValidAfterSeconds:           44,
			ObservationTimestampSeconds: 45,
			Values:                      []StreamValue{ToDecimal(decimal.NewFromInt(1)), ToDecimal(decimal.NewFromInt(2)), &Quote{Bid: decimal.NewFromFloat(3.13), Benchmark: decimal.NewFromFloat(4.4), Ask: decimal.NewFromFloat(5.12)}},
			Specimen:                    true,
		}

		cdc := JSONReportCodec{}

		encoded, err := cdc.Encode(ctx, r, llo.ChannelDefinition{})
		require.NoError(t, err)

		fmt.Println("encoded", string(encoded))
		assert.Equal(t, `{"ConfigDigest":"0102030000000000000000000000000000000000000000000000000000000000","SeqNr":43,"ChannelID":46,"ValidAfterSeconds":44,"ObservationTimestampSeconds":45,"Values":[{"Type":0,"Value":"1"},{"Type":0,"Value":"2"},{"Type":1,"Value":"Q{Bid: 3.13, Benchmark: 4.4, Ask: 5.12}"}],"Specimen":true}`, string(encoded))

		decoded, err := cdc.Decode(encoded)
		require.NoError(t, err)

		assert.Equal(t, r, decoded)
	})
	t.Run("Pack=>Unpack", func(t *testing.T) {
		t.Run("report is not valid JSON", func(t *testing.T) {
			digest := types.ConfigDigest([32]byte{1, 2, 3})
			seqNr := uint64(43)
			report := ocr2types.Report(`foobar`)
			sigs := []types.AttributedOnchainSignature{{Signature: []byte{2, 3, 4}, Signer: 2}}

			cdc := JSONReportCodec{}

			_, err := cdc.Pack(digest, seqNr, report, sigs)
			require.EqualError(t, err, "json: error calling MarshalJSON for type json.RawMessage: invalid character 'o' in literal false (expecting 'a')")
		})
		t.Run("report is valid JSON", func(t *testing.T) {
			digest := types.ConfigDigest([32]byte{1, 2, 3})
			seqNr := uint64(43)
			report := ocr2types.Report(`{"foo":"bar"}`)
			sigs := []types.AttributedOnchainSignature{{Signature: []byte{2, 3, 4}, Signer: 2}}

			cdc := JSONReportCodec{}

			packed, err := cdc.Pack(digest, seqNr, report, sigs)
			require.NoError(t, err)
			assert.Equal(t, `{"configDigest":"0102030000000000000000000000000000000000000000000000000000000000","seqNr":43,"report":{"foo":"bar"},"sigs":[{"Signature":"AgME","Signer":2}]}`, string(packed))

			digest2, seqNr2, report2, sigs2, err := cdc.Unpack(packed)
			require.NoError(t, err)
			assert.Equal(t, digest, digest2)
			assert.Equal(t, seqNr, seqNr2)
			assert.Equal(t, report, report2)
			assert.Equal(t, sigs, sigs2)
		})
	})
	t.Run("UnpackDecode unpacks and decodes report", func(t *testing.T) {
		b := []byte(`{"configDigest":"0102030000000000000000000000000000000000000000000000000000000000","seqNr":43,"report":{"ConfigDigest":"0102030000000000000000000000000000000000000000000000000000000000","SeqNr":43,"ChannelID":46,"ValidAfterSeconds":44,"ObservationTimestampSeconds":45,"Values":[{"Type":0,"Value":"1"},{"Type":0,"Value":"2"},{"Type":1,"Value":"Q{Bid: 3.13, Benchmark: 4.4, Ask: 5.12}"}],"Specimen":true},"sigs":[{"Signature":"AgME","Signer":2}]}`)

		cdc := JSONReportCodec{}
		digest, seqNr, report, sigs, err := cdc.UnpackDecode(b)
		require.NoError(t, err)

		assert.Equal(t, types.ConfigDigest([32]byte{1, 2, 3}), digest)
		assert.Equal(t, uint64(43), seqNr)
		assert.Equal(t, Report{
			ConfigDigest:                types.ConfigDigest([32]byte{1, 2, 3}),
			SeqNr:                       43,
			ChannelID:                   llotypes.ChannelID(46),
			ValidAfterSeconds:           44,
			ObservationTimestampSeconds: 45,
			Values:                      []StreamValue{ToDecimal(decimal.NewFromInt(1)), ToDecimal(decimal.NewFromInt(2)), &Quote{Bid: decimal.NewFromFloat(3.13), Benchmark: decimal.NewFromFloat(4.4), Ask: decimal.NewFromFloat(5.12)}},
			Specimen:                    true,
		}, report)
		assert.Equal(t, []types.AttributedOnchainSignature{{Signature: []byte{2, 3, 4}, Signer: 2}}, sigs)
	})
	t.Run("invalid input fails decode", func(t *testing.T) {
		cdc := JSONReportCodec{}
		_, err := cdc.Decode([]byte(`{}`))
		assert.EqualError(t, err, "invalid ConfigDigest; cannot convert bytes to ConfigDigest. bytes have wrong length 0")
		_, err = cdc.Decode([]byte(`{"ConfigDigest":"0102030000000000000000000000000000000000000000000000000000000000"}`))
		assert.EqualError(t, err, "missing SeqNr")
	})
}
