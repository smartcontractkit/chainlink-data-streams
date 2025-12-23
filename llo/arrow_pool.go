package llo

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// LLOMemoryPool wraps Arrow's allocator with metrics and bounds checking.
// This can replace the 1GB memory ballast by providing controlled allocation.
type LLOMemoryPool struct {
	allocator    memory.Allocator
	allocated    atomic.Int64
	maxBytes     int64
	allocCount   atomic.Int64
	releaseCount atomic.Int64
}

// NewLLOMemoryPool creates a new memory pool with optional max memory limit.
// If maxBytes is 0, no limit is enforced.
func NewLLOMemoryPool(maxBytes int64) *LLOMemoryPool {
	return &LLOMemoryPool{
		allocator: memory.NewGoAllocator(),
		maxBytes:  maxBytes,
	}
}

// ErrMemoryLimitExceeded is returned when an allocation would exceed the memory limit.
var ErrMemoryLimitExceeded = fmt.Errorf("memory limit exceeded")

// Allocate allocates a new byte slice of the given size.
// Returns nil if the allocation would exceed the configured memory limit.
func (p *LLOMemoryPool) Allocate(size int) []byte {
	if p.maxBytes > 0 && p.allocated.Load()+int64(size) > p.maxBytes {
		// Enforce memory limit - return nil to signal allocation failure.
		// Caller must handle this case appropriately.
		return nil
	}
	buf := p.allocator.Allocate(size)
	p.allocated.Add(int64(size))
	p.allocCount.Add(1)
	return buf
}

// Reallocate reallocates a byte slice to a new size.
func (p *LLOMemoryPool) Reallocate(size int, b []byte) []byte {
	oldSize := len(b)
	buf := p.allocator.Reallocate(size, b)
	p.allocated.Add(int64(size - oldSize))
	return buf
}

// Free releases a byte slice back to the pool.
func (p *LLOMemoryPool) Free(b []byte) {
	p.allocated.Add(-int64(len(b)))
	p.releaseCount.Add(1)
	p.allocator.Free(b)
}

// Metrics returns current allocation statistics.
func (p *LLOMemoryPool) Metrics() (allocated, allocs, releases int64) {
	return p.allocated.Load(), p.allocCount.Load(), p.releaseCount.Load()
}

// AllocatedBytes returns the current allocated bytes count.
func (p *LLOMemoryPool) AllocatedBytes() int64 {
	return p.allocated.Load()
}

// RecordBuilderPool manages a pool of Arrow RecordBuilders for efficient reuse.
// This reduces allocations when building Arrow records repeatedly.
type RecordBuilderPool struct {
	pool    sync.Pool
	memPool *LLOMemoryPool
	schema  *arrow.Schema
}

// NewRecordBuilderPool creates a new pool for the given schema.
func NewRecordBuilderPool(schema *arrow.Schema, memPool *LLOMemoryPool) *RecordBuilderPool {
	rbp := &RecordBuilderPool{
		memPool: memPool,
		schema:  schema,
	}
	rbp.pool = sync.Pool{
		New: func() any {
			return array.NewRecordBuilder(memPool, schema)
		},
	}
	return rbp
}

// Get retrieves a RecordBuilder from the pool.
func (p *RecordBuilderPool) Get() *array.RecordBuilder {
	return p.pool.Get().(*array.RecordBuilder)
}

// Put returns a RecordBuilder to the pool.
//
// IMPORTANT: Callers MUST ensure the builder is in a clean state before calling Put.
// This means either:
// 1. NewRecord() was called (which resets the builder), OR
// 2. No data was appended to the builder since it was retrieved
//
// Returning a builder with partial/unbalanced data will cause corruption for
// the next user. In error paths where NewRecord() wasn't called, callers should
// either call NewRecord() and release the result, or simply not return the
// builder to the pool (let it be garbage collected).
func (p *RecordBuilderPool) Put(b *array.RecordBuilder) {
	p.pool.Put(b)
}

// ArrowBuilderPool contains pools for all LLO-related Arrow schemas.
type ArrowBuilderPool struct {
	memPool          *LLOMemoryPool
	observationPool  *RecordBuilderPool
	aggregatesPool   *RecordBuilderPool
	cachePool        *RecordBuilderPool
	transmissionPool *RecordBuilderPool
}

// NewArrowBuilderPool creates a new pool for all LLO Arrow schemas.
// maxMemoryBytes sets the memory limit (0 for unlimited).
func NewArrowBuilderPool(maxMemoryBytes int64) *ArrowBuilderPool {
	memPool := NewLLOMemoryPool(maxMemoryBytes)
	return &ArrowBuilderPool{
		memPool:          memPool,
		observationPool:  NewRecordBuilderPool(ObservationSchema, memPool),
		aggregatesPool:   NewRecordBuilderPool(StreamAggregatesSchema, memPool),
		cachePool:        NewRecordBuilderPool(CacheSchema, memPool),
		transmissionPool: NewRecordBuilderPool(TransmissionSchema, memPool),
	}
}

// GetObservationBuilder returns a builder for observation records.
func (p *ArrowBuilderPool) GetObservationBuilder() *array.RecordBuilder {
	return p.observationPool.Get()
}

// PutObservationBuilder returns an observation builder to the pool.
func (p *ArrowBuilderPool) PutObservationBuilder(b *array.RecordBuilder) {
	p.observationPool.Put(b)
}

// GetAggregatesBuilder returns a builder for aggregate records.
func (p *ArrowBuilderPool) GetAggregatesBuilder() *array.RecordBuilder {
	return p.aggregatesPool.Get()
}

// PutAggregatesBuilder returns an aggregates builder to the pool.
func (p *ArrowBuilderPool) PutAggregatesBuilder(b *array.RecordBuilder) {
	p.aggregatesPool.Put(b)
}

// GetCacheBuilder returns a builder for cache records.
func (p *ArrowBuilderPool) GetCacheBuilder() *array.RecordBuilder {
	return p.cachePool.Get()
}

// PutCacheBuilder returns a cache builder to the pool.
func (p *ArrowBuilderPool) PutCacheBuilder(b *array.RecordBuilder) {
	p.cachePool.Put(b)
}

// GetTransmissionBuilder returns a builder for transmission records.
func (p *ArrowBuilderPool) GetTransmissionBuilder() *array.RecordBuilder {
	return p.transmissionPool.Get()
}

// PutTransmissionBuilder returns a transmission builder to the pool.
func (p *ArrowBuilderPool) PutTransmissionBuilder(b *array.RecordBuilder) {
	p.transmissionPool.Put(b)
}

// MemoryPool returns the underlying memory pool for direct access.
func (p *ArrowBuilderPool) MemoryPool() *LLOMemoryPool {
	return p.memPool
}

// MemoryStats returns current memory allocation statistics.
func (p *ArrowBuilderPool) MemoryStats() (allocated, allocs, releases int64) {
	return p.memPool.Metrics()
}
