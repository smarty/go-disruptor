package disruptor

import "sync/atomic"

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
//   - committedSlots: a commit marker array indexed by sequence & mask. Commit(lower, upper) stores a single marker,
//     the batch's upper sequence, in the slot of the batch's *first* sequence; the batch's remaining slots are not
//     written. Load starts at a batch boundary and jumps from marker to marker (to marker+1), stopping at the first
//     slot whose marker is less than the sequence being examined, to find the highest contiguously committed
//     sequence. This costs one atomic store per batch in Commit and one atomic load per batch in Load, rather than
//     one per slot. A slot examined for sequence s holds either this lap's marker for the batch beginning at s (which
//     is >= s), or a stale marker from any earlier lap, or the initial -1. A stale marker belongs to a batch that began
//     at s-k*capacity (k >= 1) and was at most capacity long, so it is always < s; because sequences are int64 and
//     never wrap, no stale marker can ever be mistaken for a current one, however many laps it survives unwritten.
//
//     Correctness relies on Load only ever being called with a lower bound that is the first sequence of a batch.
//     That holds because every caller passes handledSequence+1, and every handledSequence is either the initial -1
//     or a value previously returned by Load (directly, or as the minimum across a handler group), all of which are
//     the last sequence of some batch. The slice header lives on cache line 1; the backing array is allocated
//     separately but in contiguous memory.
//
//   - capacity: the total number of slots in the ring buffer, always a power of 2.
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
	committedSlots         []atomic.Int64  // 24B — Store every Commit; scanned every Load (slice header)
	capacity               uint32          // 4B  — buffer capacity (power of 2)
	_                      [20]byte        // 20B — padding to 64B boundary

	// cache line 2 — slow path only
	consumerBarrier sequenceBarrier // 16B — slow path
	waiter          WaitStrategy    // 16B — slow path
	_               [32]byte        // 32B — tail padding
} // 128B total — fills two 64B cache lines

func newSharedSequencer(capacity uint32, reservedSequence *atomicSequence, waiter WaitStrategy) *sharedSequencer {
	committedSlots := make([]atomic.Int64, capacity)
	for i := range committedSlots {
		committedSlots[i].Store(defaultSequenceValue)
	}

	return &sharedSequencer{
		reservedSequence:       reservedSequence,
		cachedConsumerSequence: newSequence(),
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
	if lower > upper {
		return // an empty range would overwrite (and hide) the marker of a batch already committed at lower
	}

	this.committedSlots[lower&(int64(this.capacity)-1)].Store(upper) // see notes above for committedSlots field
}

func (this *sharedSequencer) Load(lower int64) int64 {
	// A batch reserved after this read begins beyond upper, and a batch beginning at or before upper was reserved (in
	// its entirety) before this read, so jumping by whole batches can never overshoot upper.
	upper := this.reservedSequence.Load()

	for mask := int64(this.capacity) - 1; lower <= upper; {
		// A single-slot batch advances via lower++ behind a (predictable) branch rather than lower = marker+1: that
		// turns the data dependency upon the loaded marker into a control dependency, so the CPU speculatively issues
		// the next slot's load without waiting for this one. As a pure data dependency, each load must complete
		// (often a cross-core transfer) before the next can begin, which made single-slot batches ~6% slower.
		marker := this.committedSlots[lower&mask].Load()
		if marker < lower {
			break // uncommitted: a stale marker from an earlier lap (or the initial -1)
		} else if marker == lower {
			lower++
		} else {
			lower = marker + 1 // skip to the first sequence of the next batch
		}
	}

	return lower - 1
}
