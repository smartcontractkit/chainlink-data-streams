package protocol

import (
	"testing"

	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_RetirementReport_CheckCompatible(t *testing.T) {
	rr := func(v uint32) RetirementReport {
		return RetirementReport{ProtocolVersion: v, ValidAfterNanoseconds: map[llotypes.ChannelID]uint64{1: 500}}
	}

	t.Run("every supported version pair interoperates", func(t *testing.T) {
		for predecessor := uint32(0); predecessor <= MaxSupportedProtocolVersion; predecessor++ {
			for successor := uint32(0); successor <= MaxSupportedProtocolVersion; successor++ {
				require.NoError(t, rr(predecessor).CheckCompatible(successor),
					"predecessor v%d -> successor v%d", predecessor, successor)
			}
		}
	})

	t.Run("rejects an unsupported predecessor version", func(t *testing.T) {
		err := rr(MaxSupportedProtocolVersion + 1).CheckCompatible(MaxSupportedProtocolVersion)
		require.ErrorContains(t, err, "predecessor retirement report has unsupported protocol version")
	})

	t.Run("rejects an unsupported successor version", func(t *testing.T) {
		err := rr(0).CheckCompatible(MaxSupportedProtocolVersion + 1)
		require.ErrorContains(t, err, "this instance has unsupported protocol version")
	})
}
