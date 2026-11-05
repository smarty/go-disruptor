package disruptor

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// ParkingWaitStrategy spins, then yields, then parks an idle listener until a producer commits or Timeout passes.
//
// An idle or gated Listener calls Idle or Gate for its first Spins+Yields empty iterations, then parks on a channel
// that every Commit, every advance of a Handler group, and Close wake. An idle Disruptor therefore uses almost no CPU,
// and a parked Listener wakes as soon as the runtime schedules it, not after a sleep. Producers pay one atomic add per
// Commit when no Listener is parked, and do not park themselves: Reserve on a full ring spins, yields, and then
// sleeps for ReserveSleep at a time. Timeout is only a safety net; no wake-up depends on it.
type ParkingWaitStrategy struct {
	Spins        int64         // Idle calls that spin before yielding; default 64
	Yields       int64         // Idle calls that yield before parking; default 16
	Timeout      time.Duration // the longest a listener stays parked without a notify; default 1ms
	ReserveSleep time.Duration // a producer's sleep on a full ring after Spins+Yields tries; default 50us
}

func (this ParkingWaitStrategy) Gate(count int64) { this.Idle(count) }
func (this ParkingWaitStrategy) Idle(count int64) {
	if count <= this.Spins {
		spinHint()
	} else {
		runtime.Gosched()
	}
}
func (this ParkingWaitStrategy) Reserve(count int64) {
	if count <= this.Spins {
		spinHint()
	} else if count <= this.Spins+this.Yields {
		runtime.Gosched()
	} else {
		time.Sleep(this.ReserveSleep)
	}
}

// withDefaults returns a copy with every zero field set to its default.
func (this ParkingWaitStrategy) withDefaults() ParkingWaitStrategy {
	if this.Spins == 0 {
		this.Spins = 64
	}
	if this.Yields == 0 {
		this.Yields = 16
	}
	if this.Timeout == 0 {
		this.Timeout = time.Millisecond
	}
	if this.ReserveSleep == 0 {
		this.ReserveSleep = 50 * time.Microsecond
	}
	return this
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// parker is shared by every Listener of one Disruptor. A Listener parks in three steps: prepare, a re-check of its
// barrier and running flag, then wait (or cancel, if the re-check found a change). Every party that can end a park
// (a Commit, a Listener storing its handled sequence, Close) first stores its change and then calls notify. The fields
// are as follows:
//
//   - waiters: the number of Listeners between prepare and the end of wait (or cancel). notify returns at once when
//     it is zero, so producers pay one atomic add per Commit while no Listener is parked.
//
//   - epoch: incremented by every notify that finds a waiter. wait returns at once when the epoch moved after
//     prepare, because the notify that moved it may have closed a signal channel that this Listener never saw.
//
//   - mutex: guards signal, and orders each notify's epoch increment and close with each wait's reads of them.
//
//   - signal: closed by notify to wake every parked Listener at once, then replaced.
//
//   - threshold: the number of empty iterations (Spins+Yields) after which a Listener parks.
//
//   - timeout: the longest a Listener stays parked without a notify.
type parker struct {
	waiters   atomic.Int32
	epoch     atomic.Uint64
	mutex     sync.Mutex
	signal    chan struct{}
	threshold int64
	timeout   time.Duration
}

func newParker(strategy ParkingWaitStrategy) *parker {
	strategy = strategy.withDefaults()
	return &parker{
		signal:    make(chan struct{}),
		threshold: strategy.Spins + strategy.Yields,
		timeout:   strategy.Timeout,
	}
}

// prepare announces a waiter before the caller re-checks its barrier. The add is a seq-cst read-modify-write, so the
// re-check cannot be reordered before it.
func (this *parker) prepare() (ticket uint64) {
	this.waiters.Add(1)
	return this.epoch.Load()
}
func (this *parker) cancel() {
	this.waiters.Add(-1)
}
func (this *parker) wait(ticket uint64, timer *time.Timer) {
	this.mutex.Lock()
	signal := this.signal
	moved := this.epoch.Load() != ticket
	this.mutex.Unlock()

	if !moved {
		timer.Reset(this.timeout)
		select {
		case <-signal:
		case <-timer.C:
		}
		timer.Stop()
	}

	this.waiters.Add(-1)
}

// notify wakes every parked Listener. The caller has just stored its change (a commit, a handled sequence, or the
// closed state) and the parked side has just added itself to waiters and will then re-read that change: this is
// Dekker's pattern, so each side needs a full store-then-load fence. On amd64 without -race, Commit publishes with
// storeRelease (a plain MOVQ), which x86 may reorder after a later plain load such as waiters.Load(); the
// read-modify-write waiters.Add(0) is a LOCK XADD, a full fence. Either notify sees the waiter, or the waiter's
// re-check sees the change. Do not replace it with waiters.Load().
func (this *parker) notify() {
	if this.waiters.Add(0) == 0 {
		return
	}

	this.mutex.Lock()
	this.epoch.Add(1)
	close(this.signal)
	this.signal = make(chan struct{})
	this.mutex.Unlock()
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// notifyingSequencer wakes parked Listeners after each Commit. New wraps the Sequencer in it only for the
// ParkingWaitStrategy, so the sequencers themselves do not change.
type notifyingSequencer struct {
	Sequencer
	parker *parker
}

func (this *notifyingSequencer) Commit(lower, upper int64) {
	this.Sequencer.Commit(lower, upper)
	this.parker.notify()
}
