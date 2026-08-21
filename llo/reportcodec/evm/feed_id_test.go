package evm

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

func Test_FeedID(t *testing.T) {
	feedID := common.HexToHash("0x0003111111111111111111111111111111111111111111111111111111111111")

	codecs := map[string]protocol.FeedIDer{
		"premium legacy":           ReportCodecPremiumLegacy{},
		"abi encode unpacked":      ReportCodecEVMABIEncodeUnpacked{},
		"abi encode unpacked expr": ReportCodecEVMABIEncodeUnpackedExpr{},
		"streamlined":              ReportCodecEVMStreamlined{},
	}
	for name, codec := range codecs {
		t.Run(name, func(t *testing.T) {
			cd := llotypes.ChannelDefinition{Opts: []byte(`{"feedID":"` + feedID.Hex() + `"}`)}
			got, ok, err := codec.FeedID(cd)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, [32]byte(feedID), got)

			_, _, err = codec.FeedID(llotypes.ChannelDefinition{Opts: []byte(`not json`)})
			require.ErrorContains(t, err, "invalid Opts")
		})
	}

	t.Run("streamlined reports no feed ID when unset", func(t *testing.T) {
		_, ok, err := ReportCodecEVMStreamlined{}.FeedID(llotypes.ChannelDefinition{Opts: []byte(`{}`)})
		require.NoError(t, err)
		require.False(t, ok)
	})
}
