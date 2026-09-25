package disruptor

import (
	"sync/atomic"
)

// defaultListener is not goroutine-safe and is designed to run on a single goroutine. It tracks which slots or
// events in the associated ring buffer have been read, processed, or handled in some manner. The fields are as
// follows:
//
//   - running: an atomic flag indicating whether the listener is running (0) or closed (1). Checked on each
//     iteration of the listen loop to determine when to exit.
//
//   - handledSequence: the sequence position up to which this listener's Handler has processed. Updated after each
//     successful call to Handle, making the listener's progress visible as part of a larger barrier (group of
//     sequence values) to other downstream parties--either a group of Listener instances or looping back to the
//     configured Sequencer.
//
//   - committedBarrier: a barrier representing how far all producers (via the configured Sequencer instance) have
//     committed. The value is read on each iteration to detect when data has been written but the prior Handler
//     group has not yet advanced. This means that this Listener is in a "gated" state and that new work is
//     imminent.
//
//   - upstreamBarrier: a barrier representing the minimum sequence position across the prior Handler group. The
//     value is read on each iteration to determine how far this Listener is allowed to advance. If this instance is
//     part of the first Handler group, the value points directly at the committed barrier of the configured
//     Sequencer.
//
//   - waiter: the WaitStrategy whose Gate and Idle methods are called during the gated and idle states
//     respectively.
//
//   - handler: the Handler whose Handle method is called when each batch of sequence values is ready to be
//     processed.
type defaultListener struct {
	running          *atomic.Int64
	handledSequence  *atomicSequence
	committedBarrier sequenceBarrier
	upstreamBarrier  sequenceBarrier
	waiter           WaitStrategy
	handler          Handler
}

func newListener(handledSequence *atomicSequence, committedBarrier, upstreamBarrier sequenceBarrier, waiter WaitStrategy, handler Handler) ListenCloser {
	return &defaultListener{
		running:          &atomic.Int64{},
		handledSequence:  handledSequence,
		committedBarrier: committedBarrier,
		upstreamBarrier:  upstreamBarrier,
		waiter:           waiter,
		handler:          handler,
	}
}

func (this *defaultListener) Listen() {
	var gatedCount, idlingCount, lowerSequence, upperSequence int64
	var handledSequence = this.handledSequence.Load()

	for {
		lowerSequence = handledSequence + 1
		upperSequence = this.upstreamBarrier.Load(lowerSequence)

		if lowerSequence <= upperSequence {
			this.handler.Handle(lowerSequence, upperSequence)
			this.handledSequence.Store(upperSequence)
			handledSequence = upperSequence
			gatedCount = 0
			idlingCount = 0
		} else if upperSequence = this.committedBarrier.Load(lowerSequence); lowerSequence <= upperSequence {
			gatedCount++
			idlingCount = 0
			this.waiter.Gate(gatedCount)
		} else if this.running.Load() == stateRunning {
			idlingCount++
			gatedCount = 0
			this.waiter.Idle(idlingCount)
		} else if upperSequence = this.committedBarrier.Load(lowerSequence); lowerSequence > upperSequence {
			// Close was observed only *after* the committed barrier was checked above, so any Commit that preceded
			// Close may have landed in between; re-checking here guarantees those events are drained, not dropped.
			break
		}
	}
}

func (this *defaultListener) Close() error {
	this.running.Store(stateClosed)
	return nil
}

const (
	stateRunning = 0
	stateClosed  = 1
)
