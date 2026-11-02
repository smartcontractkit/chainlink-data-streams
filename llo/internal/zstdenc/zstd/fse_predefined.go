// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"fmt"
	"sync"
)

var (
	// fsePredef are the predefined fse tables as defined here:
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#default-distributions
	// These values are already transformed.
	fsePredef [3]fseDecoder

	// fsePredefEnc are the predefined encoder based on fse tables as defined here:
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#default-distributions
	// These values are already transformed.
	fsePredefEnc [3]fseEncoder

	// bitTables is the bits table for each table.
	bitTables = [3][]byte{tableLiteralLengths: llBitsTable[:], tableOffsets: nil, tableMatchLengths: mlBitsTable[:]}
)

type tableIndex uint8

const (
	// indexes for fsePredef and symbolTableX
	tableLiteralLengths tableIndex = 0
	tableOffsets        tableIndex = 1
	tableMatchLengths   tableIndex = 2

	maxLiteralLengthSymbol = 35
	maxMatchLengthSymbol   = 52
)

var predef sync.Once

func initPredefined() {
	predef.Do(func() {
		// Fill predefined tables and transform them.
		// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#default-distributions
		for i := range fsePredef[:] {
			f := &fsePredef[i]
			switch tableIndex(i) {
			case tableLiteralLengths:
				// https://github.com/facebook/zstd/blob/ededcfca57366461021c922720878c81a5854a0a/lib/decompress/zstd_decompress_block.c#L243
				f.actualTableLog = 6
				copy(f.norm[:], []int16{4, 3, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1, 1, 1,
					2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 2, 1, 1, 1, 1, 1,
					-1, -1, -1, -1})
				f.symbolLen = 36
			case tableOffsets:
				// https://github.com/facebook/zstd/blob/ededcfca57366461021c922720878c81a5854a0a/lib/decompress/zstd_decompress_block.c#L281
				f.actualTableLog = 5
				copy(f.norm[:], []int16{
					1, 1, 1, 1, 1, 1, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
					1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1})
				f.symbolLen = 29
			case tableMatchLengths:
				//https://github.com/facebook/zstd/blob/ededcfca57366461021c922720878c81a5854a0a/lib/decompress/zstd_decompress_block.c#L304
				f.actualTableLog = 6
				copy(f.norm[:], []int16{
					1, 4, 3, 2, 2, 2, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
					1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
					1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1,
					-1, -1, -1, -1, -1})
				f.symbolLen = 53
			}
			// Create encoder as well
			enc := &fsePredefEnc[i]
			copy(enc.norm[:], f.norm[:])
			enc.symbolLen = f.symbolLen
			enc.actualTableLog = f.actualTableLog
			if err := enc.buildCTable(); err != nil {
				panic(fmt.Errorf("building encoding table %v: %v", tableIndex(i), err))
			}
			enc.setBits(bitTables[i])
			enc.preDefined = true
		}
	})
}
