package zstd

import (
	"errors"
	"fmt"
	"math"
	"runtime"
)

// EOption is an option for creating a encoder.
type EOption func(*encoderOptions) error

// options retains accumulated state of multiple options.
type encoderOptions struct {
	resetOpt         bool
	concurrent       int
	level            EncoderLevel
	single           *bool
	pad              int
	blockSize        int
	windowSize       int
	crc              bool
	fullZero         bool
	noEntropy        bool
	allLitEntropy    bool
	customWindow     bool
	customALEntropy  bool
	customBlockSize  bool
	lowMem           bool
	dict             *dict
	concurrentBlocks bool
}

func (o *encoderOptions) setDefault() {
	*o = encoderOptions{
		concurrent:    runtime.GOMAXPROCS(0),
		crc:           true,
		single:        nil,
		blockSize:     maxCompressedBlockSize,
		windowSize:    8 << 20,
		level:         SpeedDefault,
		allLitEntropy: false,
		lowMem:        false,
		fullZero:      true,
	}
}

// encoder returns an encoder with the selected options.
func (o encoderOptions) encoder() encoder {
	switch o.level {
	case SpeedDefault:
		return &doubleFastEncoder{fastEncoder: fastEncoder{fastBase: fastBase{maxMatchOff: int32(o.windowSize), bufferReset: math.MaxInt32 - int32(o.windowSize*2), lowMem: o.lowMem}}}
	}
	panic("unknown compression level")
}

// WithEncoderCRC will add CRC value to output.
// Output will be 4 bytes larger.
// Can be changed with ResetWithOptions.
func WithEncoderCRC(b bool) EOption {
	return func(o *encoderOptions) error { o.crc = b; return nil }
}

// EncoderLevel predefines encoder compression levels.
// Only use the constants made available, since the actual mapping
// of these values are very likely to change and your compression could change
// unpredictably when upgrading the library.
type EncoderLevel int

const (
	speedNotSet EncoderLevel = iota

	// SpeedFastest will choose the fastest reasonable compression.
	// This is roughly equivalent to the fastest Zstandard mode.
	SpeedFastest

	// SpeedDefault is the default "pretty fast" compression option.
	// This is roughly equivalent to the default Zstandard mode (level 3).
	SpeedDefault

	// SpeedBetterCompression will yield better compression than the default.
	// Currently it is about zstd level 7-8 with ~ 2x-3x the default CPU usage.
	// By using this, notice that CPU usage may go up in the future.
	SpeedBetterCompression

	// SpeedBestCompression will choose the best available compression option.
	// This will offer the best compression no matter the CPU cost.
	SpeedBestCompression

	// speedLast should be kept as the last actual compression option.
	// The is not for external usage, but is used to keep track of the valid options.
	speedLast
)

// WithEncoderLevel specifies a predefined compression level.
// Cannot be changed with ResetWithOptions.
func WithEncoderLevel(l EncoderLevel) EOption {
	return func(o *encoderOptions) error {
		switch {
		case l <= speedNotSet || l >= speedLast:
			return fmt.Errorf("unknown encoder level")
		}
		if o.resetOpt && l != o.level {
			return errors.New("WithEncoderLevel cannot be changed on Reset")
		}
		o.level = l
		if !o.customWindow {
			switch o.level {
			case SpeedFastest:
				o.windowSize = 4 << 20
				if !o.customBlockSize {
					o.blockSize = 1 << 16
				}
			case SpeedDefault:
				o.windowSize = 8 << 20
			case SpeedBetterCompression:
				o.windowSize = 8 << 20
			case SpeedBestCompression:
				o.windowSize = 8 << 20
			}
		}
		if !o.customALEntropy {
			o.allLitEntropy = l > SpeedDefault
		}

		return nil
	}
}

// WithZeroFrames will encode 0 length input as full frames.
// This can be needed for compatibility with zstandard usage,
// but is not needed for this package.
// Can be changed with ResetWithOptions.
func WithZeroFrames(b bool) EOption {
	return func(o *encoderOptions) error {
		o.fullZero = b
		return nil
	}
}

// WithSingleSegment will set the "single segment" flag when EncodeAll is used.
// If this flag is set, data must be regenerated within a single continuous memory segment.
// In this case, Window_Descriptor byte is skipped, but Frame_Content_Size is necessarily present.
// As a consequence, the decoder must allocate a memory segment of size equal or larger than size of your content.
// In order to preserve the decoder from unreasonable memory requirements,
// a decoder is allowed to reject a compressed frame which requests a memory size beyond decoder's authorized range.
// For broader compatibility, decoders are recommended to support memory sizes of at least 8 MB.
// This is only a recommendation, each decoder is free to support higher or lower limits, depending on local limitations.
// If this is not specified, block encodes will automatically choose this based on the input size and the window size.
// This setting has no effect on streamed encodes.
// Can be changed with ResetWithOptions.
func WithSingleSegment(b bool) EOption {
	return func(o *encoderOptions) error {
		o.single = &b
		return nil
	}
}

// jobSize returns the input section size per parallel job.
func (o *encoderOptions) jobSize() int {
	s := max(o.windowSize*4, 512<<10)
	return s
}

// overlapSize returns the overlap prefix size for parallel jobs.
func (o *encoderOptions) overlapSize() int {
	switch o.level {
	case SpeedBestCompression:
		return o.windowSize / 2
	case SpeedBetterCompression:
		return o.windowSize / 4
	default:
		return o.windowSize / 8
	}
}
