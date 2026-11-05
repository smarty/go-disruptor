package disruptor

import (
	"sync/atomic"
	"time"
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
//     committed. When gated, the value is read on each empty iteration to detect when data has been written but the
//     prior Handler group has not yet advanced. This means that this Listener is in a "gated" state and that new work
//     is imminent. It is also re-read after observing Close, so that commits preceding Close are drained.
//
//   - upstreamBarrier: a barrier representing the minimum sequence position across the prior Handler group. The
//     value is read on each iteration to determine how far this Listener is allowed to advance. If this instance is
//     part of the first Handler group, the value points directly at the committed barrier of the configured
//     Sequencer.
//
//   - gated: whether a prior Handler group exists, i.e. whether upstreamBarrier differs from committedBarrier. For the
//     first Handler group both are the same barrier, so the gated check would only re-read the barrier that was just
//     found empty; skipping it saves a second barrier read (a scan of committedSlots in the shared sequencer) on every
//     empty iteration, and avoids a needless Gate when a commit lands between the two reads.
//
//   - waiter: the WaitStrategy whose Gate and Idle methods are called during the gated and idle states
//     respectively.
//
//   - parker: shared by every Listener of a ParkingWaitStrategy Disruptor, and nil otherwise. After more than its
//     threshold of consecutive gated or idle iterations, the Listener parks on it instead of calling the waiter. The
//     Listener also notifies it after each handled batch (to wake a gated downstream group) and on Close.
//
//   - timer: the park timeout timer, created on the first park and reused.
//
//   - handler: the Handler whose Handle method is called when each batch of sequence values is ready to be
//     processed.
type defaultListener struct {
	running          *atomic.Int64
	handledSequence  *atomicSequence
	committedBarrier sequenceBarrier
	upstreamBarrier  sequenceBarrier
	gated            bool
	waiter           WaitStrategy
	parker           *parker
	timer            *time.Timer
	handler          Handler
}

func newListener(handledSequence *atomicSequence, committedBarrier, upstreamBarrier sequenceBarrier, gated bool, waiter WaitStrategy, parker *parker, handler Handler) ListenCloser {
	return &defaultListener{
		running:          &atomic.Int64{},
		handledSequence:  handledSequence,
		committedBarrier: committedBarrier,
		upstreamBarrier:  upstreamBarrier,
		gated:            gated,
		waiter:           waiter,
		parker:           parker,
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
			if this.parker != nil {
				this.parker.notify() // the Store is seq-cst, so no fence is needed before notify's load
			}
			handledSequence = upperSequence
			gatedCount = 0
			idlingCount = 0
		} else if this.gated && lowerSequence <= this.committedBarrier.Load(lowerSequence) {
			gatedCount++
			idlingCount = 0
			if this.parker != nil && gatedCount > this.parker.threshold {
				this.park(lowerSequence, false)
			} else {
				this.waiter.Gate(gatedCount)
			}
		} else if this.running.Load() == stateRunning {
			idlingCount++
			gatedCount = 0
			if this.parker != nil && idlingCount > this.parker.threshold {
				this.park(lowerSequence, true)
			} else {
				this.waiter.Idle(idlingCount)
			}
		} else if upperSequence = this.committedBarrier.Load(lowerSequence); lowerSequence > upperSequence {
			// Close was observed only *after* the barriers were read above (the upstream barrier, which is the committed
			// barrier for the first group, and the committed barrier when gated), so any Commit that preceded Close may
			// have landed in between; re-checking here guarantees those events are drained, not dropped.
			break
		}
	}
}

// park waits until a notify, unless the upstream barrier advanced past lowerSequence (or, when idle, Close was called)
// after prepare: the re-check after prepare is what makes a notify impossible to miss. A gated Listener does not
// re-check the running flag, because Close does not change what it waits for, and cancelling on it would spin.
func (this *defaultListener) park(lowerSequence int64, idle bool) {
	ticket := this.parker.prepare()
	if lowerSequence <= this.upstreamBarrier.Load(lowerSequence) || (idle && this.running.Load() != stateRunning) {
		this.parker.cancel()
		return
	}

	if this.timer == nil {
		this.timer = time.NewTimer(this.parker.timeout)
	}
	this.parker.wait(ticket, this.timer)
}

func (this *defaultListener) Close() error {
	this.running.Store(stateClosed)
	if this.parker != nil {
		this.parker.notify() // the Store is seq-cst, so no fence is needed before notify's load
	}
	return nil
}

const (
	stateRunning = 0
	stateClosed  = 1
)
