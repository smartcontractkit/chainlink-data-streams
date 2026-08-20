package llo

import (
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

func Test_BlobPayload_RoundTrip(t *testing.T) {
	t.Run("compressible payload uses zstd", func(t *testing.T) {
		raw := []byte(strings.Repeat("stream-values-are-repetitive", 200))
		framed, err := encodeBlobPayload(raw)
		require.NoError(t, err)
		require.Equal(t, blobCodecZstd, framed[0])
		assert.Less(t, len(framed), len(raw))

		got, err := decodeBlobPayload(framed)
		require.NoError(t, err)
		assert.Equal(t, raw, got)
	})

	t.Run("incompressible payload falls back to raw", func(t *testing.T) {
		// zstd cannot shrink a short high-entropy payload, so the encoder must
		// not pay the framing overhead of compressing it.
		raw := []byte{0x9f, 0x1c, 0x00, 0xd3, 0x7a}
		framed, err := encodeBlobPayload(raw)
		require.NoError(t, err)
		require.Equal(t, blobCodecRaw, framed[0])
		assert.Equal(t, len(raw)+1, len(framed))

		got, err := decodeBlobPayload(framed)
		require.NoError(t, err)
		assert.Equal(t, raw, got)
	})

	t.Run("empty payload", func(t *testing.T) {
		framed, err := encodeBlobPayload(nil)
		require.NoError(t, err)
		got, err := decodeBlobPayload(framed)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func Test_BlobPayload_MarshalStreamValuesRoundTrip(t *testing.T) {
	sv := make(protocol.StreamValues, 500)
	for i := uint32(0); i < 500; i++ {
		sv[i] = protocol.ToDecimal(decimal.NewFromInt(int64(i)))
	}
	payload, err := marshalStreamValues(sv)
	require.NoError(t, err)
	require.Equal(t, blobCodecZstd, payload[0])

	raw, err := decodeBlobPayload(payload)
	require.NoError(t, err)
	chunk := &protocol.LLOObservationProto{}
	require.NoError(t, proto.Unmarshal(raw, chunk))
	assert.Len(t, chunk.StreamValues, 500)
}

func Test_decodeBlobPayload_Errors(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, err := decodeBlobPayload(nil)
		require.ErrorContains(t, err, "empty blob payload")
	})

	t.Run("unknown codec", func(t *testing.T) {
		_, err := decodeBlobPayload([]byte{0x7f, 0x01, 0x02})
		require.ErrorContains(t, err, "unknown blob payload codec 127")
	})

	t.Run("corrupt zstd body", func(t *testing.T) {
		_, err := decodeBlobPayload([]byte{blobCodecZstd, 0xde, 0xad, 0xbe, 0xef})
		require.ErrorContains(t, err, "decompress blob payload")
	})

	t.Run("zstd bomb is rejected without allocating", func(t *testing.T) {
		enc, err := zstd.NewWriter(nil)
		require.NoError(t, err)
		bomb := enc.EncodeAll(make([]byte, maxDecompressedBlobPayloadBytes+1), []byte{blobCodecZstd})
		require.Less(t, len(bomb), 1<<20, "bomb should be tiny on the wire")

		_, err = decodeBlobPayload(bomb)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blob payload")
	})
}

func Test_encodeBlobPayload_RejectsOversized(t *testing.T) {
	_, err := encodeBlobPayload(make([]byte, maxDecompressedBlobPayloadBytes+1))
	require.ErrorContains(t, err, "blob payload too large")
}
