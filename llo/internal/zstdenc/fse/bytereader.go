// Copyright 2018 Klaus Post. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Based on work Copyright (c) 2013, Yann Collet, released under BSD License.

package fse

// byteReader provides a byte reader that reads
// little endian values from a byte stream.
// The input stream is manually advanced.
// The reader performs no bounds checks.
type byteReader struct {
	b   []byte
	off int
}

// init will initialize the reader and set the input.
func (b *byteReader) init(in []byte) {
	b.b = in
	b.off = 0
}

// remain will return the number of bytes remaining.
func (b byteReader) remain() int {
	return len(b.b) - b.off
}
