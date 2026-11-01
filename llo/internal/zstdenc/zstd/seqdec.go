// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"fmt"
)

type seq struct {
	litLen   uint32
	matchLen uint32
	offset   uint32

	// Codes are stored here for the encoder
	// so they only have to be looked up once.
	llCode, mlCode, ofCode uint8
}

func (s seq) String() string {
	if s.offset <= 3 {
		if s.offset == 0 {
			return fmt.Sprint("litLen:", s.litLen, ", matchLen:", s.matchLen+zstdMinMatch, ", offset: INVALID (0)")
		}
		return fmt.Sprint("litLen:", s.litLen, ", matchLen:", s.matchLen+zstdMinMatch, ", offset:", s.offset, " (repeat)")
	}
	return fmt.Sprint("litLen:", s.litLen, ", matchLen:", s.matchLen+zstdMinMatch, ", offset:", s.offset-3, " (new)")
}

type seqCompMode uint8

const (
	compModePredefined seqCompMode = iota
	compModeRLE
	compModeFSE
	compModeRepeat
)

var bitMask [16]uint16

func init() {
	for i := range bitMask[:] {
		bitMask[i] = uint16((1 << uint(i)) - 1)
	}
}
