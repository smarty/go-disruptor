package disruptor

import (
	"sync/atomic"
	"unsafe"
)

// atomicSequence is a cache-line-padded atomic int64 used to track sequence positions without false sharing.
type atomicSequence struct {
	_ [CacheLineBytes - unsafe.Sizeof(int64(0))]byte
	atomic.Int64
}

func newSequence() *atomicSequence {
	return newSequences(1)[0]
}

// newSequences allocates a contiguous, cache-line-aligned slice of *atomicSequence. Rather than relying upon the Go
// allocator's size classes to happen to produce an aligned address (which, for example, does not hold for some counts
// on s390x with 256B cache lines), one extra cache line is over-allocated and the sequences begin at the first aligned
// offset within it. The backing array contains no pointers (noscan) and Go's GC does not move heap objects, so the
// alignment is stable for the life of the allocation, which is kept alive by the returned pointers into it.
func newSequences(count int) []*atomicSequence {
	backing := make([]byte, (count+1)*CacheLineBytes) // single, contiguous allocation
	offset := (CacheLineBytes - uintptr(unsafe.Pointer(&backing[0]))%CacheLineBytes) % CacheLineBytes
	contiguous := unsafe.Slice((*atomicSequence)(unsafe.Pointer(&backing[offset])), count)

	// guaranteed cache alignment of underlying sequence values which are *contiguous*
	this := make([]*atomicSequence, count)
	for i := range this {
		this[i] = &contiguous[i]
		this[i].Store(defaultSequenceValue)
	}
	return this
}

const defaultSequenceValue = -1
