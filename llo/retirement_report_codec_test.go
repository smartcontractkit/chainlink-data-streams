package llo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_StandardRetirementReportCodec(t *testing.T) {
	rr := RetirementReport{
		ValidAfterSeconds: map[llotypes.ChannelID]uint32{
			1: 2,
			2: 3,
		},
	}

	codec := StandardRetirementReportCodec{}

	encoded, err := codec.Encode(rr)
	require.NoError(t, err)

	assert.Equal(t, `{"ValidAfterSeconds":{"1":2,"2":3}}`, string(encoded))

	decoded, err := codec.Decode(encoded)
	require.NoError(t, err)

	require.Equal(t, rr, decoded)
}
