package llo

import (
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Test_protoObservationCodec_CoefficientBound covers the v3.0 observation path.
// The bound lives in shared code, so both plugin versions reject the same value,
// which is what keeps a mixed-version DON from disagreeing about more than one
// peer's observation.
func Test_protoObservationCodec_CoefficientBound(t *testing.T) {
	codec, err := NewProtoObservationCodec(logger.Nop(), true)
	require.NoError(t, err)

	atLimit := decimal.NewFromBigInt(new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits-1), -2)
	overSized := decimal.NewFromBigInt(new(big.Int).Lsh(big.NewInt(1), protocol.MaxDecimalCoefficientBits), -2)

	encoded := func(t *testing.T, d decimal.Decimal) []byte {
		t.Helper()
		b, err := codec.Encode(Observation{
			UnixTimestampNanoseconds: 1,
			StreamValues:             protocol.StreamValues{llotypes.StreamID(1): protocol.ToDecimal(d)},
		})
		require.NoError(t, err)
		return b
	}

	_, err = codec.Decode(encoded(t, atLimit))
	require.NoError(t, err)

	_, err = codec.Decode(encoded(t, overSized))
	require.ErrorIs(t, err, protocol.ErrDecimalCoefficientOutOfRange)
}
