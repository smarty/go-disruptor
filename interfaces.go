package disruptor

import "io"

// Disruptor is the top-level container combining a Sequencer (producer API) with a ListenCloser (consumer
// lifecycle). Created via invocations of New(...option).
type Disruptor interface {
	Sequencer
	ListenCloser
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// ListenCloser processes events from the ring buffer. Listen blocks the calling goroutine until the listener is closed.
type ListenCloser interface {
	Listen()
	io.Closer
}

// WaitStrategy provides pluggable backpressure for both producers and consumers. The default implementation uses
// runtime.Gosched for Gate, time.Sleep(500ns) for Idle, and time.Sleep(1ns) for Reserve.
type WaitStrategy interface {
	// Gate is invoked when data has been committed to the ring buffer by a producer but the prior Handler group has
	// not yet finished processing it. This means new work is imminent and the Listener should wait briefly.
	Gate(int64)
	// Idle is invoked when all meaningful work has been completed and there are no slots with available messages.
	Idle(int64)
	// Reserve is invoked by the Sequencer when there are no available slots in the underlying ring buffer.
	Reserve(int64)
}

// Handler is the consumer callback invoked by a Listener with each batch of available sequences from the ring buffer.
type Handler interface {
	Handle(lowerSequence, upperSequence int64)
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// The Sequencer tracks the state of a given "writer" or producer to the ring buffer. It is the heart of the Disruptor
// pattern. When a caller desires to push events to the ring buffer, the caller or producer must first Reserve the
// desired slots on the ring buffer using the Sequencer. After obtaining a reservation on certain slots of the ring
// buffer, the caller must write the desired data to the reserved slots of the associated ring buffer and then
// indicate the completion of the operation(s) by calling Commit which makes the data in the written slots available
// to any downstream Handler instances which then handle or otherwise consume events from the ring buffer on different
// goroutines.
type Sequencer interface {

	// Reserve claims the desired number of slots in the ring buffer for the caller. When those slots become available
	// because any configured Handlers have properly processed all necessary data in those slots, the Sequencer returns
	// the uppermost or highest sequence of the slots claimed and reserved for the caller.
	//
	// The lower-bound sequence in the ring buffer is obtained by subtracting the specified number of slots from the
	// uppermost sequence returned. If the number of desired slots is larger than the capacity of the ring buffer,
	// ErrReservationSize is returned.
	//
	// Each successful call to Reserve should *always* be followed by a single call to Commit.
	Reserve(slots uint32) (upperSequence int64)

	// TryReserve attempts a single non-blocking reservation of the desired number of slots. If the slots are
	// immediately available, the uppermost sequence is returned. If the ring buffer has insufficient capacity
	// because consumers have not yet advanced far enough, ErrCapacityUnavailable is returned without waiting.
	// For the shared Sequencer, this uses CAS rather than atomic Add; a CAS lost to a concurrent writer is retried
	// (it signals contention, not a full ring buffer), so ErrCapacityUnavailable always means insufficient capacity.
	//
	// Each successful call to TryReserve should *always* be followed by a single call to Commit.
	TryReserve(slots uint32) (upperSequence int64)

	// Commit indicates to the Sequencer that the previously claimed slots in the ring buffer have been written to
	// successfully and that the data is now available to any configured Handler instances to process.
	//
	// Each successful call to Commit should *always* be preceded by a single successful call to Reserve.
	Commit(lowerSequence, upperSequence int64)
}

const (
	// ErrReservationSize indicates that the reservation requested is nonsensical, e.g. lower > upper OR that the
	// desired reservation size exceeds the capacity of the ring buffer altogether.
	ErrReservationSize = -1
	// ErrCapacityUnavailable indicates that TryReserve could not claim the requested slots because consumers
	// have not yet advanced far enough and the ring buffer has insufficient capacity.
	ErrCapacityUnavailable = -2
)
