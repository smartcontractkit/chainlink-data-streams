// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"crypto/rand"
	"io"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// Encoder provides encoding to Zstandard.
// An Encoder can be used for either compressing a stream via the
// io.WriteCloser interface supported by the Encoder or as multiple independent
// tasks via the EncodeAll function.
// Smaller encodes are encouraged to use the EncodeAll function.
// Use NewWriter to create a new instance.
type Encoder struct {
	o        encoderOptions
	encoders chan encoder
	state    encoderState
	init     sync.Once
}

type encoder interface {
	Encode(blk *blockEnc, src []byte)
	EncodeNoHist(blk *blockEnc, src []byte)
	Block() *blockEnc
	CRC() *xxhash.Digest
	AppendCRC([]byte) []byte
	WindowSize(size int64) int32
	Reset(d *dict, singleBlock bool)
}

type encoderState struct {
	w                io.Writer
	filling          []byte
	current          []byte
	previous         []byte
	encoder          encoder
	writing          *blockEnc
	err              error
	writeErr         error
	nWritten         int64
	nInput           int64
	frameContentSize int64
	headerWritten    bool
	eofWritten       bool
	fullFrameWritten bool

	// This waitgroup indicates an encode is running.
	wg sync.WaitGroup
	// This waitgroup indicates we have a block encoding/writing.
	wWg sync.WaitGroup

	// Parallel job state (used when concurrentBlocks is enabled).
	jobs jobState
}

// NewWriter will create a new Zstandard encoder.
// If the encoder will be used for encoding blocks a nil writer can be used.
func NewWriter(w io.Writer, opts ...EOption) (*Encoder, error) {
	initPredefined()
	var e Encoder
	e.o.setDefault()
	for _, o := range opts {
		err := o(&e.o)
		if err != nil {
			return nil, err
		}
	}
	if e.o.concurrentBlocks && (e.o.dict != nil || e.o.concurrent <= 1) {
		e.o.concurrentBlocks = false
	}
	if w != nil {
		e.Reset(w)
	}
	return &e, nil
}

func (e *Encoder) initialize() {
	if e.o.concurrent == 0 {
		e.o.setDefault()
	}
	e.encoders = make(chan encoder, e.o.concurrent)
	for i := 0; i < e.o.concurrent; i++ {
		enc := e.o.encoder()
		e.encoders <- enc
	}
}

// Reset will re-initialize the writer and new writes will encode to the supplied writer
// as a new, independent stream.
func (e *Encoder) Reset(w io.Writer) {
	s := &e.state

	if e.o.concurrentBlocks {
		e.shutdownJobWorkers()
		js := &s.jobs
		js.jobSize = e.o.jobSize()
		js.overlapSize = e.o.overlapSize()
		// js.filling is allocated lazily on first Write/ReadFrom so callers
		// that only use EncodeAll don't pay the (up to ~32 MB) jobSize cost.
		js.filling = js.filling[:0]
		if js.nextPrefix != nil {
			js.putOverlapBuf(js.nextPrefix)
			js.nextPrefix = nil
		}
		js.jobSeq = 0
		js.flushedSeq = 0
		js.flusherErr = nil
		js.started = false
	}

	s.wg.Wait()
	s.wWg.Wait()
	if cap(s.filling) == 0 {
		s.filling = make([]byte, 0, e.o.blockSize)
	}
	if e.o.concurrent > 1 && !e.o.concurrentBlocks {
		if cap(s.current) == 0 {
			s.current = make([]byte, 0, e.o.blockSize)
		}
		if cap(s.previous) == 0 {
			s.previous = make([]byte, 0, e.o.blockSize)
		}
		s.current = s.current[:0]
		s.previous = s.previous[:0]
		if s.writing == nil {
			s.writing = &blockEnc{lowMem: e.o.lowMem}
			s.writing.init()
		}
		s.writing.initNewEncode()
	}
	if s.encoder == nil {
		s.encoder = e.o.encoder()
	}
	s.filling = s.filling[:0]
	s.encoder.Reset(e.o.dict, false)
	s.headerWritten = false
	s.eofWritten = false
	s.fullFrameWritten = false
	s.w = w
	s.err = nil
	s.nWritten = 0
	s.nInput = 0
	s.writeErr = nil
	s.frameContentSize = 0
}

// EncodeAll will encode all input in src and append it to dst.
// This function can be called concurrently, but each call will only run on a single goroutine.
// If empty input is given, nothing is returned, unless WithZeroFrames is specified.
// Encoded blocks can be concatenated and the result will be the combined input stream.
// Data compressed with EncodeAll can be decoded with the Decoder,
// using either a stream or DecodeAll.
func (e *Encoder) EncodeAll(src, dst []byte) []byte {
	e.init.Do(e.initialize)
	enc := <-e.encoders
	defer func() {
		e.encoders <- enc
	}()
	return e.encodeAll(enc, src, dst)
}

func (e *Encoder) encodeAll(enc encoder, src, dst []byte) []byte {
	if len(src) == 0 {
		if e.o.fullZero {
			// Add frame header.
			fh := frameHeader{
				ContentSize:   0,
				WindowSize:    MinWindowSize,
				SingleSegment: true,
				// Adding a checksum would be a waste of space.
				Checksum: false,
				DictID:   0,
			}
			dst = fh.appendTo(dst)

			// Write raw block as last one only.
			var blk blockHeader
			blk.setSize(0)
			blk.setType(blockTypeRaw)
			blk.setLast(true)
			dst = blk.appendTo(dst)
		}
		return dst
	}

	// Use single segments when above minimum window and below window size.
	single := len(src) <= e.o.windowSize && len(src) > MinWindowSize
	if e.o.single != nil {
		single = *e.o.single
	}
	fh := frameHeader{
		ContentSize:   uint64(len(src)),
		WindowSize:    uint32(enc.WindowSize(int64(len(src)))),
		SingleSegment: single,
		Checksum:      e.o.crc,
		DictID:        e.o.dict.ID(),
	}

	// If less than 1MB, allocate a buffer up front.
	if len(dst) == 0 && cap(dst) == 0 && len(src) < 1<<20 && !e.o.lowMem {
		dst = make([]byte, 0, len(src))
	}
	dst = fh.appendTo(dst)

	// If we can do everything in one block, prefer that.
	if len(src) <= e.o.blockSize {
		enc.Reset(e.o.dict, true)
		// Slightly faster with no history and everything in one block.
		if e.o.crc {
			_, _ = enc.CRC().Write(src)
		}
		blk := enc.Block()
		blk.last = true
		if e.o.dict == nil {
			enc.EncodeNoHist(blk, src)
		} else {
			enc.Encode(blk, src)
		}

		// If we got the exact same number of literals as input,
		// assume the literals cannot be compressed.
		oldout := blk.output
		// Output directly to dst
		blk.output = dst

		err := blk.encode(src, e.o.noEntropy, !e.o.allLitEntropy)
		if err != nil {
			panic(err)
		}
		dst = blk.output
		blk.output = oldout
	} else {
		enc.Reset(e.o.dict, false)
		blk := enc.Block()
		for len(src) > 0 {
			todo := src
			if len(todo) > e.o.blockSize {
				todo = todo[:e.o.blockSize]
			}
			src = src[len(todo):]
			if e.o.crc {
				_, _ = enc.CRC().Write(todo)
			}
			blk.pushOffsets()
			enc.Encode(blk, todo)
			if len(src) == 0 {
				blk.last = true
			}
			err := blk.encode(todo, e.o.noEntropy, !e.o.allLitEntropy)
			if err != nil {
				panic(err)
			}
			dst = append(dst, blk.output...)
			blk.reset(nil)
		}
	}
	if e.o.crc {
		dst = enc.AppendCRC(dst)
	}
	// Add padding with content from crypto/rand.Reader
	if e.o.pad > 0 {
		add := calcSkippableFrame(int64(len(dst)), int64(e.o.pad))
		var err error
		dst, err = skippableFrame(dst, add, rand.Reader)
		if err != nil {
			panic(err)
		}
	}
	return dst
}
