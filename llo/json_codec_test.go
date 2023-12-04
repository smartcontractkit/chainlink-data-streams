package llo

import (
	"math/big"
	"testing"

	commontypes "github.com/smartcontractkit/chainlink-common/pkg/types"
	"github.com/smartcontractkit/libocr/offchainreporting2/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_JSONCodec(t *testing.T) {
	t.Run("Encode=>Decode", func(t *testing.T) {
		r := Report{
			ConfigDigest:      types.ConfigDigest([32]byte{1, 2, 3}),
			ChainSelector:     42,
			SeqNr:             43,
			ChannelID:         commontypes.ChannelID(46),
			ValidAfterSeconds: 44,
			ValidUntilSeconds: 45,
			Values:            []*big.Int{big.NewInt(1), big.NewInt(2)},
			Specimen:          true,
		}

		cdc := JSONReportCodec{}

		encoded, err := cdc.Encode(r)
		require.NoError(t, err)

		assert.Equal(t, `{"ConfigDigest":"0102030000000000000000000000000000000000000000000000000000000000","ChainSelector":42,"SeqNr":43,"ChannelID":46,"ValidAfterSeconds":44,"ValidUntilSeconds":45,"Values":[1,2],"Specimen":true}`, string(encoded))

		decoded, err := cdc.Decode(encoded)
		require.NoError(t, err)

		assert.Equal(t, r, decoded)
	})
}
