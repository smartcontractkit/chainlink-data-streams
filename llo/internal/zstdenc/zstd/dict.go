package zstd

import (
	"io"

	"github.com/smartcontractkit/chainlink-data-streams/llo/internal/zstdenc/huff0"
)

type dict struct {
	id uint32

	litEnc              *huff0.Scratch
	llEnc, ofEnc, mlEnc *fseEncoder
	offsets             [3]int
	content             []byte
}

// ID returns the dictionary id or 0 if d is nil.
func (d *dict) ID() uint32 {
	if d == nil {
		return 0
	}
	return d.id
}

// ContentSize returns the dictionary content size or 0 if d is nil.
func (d *dict) ContentSize() int {
	if d == nil {
		return 0
	}
	return len(d.content)
}

type BuildDictOptions struct {
	// Dictionary ID.
	ID uint32

	// Content to use to create dictionary tables.
	Contents [][]byte

	// History to use for all blocks.
	History []byte

	// Offsets to use.
	Offsets [3]int

	// CompatV155 will make the dictionary compatible with Zstd v1.5.5 and earlier.
	// See https://github.com/facebook/zstd/issues/3724
	CompatV155 bool

	// Use the specified encoder level.
	// The dictionary will be built using the specified encoder level,
	// which will reflect speed and make the dictionary tailored for that level.
	// If not set SpeedBestCompression will be used.
	Level EncoderLevel

	// DebugOut will write stats and other details here if set.
	DebugOut io.Writer
}
