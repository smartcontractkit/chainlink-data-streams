package llo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_StandardRetirementReportCodec(t *testing.T) {
	codec := StandardRetirementReportCodec{}

	t.Run("encodes/decodes with ValidAfterNanoseconds", func(t *testing.T) {
		rr := RetirementReport{
			ProtocolVersion: 1,
			ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{
				1: uint64(2 * time.Second),
				2: uint64(3 * time.Second),
				3: uint64(100 * time.Millisecond),
			},
		}

		encoded, err := codec.Encode(rr)
		require.NoError(t, err)

		assert.JSONEq(t, `{"ProtocolVersion":1,"ValidAfterNanoseconds":{"1":2000000000,"2":3000000000,"3":100000000}}`, string(encoded))

		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)

		require.Equal(t, rr, decoded)
	})
}
