// We enable 64 bit LE platforms:

//go:build (amd64 || arm64 || ppc64le || riscv64) && !nounsafe && !purego

package le

import (
	"unsafe"
)

// Load32 will load from b at index i.
func Load32[I Indexer](b []byte, i I) uint32 {
	//return binary.LittleEndian.Uint32(b[i:])
	//return *(*uint32)(unsafe.Pointer(&b[i]))
	return *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i))
}

// Load64 will load from b at index i.
func Load64[I Indexer](b []byte, i I) uint64 {
	//return binary.LittleEndian.Uint64(b[i:])
	//return *(*uint64)(unsafe.Pointer(&b[i]))
	return *(*uint64)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i))
}

// Store64 will store v at b[i:].
func Store64[I Indexer](b []byte, i I, v uint64) {
	*(*uint64)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i)) = v
}
