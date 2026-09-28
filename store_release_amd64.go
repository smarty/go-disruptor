//go:build amd64 && !race

package disruptor

import (
	"sync/atomic"
	"unsafe"
)

// storeRelease stores value with release semantics, which is all that publishing committed slots requires: the
// caller's earlier ring-buffer writes become visible before the stored sequence does. On amd64 (TSO) a plain MOVQ is
// a release store, unlike the XCHG that sync/atomic's sequentially consistent Store compiles to.
//
// The hardware keeps the order; the compiler must not break it. This is only safe inside a function that is never
// inlined into its caller (see defaultSequencer.Commit, which is //go:noinline): the compiler treats a call it cannot
// see into as reading and writing all memory, so the caller's ring-buffer writes cannot move past it. Inlined into the
// caller (PGO would do so for a hot Commit), they could. It inlines into Commit itself, so no call is paid for it: on
// the Threadripper 3970X, calling the earlier one-instruction assembly stub (ABI0: stack arguments, a frame, and the
// X15/R14 restore) cost more than the XCHG it replaced.
//
// Race builds use sync/atomic instead (store_release_atomic.go), because the race detector models synchronization
// only through instrumented sync/atomic calls and would report this store as a data race.
func storeRelease(address *atomic.Int64, value int64) {
	*(*int64)(unsafe.Pointer(address)) = value // atomic.Int64's only non-zero-size field is its int64 value
}
