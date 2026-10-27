package protocol

import (
	"fmt"
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

		assert.Equal(t, `{"ProtocolVersion":1,"ValidAfterNanoseconds":{"1":2000000000,"2":3000000000,"3":100000000}}`, string(encoded)) //nolint:testifylint // need to verify exact match including order for determinism

		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)

		require.Equal(t, rr, decoded)
	})

	reportWithChannels := func(n int) []byte {
		rr := RetirementReport{ProtocolVersion: 1, ValidAfterNanoseconds: make(map[llotypes.ChannelID]uint64, n)}
		for i := range n {
			rr.ValidAfterNanoseconds[llotypes.ChannelID(i)] = uint64(i)
		}
		encoded, err := codec.Encode(rr)
		require.NoError(t, err)
		return encoded
	}

	t.Run("decodes a report at MaxRetirementReportChannels", func(t *testing.T) {
		decoded, err := codec.Decode(reportWithChannels(MaxRetirementReportChannels))
		require.NoError(t, err)
		assert.Len(t, decoded.ValidAfterNanoseconds, MaxRetirementReportChannels)
	})

	t.Run("rejects a report over MaxRetirementReportChannels", func(t *testing.T) {
		_, err := codec.Decode(reportWithChannels(MaxRetirementReportChannels + 1))
		require.ErrorContains(t, err, fmt.Sprintf("retirement report has %d channels, max %d", MaxRetirementReportChannels+1, MaxRetirementReportChannels))
	})
}
