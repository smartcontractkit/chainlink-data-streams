package protocol

import "sync"

var (
	// ballastAlloc is a byte slice initialized to 1GB to reduce CPU cycles spent in garbage collection.
	// The plugin's data source pipeline performs many small allocations, which can frequently trigger the GC
	// and increase CPU usage during the mark phase. The ballast allocation is virtually addressed and does not
	// consume physical memory unless accessed. Since the Go GC runs when the heap size doubles, this ensures
	// GC is only triggered when the heap grows to 2GB.
	ballastAlloc []byte
	ballastOnce  sync.Once
	ballastSz    int = 1e9 // 1GB
)

// InitMemoryBallast allocates the memory ballast, at most once per process. It
// is shared across plugin versions so that a process running both v30 and v31
// holds a single ballast rather than one per version.
func InitMemoryBallast() {
	ballastOnce.Do(func() {
		ballastAlloc = make([]byte, ballastSz)
	})
}
