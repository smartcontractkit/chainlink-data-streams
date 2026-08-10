package llo

import (
	"fmt"

	"github.com/klauspost/compress/zstd"

	"github.com/smartcontractkit/chainlink-data-streams/llo/protocol"
)

type compressor struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func newCompressor() (*compressor, error) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	// WithDecoderMaxMemory bounds the decompressed size to a safe limit
	decoder, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(0),
		zstd.WithDecoderMaxMemory(protocol.MaxDecompressedObservationLength),
	)
	if err != nil {
		return nil, err
	}
	return &compressor{encoder, decoder}, nil
}

func (c *compressor) CompressObservation(b []byte) ([]byte, error) {
	if len(b) > protocol.MaxDecompressedObservationLength {
		// Refuse to emit an observation that every peer would reject on
		// decompression; fail loudly here instead.
		return nil, fmt.Errorf("observation too large to compress: %d > %d bytes", len(b), protocol.MaxDecompressedObservationLength)
	}
	compressed := c.encoder.EncodeAll(b, nil)
	return compressed, nil
}

func (c *compressor) DecompressObservation(b []byte) ([]byte, error) {
	out, err := c.decoder.DecodeAll(b, nil)
	if err != nil {
		return nil, err
	}
	// DecodeAll already enforces the limit above, but keep an
	// explicit check so the bound survives any change to the decoder options.
	if len(out) > protocol.MaxDecompressedObservationLength {
		return nil, fmt.Errorf("decompressed observation too large: %d > %d bytes", len(out), protocol.MaxDecompressedObservationLength)
	}
	return out, nil
}
