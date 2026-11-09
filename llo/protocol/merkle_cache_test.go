package protocol

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	llotypes "github.com/smartcontractkit/chainlink-common/pkg/types/llo"
)

func Test_merkleBundleCache(t *testing.T) {
	t.Parallel()
	c := newMerkleBundleCache(2)
	bundles := make([][]byte, 3)
	for i := range bundles {
		b, err := EncodeMerkleBundle(&LLOMerkleBundleProto{
			Family: uint32(llotypes.ReportFormatEVMMerkle),
			Leaves: []*LLOMerkleLeafProto{{ChannelID: uint32(i + 1), ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined)}}, //nolint:gosec // test
		})
		require.NoError(t, err)
		bundles[i] = b
	}

	t.Run("returns the same tree for the same bytes", func(t *testing.T) {
		first, err := c.load(bundles[0])
		require.NoError(t, err)
		second, err := c.load(bundles[0])
		require.NoError(t, err)
		assert.Same(t, first, second)
	})

	t.Run("evicts the oldest entry", func(t *testing.T) {
		for _, b := range bundles[1:] {
			_, err := c.load(b)
			require.NoError(t, err)
		}
		assert.Len(t, c.entries, 2)
		assert.Len(t, c.order, 2)
		assert.NotContains(t, c.entries, sha256.Sum256(bundles[0]))
	})

	t.Run("expands to the same deliveries as the uncached path", func(t *testing.T) {
		loaded, err := c.load(bundles[2])
		require.NoError(t, err)
		deliveries, err := loaded.Expand(bundles[2])
		require.NoError(t, err)
		decoded, err := DecodeMerkleBundle(bundles[2])
		require.NoError(t, err)
		root, err := MerkleBundleRoot(decoded)
		require.NoError(t, err)
		expanded, err := ExpandMerkleBundle(decoded)
		require.NoError(t, err)
		assert.Equal(t, root, loaded.Root)
		assert.True(t, proto.Equal(expanded[0], deliveries[0]))
		assert.Len(t, deliveries, len(expanded))
	})

	t.Run("rejects bytes of another bundle", func(t *testing.T) {
		loaded, err := c.load(bundles[2])
		require.NoError(t, err)
		other, err := EncodeMerkleBundle(&LLOMerkleBundleProto{
			Family: uint32(llotypes.ReportFormatEVMMerkle),
			Leaves: []*LLOMerkleLeafProto{
				{ChannelID: 1, ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined)},
				{ChannelID: 2, ReportFormat: uint32(llotypes.ReportFormatEVMStreamlined)},
			},
		})
		require.NoError(t, err)
		_, err = loaded.Expand(other)
		require.EqualError(t, err, "bundle does not match the loaded merkle tree")
	})
}
