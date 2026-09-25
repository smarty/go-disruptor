package disruptor

import (
	"math/bits"
	"sync/atomic"
)

// sharedSequencer is a multi-writer Sequencer that allows multiple goroutines to concurrently reserve slots in the
// same ring buffer. Unlike defaultSequencer, it is goroutine-safe for concurrent producers. The struct occupies two
// cache lines: the first for the hot path (Reserve/Commit/Load), the second for the slow path only. The fields are
// as follows:
//
//   - reservedSequence: a shared atomicSequence representing the highest sequence value that has been claimed across
//     all producers. Incremented via atomic Add on every Reserve call. Also read by Load to determine the upper
//     bound of potentially committed data.
//
//   - cachedConsumerSequence: an atomic cache of the slowest consumer's sequence position. Checked on every Reserve
//     to avoid reading the more expensive consumerBarrier when no wrap contention exists (the fast path). Unlike
//     defaultSequencer, this is an atomicSequence (not a plain int64) because multiple writers may update it
//     concurrently. Further, the value here is merely a cache and will almost certainly be clobbered or overwritten
//     by multiple writers reserving sequences in the ring buffer. But checking this cached value is significantly
//     less expensive than checking the consumerBarrier field.
//
//   - committedSlots: a per-slot commit status array indexed by sequence & mask. Each entry stores the "round"
//     (sequence >> shift) to indicate that a specific slot has been committed. Commit writes slots from lowest to
//     highest, and Load reads in the same direction, stopping at the first uncommitted slot to find the highest
//     contiguously committed sequence. The slice header lives on cache line 1; the backing array is allocated
//     separately but in contiguous memory.
//
//   - capacity: the total number of slots in the ring buffer, always a power of 2.
//
//   - shift: log2(capacity), used to compute the round number stored in committedSlots on every Commit and Load.
//     When a producer commits sequence s, it stores int32(s >> shift) at committedSlots[s & mask]. This lets Load
//     verify that a slot was committed for the correct lap around the ring buffer, not a stale value from a
//     previous lap of the ring buffer.
//
//   - consumerBarrier: a barrier used to determine the slowest sequence position across all downstream consumers.
//     Only read during the slow-path spin loop when a producer has detected possible overwrite contention.
//
//   - waiter: the WaitStrategy used during the slow-path spin loop. Its Reserve method is called on each iteration
//     while waiting for consumers to advance.
type sharedSequencer struct {
	// cache line 1 — hot path (Reserve/Commit/Load)
	reservedSequence       *atomicSequence // 8B  — atomic Add every Reserve; read every Load
	cachedConsumerSequence *atomicSequence // 8B  — read every Reserve (wrap check)
	committedSlots         []atomic.Int32  // 24B — Store every Commit; scanned every Load (slice header)
	capacity               uint32          // 4B  — buffer capacity (power of 2)
	shift                  uint8           // 1B  — read every Commit and Load
	_                      [19]byte        // 19B — padding to 64B boundary

	// cache line 2 — slow path only
	consumerBarrier sequenceBarrier // 16B — slow path
	waiter          WaitStrategy    // 16B — slow path
	_               [32]byte        // 32B — tail padding
} // 128B total — fills two 64B cache lines

func newSharedSequencer(capacity uint32, reservedSequence *atomicSequence, waiter WaitStrategy) *sharedSequencer {
	committedSlots := make([]atomic.Int32, capacity)
	for i := range committedSlots {
		committedSlots[i].Store(defaultSequenceValue)
	}

	return &sharedSequencer{
		reservedSequence:       reservedSequence,
		cachedConsumerSequence: newSequence(),
		shift:                  uint8(bits.TrailingZeros32(capacity)),
		capacity:               capacity,
		committedSlots:         committedSlots,
		waiter:                 waiter,
	}
}

func (this *sharedSequencer) Reserve(count uint32) int64 {
	if count == 0 || count > this.capacity {
		return ErrReservationSize
	}

	reservedSequence := this.reservedSequence.Add(int64(count)) // claims the slot for the caller (not using CAS operation)
	if minimumSequence := reservedSequence - int64(this.capacity); minimumSequence > this.cachedConsumerSequence.Load() {
		this.waitForConsumers(minimumSequence) // slow path
	}

	return reservedSequence
}
func (this *sharedSequencer) waitForConsumers(minimumSequence int64) {
	for spin := int64(0); ; spin++ {
		if consumerSequence := this.consumerBarrier.Load(0); minimumSequence <= consumerSequence {
			// The cachedConsumerSequence field may be overwritten by multiple writers. It's only useful for helping
			// prevent execution of the slow path. In a worst-case scenario, the value is behind and the slow path is
			// traversed.
			this.cachedConsumerSequence.Store(consumerSequence)
			return
		}
		this.waiter.Reserve(spin)
	}
}

func (this *sharedSequencer) TryReserve(count uint32) int64 {
	if count == 0 || count > this.capacity {
		return ErrReservationSize
	}

	// A failed CAS means another writer claimed slots first (i.e. contention), not that the ring buffer is full, so the
	// capacity is re-evaluated and the claim retried. The loop is lock-free: every failed CAS means some other writer
	// made progress, and the loop exits with ErrCapacityUnavailable as soon as capacity is genuinely exhausted.
	for slots := int64(count); ; {
		previousReservedSequence := this.reservedSequence.Load()
		if !this.hasAvailableCapacity(previousReservedSequence, slots) {
			return ErrCapacityUnavailable
		}

		if this.reservedSequence.CompareAndSwap(previousReservedSequence, previousReservedSequence+slots) {
			return previousReservedSequence + slots
		}
	}
}
func (this *sharedSequencer) hasAvailableCapacity(previousReservedSequence, count int64) bool {
	var (
		reservedSequence = previousReservedSequence + count
		minimumSequence  = reservedSequence - int64(this.capacity)
		consumerSequence = this.cachedConsumerSequence.Load()
	)

	// fast path
	if minimumSequence <= consumerSequence {
		return true
	}

	// slow path
	consumerSequence = this.consumerBarrier.Load(0)
	this.cachedConsumerSequence.Store(consumerSequence) // see notes above for cachedConsumerSequence field
	return minimumSequence <= consumerSequence
}

func (this *sharedSequencer) Commit(lower, upper int64) {
	for mask := int64(this.capacity) - 1; lower <= upper; lower++ {
		this.committedSlots[lower&mask].Store(int32(lower >> this.shift)) // see notes above for shift field
	}
}

func (this *sharedSequencer) Load(lower int64) int64 {
	upper := this.reservedSequence.Load()

	for mask := int64(this.capacity) - 1; lower <= upper; lower++ {
		if this.committedSlots[lower&mask].Load() != int32(lower>>this.shift) {
			return lower - 1
		}
	}

	return upper
}
