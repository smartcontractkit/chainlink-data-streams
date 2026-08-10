package llo

import (
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// zstdBomb returns a zstd payload that decompresses to n zero bytes.
func zstdBomb(t *testing.T, n int) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	defer enc.Close()
	return enc.EncodeAll(make([]byte, n), nil)
}

func Test_compressor_decompressionBomb(t *testing.T) {
	// The OCR3 transport limit only bounds the compressed observation
	// (MaxObservationLength). A byzantine peer can fit a payload that expands
	// by ~9000x inside it, so decompression must be bounded independently.
	t.Run("rejects payload that expands beyond MaxDecompressedObservationLength", func(t *testing.T) {
		bomb := zstdBomb(t, protocol.MaxDecompressedObservationLength+1)
		require.Less(t, len(bomb), MaxObservationLength, "bomb must fit within the on-wire limit for this test to be meaningful")

		c, err := newCompressor()
		require.NoError(t, err)

		_, err = c.DecompressObservation(bomb)
		require.Error(t, err)
	})

	t.Run("accepts payload at the limit", func(t *testing.T) {
		c, err := newCompressor()
		require.NoError(t, err)

		out, err := c.DecompressObservation(zstdBomb(t, protocol.MaxDecompressedObservationLength))
		require.NoError(t, err)
		assert.Len(t, out, protocol.MaxDecompressedObservationLength)
	})

	t.Run("Decode does not allocate an error string proportional to the decompressed payload", func(t *testing.T) {
		// All-zero bytes are not valid protobuf, so this exercises the
		// unmarshal error path. It must not echo the decompressed payload.
		bomb := zstdBomb(t, protocol.MaxDecompressedObservationLength+1)

		codec, err := NewProtoObservationCodec(logger.Nop(), true)
		require.NoError(t, err)

		_, err = codec.Decode(bomb)
		require.Error(t, err)
		assert.Less(t, len(err.Error()), 1024, "error string must not embed the untrusted payload")
	})

	t.Run("Decode error on valid-length garbage does not embed the payload", func(t *testing.T) {
		bomb := zstdBomb(t, 1<<20) // 1 MiB of zeros: decompresses fine, invalid protobuf

		codec, err := NewProtoObservationCodec(logger.Nop(), true)
		require.NoError(t, err)

		_, err = codec.Decode(bomb)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode observation: expected protobuf (len: 1048576)")
		assert.Less(t, len(err.Error()), 1024)
	})

	t.Run("CompressObservation refuses oversized input", func(t *testing.T) {
		c, err := newCompressor()
		require.NoError(t, err)

		_, err = c.CompressObservation(make([]byte, protocol.MaxDecompressedObservationLength+1))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "observation too large to compress")
	})
}
