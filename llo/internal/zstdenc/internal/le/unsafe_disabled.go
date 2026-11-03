//go:build !(amd64 || arm64 || ppc64le || riscv64) || nounsafe || purego

package le

import (
	"encoding/binary"
)

// Load32 will load from b at index i.
func Load32[I Indexer](b []byte, i I) uint32 {
	return binary.LittleEndian.Uint32(b[i:])
}

// Load64 will load from b at index i.
func Load64[I Indexer](b []byte, i I) uint64 {
	return binary.LittleEndian.Uint64(b[i:])
}

// Store64 will store v at b.
func Store64[I Indexer](b []byte, i I, v uint64) {
	binary.LittleEndian.PutUint64(b[i:], v)
}
