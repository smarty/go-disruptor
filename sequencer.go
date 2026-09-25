package disruptor

// defaultSequencer is a single-writer Sequencer which "owns" writes to an associated ring buffer. An instance of
// this Sequencer should not be shared among separate goroutines without explicit synchronization. The fields are as
// follows:
//
//   - capacity: the total number of slots in the ring buffer, always a power of 2.
//
//   - reservedSequence: the highest sequence value that has been claimed or reserved by this producer. The field
//     is eagerly incremented at the start of Reserve before the wrap check, so it always reflects the most
//     optimistic position. The field is initialized to -1 so that the first Reserve(1) yields sequence 0--the very
//     beginning of the ring buffer.
//
//   - cachedConsumerSequence: a locally cached snapshot of the slowest consumer's sequence position. The value is
//     checked on every call to Reserve in order to avoid reading the more expensive consumerBarrier field when no
//     wrap contention exists (the fast path). The value is only refreshed and updated from the synchronized
//     consumerBarrier field during the slow-path spin loop.
//
//   - committedSequence: a synchronized sequence value which indicates to downstream consumers how far the producer
//     has committed. The value is updated when Commit is called via a single atomic operation. This atomic
//     operation acts as the appropriate memory fence which guarantees that changes to the contents of the underlying
//     ring buffer are visible to downstream consumers.
//
//   - consumerBarrier: a barrier or group of sequences used to determine the slowest sequence position across all
//     downstream consumers. The value is only read during the slow-path spin loop when the producer has detected
//     possible overwrite contention which means the Sequencer must wait for associated consumers to advance before
//     allowing the desired number of slots to be reserved.
//
//   - waiter: the WaitStrategy used during the slow-path spin loop. Its Reserve method is called on each iteration
//     while waiting for consumers to advance.
type defaultSequencer struct {
	_                      [4]byte         // 4B  — explicit alignment padding
	capacity               uint32          // 4B  — read every Reserve
	reservedSequence       int64           // 8B  — read+write every Reserve
	cachedConsumerSequence int64           // 8B  — read every Reserve
	committedSequence      *atomicSequence // 8B  — written every Commit
	consumerBarrier        sequenceBarrier // 16B — slow path
	waiter                 WaitStrategy    // 16B — slow path
} // 64B total — fills a single 64B cache line

func newSequencer(capacity uint32, committedSequence *atomicSequence, consumerBarrier sequenceBarrier, waiter WaitStrategy) Sequencer {
	return &defaultSequencer{
		capacity:               capacity,
		reservedSequence:       defaultSequenceValue,
		cachedConsumerSequence: defaultSequenceValue,
		committedSequence:      committedSequence,
		consumerBarrier:        consumerBarrier,
		waiter:                 waiter,
	}
}

func (this *defaultSequencer) Reserve(count uint32) int64 {
	if count == 0 || count > this.capacity {
		return ErrReservationSize
	}

	this.reservedSequence += int64(count)
	if minimumSequence := this.reservedSequence - int64(this.capacity); minimumSequence > this.cachedConsumerSequence {
		this.waitForConsumers(minimumSequence) // slow path
	}

	return this.reservedSequence
}
func (this *defaultSequencer) waitForConsumers(minimumSequence int64) {
	for spin := int64(0); ; spin++ {
		if this.cachedConsumerSequence = this.consumerBarrier.Load(0); minimumSequence <= this.cachedConsumerSequence {
			return
		}
		this.waiter.Reserve(spin)
	}
}
func (this *defaultSequencer) TryReserve(count uint32) int64 {
	if count == 0 || count > this.capacity {
		return ErrReservationSize
	}

	// fast path
	this.reservedSequence += int64(count)
	minimumSequence := this.reservedSequence - int64(this.capacity)
	if minimumSequence <= this.cachedConsumerSequence {
		return this.reservedSequence
	}

	// slow path
	this.cachedConsumerSequence = this.consumerBarrier.Load(0)
	if minimumSequence > this.cachedConsumerSequence {
		this.reservedSequence -= int64(count)
		return ErrCapacityUnavailable
	}

	return this.reservedSequence
}
func (this *defaultSequencer) Commit(_, upper int64) { this.committedSequence.Store(upper) }
