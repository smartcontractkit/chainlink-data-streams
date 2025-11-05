package llo

import (
	"github.com/klauspost/compress/zstd"
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
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, err
	}
	return &compressor{encoder, decoder}, nil
}

func (c *compressor) Compress(b []byte) ([]byte, error) {
	compressed := c.encoder.EncodeAll(b, nil)
	return compressed, nil
}

func (c *compressor) Decompress(b []byte) ([]byte, error) {
	return c.decoder.DecodeAll(b, nil)
}
