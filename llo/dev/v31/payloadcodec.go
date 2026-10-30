package llo

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/smartcontractkit/libocr/offchainreporting2plus/ocr3_1types"

	protocol "github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

// Payload codec identifiers. The first byte of a framed payload (a blob, the
// precursor or a KV record) names the codec used for the remaining bytes, so a reader never
// has to guess and the writer stays free to fall back to raw when compression
// does not pay off.
const (
	payloadCodecRaw  byte = 0
	payloadCodecZstd byte = 1
)

// maxDecompressedBlobPayloadBytes bounds the size of a blob payload after
// decompression. A blob is a stream-values-only observation proto, so the same
// bound the v30 observation codec uses applies here: it is generously above any
// honest payload while keeping a malicious peer from turning a small blob into
// a huge allocation (zstd bomb).
const maxDecompressedBlobPayloadBytes = protocol.MaxDecompressedObservationLength

// MaxBlobPayloadBytes is the size of a broadcast blob payload libocr accepts,
// and is the value the factory declares as MaxBlobPayloadBytes. It bounds the
// payload as framed for broadcast (codec byte plus body), which is a different
// quantity from maxDecompressedBlobPayloadBytes: the latter is the anti-bomb
// bound applied to the decompressed bytes on the read side, and is deliberately
// looser. Enforcing this one on the write side is what stops the pump from
// broadcasting a blob every peer's libocr would reject.
const MaxBlobPayloadBytes = ocr3_1types.MaxMaxBlobPayloadBytes

// maxDecompressedPrecursorBytes bounds the raw precursor proto. It equals the
// declared libocr limit on purpose: admission keeps the raw precursor under it
// (see TestLimits_PrecursorWorstCaseFitsDeclared), so compression is headroom
// and never what makes a precursor fit.
const maxDecompressedPrecursorBytes = ocr3_1types.MaxMaxReportsPlusPrecursorBytes

// maxKVRecordBytes bounds a framed KV record both raw and framed. libocr
// rejects a value over MaxMaxKeyValueValueBytes, and the limits tests keep the
// raw worst case of every framed record under it, so compression is headroom
// here too.
const maxKVRecordBytes = ocr3_1types.MaxMaxKeyValueValueBytes

// zstd Encoder/Decoder are safe for concurrent use via EncodeAll/DecodeAll and
// are expensive to build, so a single pair is shared process-wide. Built lazily
// so a construction failure surfaces at the call site rather than in init.
var (
	payloadZstd     payloadCompressor
	payloadZstdOnce sync.Once
	payloadZstdErr  error
)

type payloadCompressor struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func getPayloadCompressor() (payloadCompressor, error) {
	payloadZstdOnce.Do(func() {
		// The precursor is digested and signed, and KV records feed the state
		// root, so every oracle must produce the same bytes. zstd output is stable for one library version and
		// one set of options. Pin the options that shape the output instead of
		// relying on defaults; Test_Golden_Precursor and Test_Golden_KVRecords
		// catch a library bump that changes the bitstream.
		encoder, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderCRC(true),
			zstd.WithZeroFrames(false),
		)
		if err != nil {
			payloadZstdErr = fmt.Errorf("new zstd encoder: %w", err)
			return
		}
		// WithDecoderMaxMemory bounds the decompressed size to a safe limit.
		// WithDecodeAllCapLimit bounds each DecodeAll call to the capacity of
		// its destination, which decodeFramedPayload sizes to the call's budget.
		decoder, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(0),
			zstd.WithDecoderMaxMemory(max(maxDecompressedBlobPayloadBytes, maxDecompressedPrecursorBytes, maxKVRecordBytes)),
			zstd.WithDecodeAllCapLimit(true),
		)
		if err != nil {
			payloadZstdErr = fmt.Errorf("new zstd decoder: %w", err)
			return
		}
		payloadZstd = payloadCompressor{encoder: encoder, decoder: decoder}
	})
	return payloadZstd, payloadZstdErr
}

// encodeFramedPayload frames raw bytes behind a codec byte, compressing them
// when that actually shrinks the payload. The choice depends only on raw, so
// two encoders built from the same library version always agree on it. maxRaw
// bounds raw so a reader with the same bound can decode it, and maxFramed is
// the size libocr accepts. what names the payload in errors.
func encodeFramedPayload(raw []byte, maxRaw, maxFramed int, what string) ([]byte, error) {
	if len(raw) > maxRaw {
		// Refuse to emit a payload every peer would reject on decompression;
		// fail loudly here instead.
		return nil, fmt.Errorf("%s too large: %d > %d bytes", what, len(raw), maxRaw)
	}
	c, err := getPayloadCompressor()
	if err != nil {
		return nil, err
	}
	out := c.encoder.EncodeAll(raw, []byte{payloadCodecZstd})
	if len(out) >= len(raw)+1 {
		out = make([]byte, 0, len(raw)+1)
		out = append(out, payloadCodecRaw)
		out = append(out, raw...)
	}
	// A payload that compresses poorly can pass the raw check and still land
	// over the framed limit.
	if len(out) > maxFramed {
		return nil, fmt.Errorf("framed %s too large: %d > %d bytes", what, len(out), maxFramed)
	}
	return out, nil
}

// decodeFramedPayload reverses encodeFramedPayload. The payload is untrusted,
// so the decompressed size is bounded by maxOut: cheaply up front via the zstd
// frame header when it declares a content size, and during decompression via
// the capacity of the destination buffer in case it does not.
func decodeFramedPayload(payload []byte, maxOut int, what string) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty %s", what)
	}
	if maxOut <= 0 {
		return nil, fmt.Errorf("%s budget exhausted", what)
	}
	codec, body := payload[0], payload[1:]
	switch codec {
	case payloadCodecRaw:
		if len(body) > maxOut {
			return nil, fmt.Errorf("%s too large: %d > %d bytes", what, len(body), maxOut)
		}
		return body, nil
	case payloadCodecZstd:
		c, err := getPayloadCompressor()
		if err != nil {
			return nil, err
		}
		// Reject an oversized frame before spending decompression work on it.
		// A hostile peer can omit or understate the declared size, so this is
		// an optimization, not the enforcement point.
		var hdr zstd.Header
		if err := hdr.Decode(body); err != nil {
			return nil, fmt.Errorf("decode %s header: %w", what, err)
		}
		if hdr.HasFCS && hdr.FrameContentSize > uint64(maxOut) {
			return nil, fmt.Errorf("declared decompressed %s too large: %d > %d bytes", what, hdr.FrameContentSize, maxOut)
		}
		// The decoder stops once the output outgrows the destination capacity,
		// so the per-call budget holds while decompressing, not only after.
		// encodeFramedPayload always declares the content size, so an honest
		// payload allocates exactly its size here.
		capacity := maxOut
		if hdr.HasFCS {
			capacity = int(hdr.FrameContentSize) //nolint:gosec // bounded by maxOut above
		}
		out, err := c.decoder.DecodeAll(body, make([]byte, 0, capacity))
		if err != nil {
			return nil, fmt.Errorf("decompress %s: %w", what, err)
		}
		// The cap limit is checked per block, so a block can overshoot it.
		if len(out) > maxOut {
			return nil, fmt.Errorf("decompressed %s too large: %d > %d bytes", what, len(out), maxOut)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown %s codec %d", what, codec)
	}
}

// encodeBlobPayload frames raw proto bytes for broadcast. Unlike the
// precursor, nodes need not agree on these bytes: each blob is fetched and
// decoded on its own.
func encodeBlobPayload(raw []byte) ([]byte, error) {
	return encodeFramedPayload(raw, maxDecompressedBlobPayloadBytes, MaxBlobPayloadBytes, "blob payload")
}

// decodeBlobPayload reverses encodeBlobPayload. maxOut is a caller-supplied
// budget (see fetchBlobsAndDecodeObservation) and is clamped to
// maxDecompressedBlobPayloadBytes.
func decodeBlobPayload(payload []byte, maxOut int) ([]byte, error) {
	return decodeFramedPayload(payload, min(maxOut, maxDecompressedBlobPayloadBytes), "blob payload")
}

// encodeKVRecord frames a marshaled KV record for writing. libocr builds the
// state root from these bytes, so they must match on every oracle, as for the
// precursor.
func encodeKVRecord(raw []byte, what string) ([]byte, error) {
	return encodeFramedPayload(raw, maxKVRecordBytes, maxKVRecordBytes, what)
}

// decodeKVRecord reverses encodeKVRecord.
func decodeKVRecord(b []byte, what string) ([]byte, error) {
	return decodeFramedPayload(b, maxKVRecordBytes, what)
}
