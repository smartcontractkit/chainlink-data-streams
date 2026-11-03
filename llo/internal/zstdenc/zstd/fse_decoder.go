// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

const (
	tablelogAbsoluteMax = 9
)

const (
	/*!MEMORY_USAGE :
	 *  Memory usage formula : N->2^N Bytes (examples : 10 -> 1KB; 12 -> 4KB ; 16 -> 64KB; 20 -> 1MB; etc.)
	 *  Increasing memory usage improves compression ratio
	 *  Reduced memory usage can improve speed, due to cache effect
	 *  Recommended max value is 14, for 16KB, which nicely fits into Intel x86 L1 cache */
	maxMemoryUsage = tablelogAbsoluteMax + 2

	maxTableLog    = maxMemoryUsage - 2
	maxTablesize   = 1 << maxTableLog
	maxTableMask   = (1 << maxTableLog) - 1
	minTablelog    = 5
	maxSymbolValue = 255
)

// fseDecoder provides temporary storage for compression and decompression.
type fseDecoder struct {
	symbolLen      uint16 // Length of active part of the symbol table.
	actualTableLog uint8  // Selected tablelog.

	norm [maxSymbolValue + 1]int16
}

// tableStep returns the next table index.
func tableStep(tableSize uint32) uint32 {
	return (tableSize >> 1) + (tableSize >> 3) + 3
}
