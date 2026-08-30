package llo

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Blob payload codec identifiers. The first byte of a blob payload names the
// codec used for the remaining bytes, so a reader never has to guess and the
// writer stays free to fall back to raw when compression does not pay off.
const (
	blobCodecRaw  byte = 0
	blobCodecZstd byte = 1
)

// maxDecompressedBlobPayloadBytes bounds the size of a blob payload after
// decompression. A blob is a stream-values-only observation proto, so the same
// bound the v30 observation codec uses applies here: it is generously above any
// honest payload while keeping a malicious peer from turning a small blob into
// a huge allocation (zstd bomb).
const maxDecompressedBlobPayloadBytes = protocol.MaxDecompressedObservationLength

// zstd Encoder/Decoder are safe for concurrent use via EncodeAll/DecodeAll and
// are expensive to build, so a single pair is shared process-wide. Built lazily
// so a construction failure surfaces at the call site rather than in init.
var (
	blobZstd     blobCompressor
	blobZstdOnce sync.Once
	blobZstdErr  error
)

type blobCompressor struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func getBlobCompressor() (blobCompressor, error) {
	blobZstdOnce.Do(func() {
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			blobZstdErr = fmt.Errorf("new zstd encoder: %w", err)
			return
		}
		// WithDecoderMaxMemory bounds the decompressed size to a safe limit.
		decoder, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(0),
			zstd.WithDecoderMaxMemory(maxDecompressedBlobPayloadBytes),
		)
		if err != nil {
			blobZstdErr = fmt.Errorf("new zstd decoder: %w", err)
			return
		}
		blobZstd = blobCompressor{encoder: encoder, decoder: decoder}
	})
	return blobZstd, blobZstdErr
}

// encodeBlobPayload frames raw proto bytes for broadcast, compressing them when
// that actually shrinks the payload. The codec choice is made here and recorded
// in the leading byte; readers never re-derive it, so nodes are free to disagree
// on whether compression paid off without affecting the decoded result.
func encodeBlobPayload(raw []byte) ([]byte, error) {
	if len(raw) > maxDecompressedBlobPayloadBytes {
		// Refuse to emit a blob every peer would reject on decompression;
		// fail loudly here instead.
		return nil, fmt.Errorf("blob payload too large: %d > %d bytes", len(raw), maxDecompressedBlobPayloadBytes)
	}
	c, err := getBlobCompressor()
	if err != nil {
		return nil, err
	}
	compressed := c.encoder.EncodeAll(raw, []byte{blobCodecZstd})
	if len(compressed) < len(raw)+1 {
		return compressed, nil
	}
	out := make([]byte, 0, len(raw)+1)
	out = append(out, blobCodecRaw)
	return append(out, raw...), nil
}

// decodeBlobPayload reverses encodeBlobPayload. The payload is untrusted, so
// the decompressed size is bounded by maxOut: cheaply up front via the zstd
// frame header when it declares a content size, and again after the fact in
// case it does not. maxOut is a caller-supplied budget (see decodeObservation)
// and is clamped to maxDecompressedBlobPayloadBytes.
func decodeBlobPayload(payload []byte, maxOut int) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty blob payload")
	}
	if maxOut <= 0 {
		return nil, fmt.Errorf("blob payload budget exhausted")
	}
	if maxOut > maxDecompressedBlobPayloadBytes {
		maxOut = maxDecompressedBlobPayloadBytes
	}
	codec, body := payload[0], payload[1:]
	switch codec {
	case blobCodecRaw:
		if len(body) > maxOut {
			return nil, fmt.Errorf("blob payload too large: %d > %d bytes", len(body), maxOut)
		}
		return body, nil
	case blobCodecZstd:
		c, err := getBlobCompressor()
		if err != nil {
			return nil, err
		}
		// Reject an oversized frame before spending decompression work on it.
		// A hostile peer can omit or understate the declared size, so this is
		// an optimization, not the enforcement point.
		var hdr zstd.Header
		if err := hdr.Decode(body); err != nil {
			return nil, fmt.Errorf("decode blob payload header: %w", err)
		}
		if hdr.HasFCS && hdr.FrameContentSize > uint64(maxOut) {
			return nil, fmt.Errorf("declared decompressed blob payload too large: %d > %d bytes", hdr.FrameContentSize, maxOut)
		}
		out, err := c.decoder.DecodeAll(body, nil)
		if err != nil {
			return nil, fmt.Errorf("decompress blob payload: %w", err)
		}
		// The shared decoder only enforces the process-wide ceiling, so this
		// check is what enforces the (possibly tighter) per-call budget.
		if len(out) > maxOut {
			return nil, fmt.Errorf("decompressed blob payload too large: %d > %d bytes", len(out), maxOut)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown blob payload codec %d", codec)
	}
}
