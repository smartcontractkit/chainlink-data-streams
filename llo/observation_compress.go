package llo

import (
	"github.com/klauspost/compress/zstd"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
)

type Compressor struct {
	logger  logger.Logger
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

func NewCompressor(lggr logger.Logger) *Compressor {
	encoder, _ := zstd.NewWriter(nil)
	decoder, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	return &Compressor{logger.Sugared(lggr).Named("Compressor"), encoder, decoder}
}

func (c *Compressor) CompressObservation(b []byte) ([]byte, error) {
	compressed := c.encoder.EncodeAll(b, nil)
	c.logger.Debugw("compressed observation", "compressed_size", len(compressed), "uncompressed_size", len(b))
	return compressed, nil
}

func (c *Compressor) DecompressObservation(b []byte) ([]byte, error) {
	uncompressed, err := c.decoder.DecodeAll(b, nil)
	if err != nil {
		return nil, err
	}
	return uncompressed, nil
}
