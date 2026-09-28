package disruptor

import (
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestNew_ZeroCapacity(t *testing.T) {
	_, err := New(Options.BufferCapacity(0), Options.NewHandlerGroup(nopHandler{}))
	if err == nil {
		t.Fatal("expected error for zero capacity")
	}
}
func TestNew_NonPowerOfTwoCapacity(t *testing.T) {
	_, err := New(Options.BufferCapacity(3), Options.NewHandlerGroup(nopHandler{}))
	if err == nil {
		t.Fatal("expected error for non-power-of-two capacity")
	}
}
func TestNew_MissingCapacity(t *testing.T) {
	_, err := New(Options.NewHandlerGroup(nopHandler{}))
	if err == nil {
		t.Fatal("expected error when no capacity is provided")
	}
}
func TestNew_NoHandlers(t *testing.T) {
	_, err := New(Options.BufferCapacity(1024))
	if err == nil {
		t.Fatal("expected error for no handlers")
	}
}
func TestNew_NilHandlersFiltered(t *testing.T) {
	_, err := New(Options.BufferCapacity(1024), Options.NewHandlerGroup(nil, nil))
	if err == nil {
		t.Fatal("expected error when all handlers are nil")
	}
}
func TestNew_TypedNilHandlersFiltered(t *testing.T) {
	var nilPointer *pointerHandler
	var nilFunc handlerFunc
	_, err := New(Options.BufferCapacity(1024), Options.NewHandlerGroup(nilPointer, nilFunc))
	if err == nil {
		t.Fatal("expected error when all handlers are typed nils")
	}
}

func TestListen_DrainsCommitRacingWithClose(t *testing.T) {
	// Deterministically forces the interleaving: the listener reads the committed barrier (nothing yet), then the
	// producer commits sequence 0 and calls Close, and only then does the listener read the running flag.
	committed := newSequence()
	handled := int64(defaultSequenceValue)
	var listener ListenCloser
	barrier := &racingCommitBarrier{committed: committed, listener: &listener}
	listener = newListener(newSequence(), barrier, newAtomicBarrier(committed), true, defaultWaitStrategy{}, recordingHandler{handled: &handled})

	listener.Listen()

	if handled != 0 {
		t.Fatalf("sequence 0 was committed before Close but never handled; handled=%d", handled)
	}
}
func TestListen_FirstGroupDrainsCommitRacingWithClose(t *testing.T) {
	// The first handler group reads one barrier as both upstream and committed, and skips the gated check: the
	// producer commits sequence 0 and calls Close right after that single read, before the running flag is read.
	committed := newSequence()
	handled := int64(defaultSequenceValue)
	var listener ListenCloser
	barrier := &racingCommitBarrier{committed: committed, listener: &listener}
	listener = newListener(newSequence(), barrier, barrier, false, defaultWaitStrategy{}, recordingHandler{handled: &handled})

	listener.Listen()

	if handled != 0 {
		t.Fatalf("sequence 0 was committed before Close but never handled; handled=%d", handled)
	}
}
func TestListen_OnlyGatedListenersCallGate(t *testing.T) {
	// A commit lands right after the listener's first barrier read. A gated listener sees it on its committed-barrier
	// check and calls Gate; the first group has no upstream group to wait for, so it must go straight on to Idle.
	for _, gated := range []bool{true, false} {
		committed := newSequence()
		handled := int64(defaultSequenceValue)
		var listener ListenCloser
		barrier := &committingBarrier{committed: committed}
		waiter := &closingWaitStrategy{listener: &listener}
		listener = newListener(newSequence(), barrier, barrier, gated, waiter, recordingHandler{handled: &handled})

		listener.Listen()

		if expected := map[bool]int{true: 1, false: 0}[gated]; waiter.gates != expected {
			t.Fatalf("gated=%v: expected %d Gate calls, got %d", gated, expected, waiter.gates)
		}
		if handled != 0 {
			t.Fatalf("gated=%v: sequence 0 was never handled; handled=%d", gated, handled)
		}
	}
}

func TestNewSequences_CacheAligned(t *testing.T) {
	for count := 1; count <= 256; count++ {
		for index, sequence := range newSequences(count) {
			if address := uintptr(unsafe.Pointer(sequence)); address%CacheLineBytes != 0 {
				t.Fatalf("count=%d index=%d: address %#x is not aligned to %d bytes", count, index, address, CacheLineBytes)
			}
			if sequence.Load() != defaultSequenceValue {
				t.Fatalf("count=%d index=%d: expected initial value %d, got %d", count, index, defaultSequenceValue, sequence.Load())
			}
		}
	}
}

func TestReserve_ZeroSlots(t *testing.T) {
	d := newTestDisruptor(t, 1)
	if result := d.Reserve(0); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}
func TestReserve_ExceedsCapacity(t *testing.T) {
	d := newTestDisruptor(t, 1)
	if result := d.Reserve(2048); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}
func TestSharedReserve_ZeroSlots(t *testing.T) {
	d := newTestDisruptor(t, 2)
	if result := d.Reserve(0); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}
func TestSharedReserve_ExceedsCapacity(t *testing.T) {
	d := newTestDisruptor(t, 2)
	if result := d.Reserve(2048); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}

func TestTryReserve_ZeroSlots(t *testing.T) {
	d := newTestDisruptor(t, 1)
	if result := d.TryReserve(0); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}
func TestTryReserve_ExceedsCapacity(t *testing.T) {
	d := newTestDisruptor(t, 1)
	if result := d.TryReserve(2048); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}
func TestTryReserve_CapacityUnavailable(t *testing.T) {
	d := newTestDisruptor(t, 1)
	// fill the ring buffer without advancing the consumer
	for i := uint32(0); i < 1024; i++ {
		seq := d.Reserve(1)
		d.Commit(seq, seq)
	}
	if result := d.TryReserve(1); result != ErrCapacityUnavailable {
		t.Fatalf("expected ErrCapacityUnavailable, got %d", result)
	}
}
func TestSharedTryReserve_ZeroSlots(t *testing.T) {
	d := newTestDisruptor(t, 2)
	if result := d.TryReserve(0); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}
func TestSharedTryReserve_ExceedsCapacity(t *testing.T) {
	d := newTestDisruptor(t, 2)
	if result := d.TryReserve(2048); result != ErrReservationSize {
		t.Fatalf("expected ErrReservationSize, got %d", result)
	}
}
func TestSharedTryReserve_CapacityUnavailable(t *testing.T) {
	d := newTestDisruptor(t, 2)
	// fill the ring buffer without advancing the consumer
	for i := uint32(0); i < 1024; i++ {
		seq := d.Reserve(1)
		d.Commit(seq, seq)
	}
	if result := d.TryReserve(1); result != ErrCapacityUnavailable {
		t.Fatalf("expected ErrCapacityUnavailable, got %d", result)
	}
}
func TestSharedTryReserve_ContentionIsNotCapacityExhaustion(t *testing.T) {
	const writers = 8
	const capacity = 1024
	d := newTestDisruptor(t, writers)

	// No consumer is running, so the ring buffer holds exactly `capacity` slots; every one of these claims fits and
	// must succeed even though concurrent writers make individual CAS attempts fail.
	var start, finished sync.WaitGroup
	var failures [writers]int
	start.Add(1)
	finished.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func() {
			defer finished.Done()
			start.Wait()
			for claim := 0; claim < capacity/writers; claim++ {
				if sequence := d.TryReserve(1); sequence < 0 {
					failures[writer]++
				} else {
					d.Commit(sequence, sequence)
				}
			}
		}()
	}
	start.Done()
	finished.Wait()

	for writer, count := range failures {
		if count > 0 {
			t.Errorf("writer %d: %d TryReserve calls failed despite available capacity", writer, count)
		}
	}
	if result := d.TryReserve(1); result != ErrCapacityUnavailable {
		t.Fatalf("expected ErrCapacityUnavailable once the ring buffer is full, got %d", result)
	}
}

func TestSharedLoad_StopsAtUncommittedBatch(t *testing.T) {
	sequencer, _ := newTestSharedSequencer(16)
	first := sequencer.Reserve(4)  // 0-3
	second := sequencer.Reserve(4) // 4-7

	sequencer.Commit(second-3, second) // out of order: the later batch commits first
	if result := sequencer.Load(0); result != -1 {
		t.Fatalf("expected -1 while the first batch is uncommitted, got %d", result)
	}
	if result := sequencer.Load(4); result != 7 {
		t.Fatalf("expected 7 when loading from the second batch, got %d", result)
	}

	sequencer.Commit(first-3, first)
	if result := sequencer.Load(0); result != 7 {
		t.Fatalf("expected 7 once both batches are committed, got %d", result)
	}
}
func TestSharedLoad_RejectsPreviousLap(t *testing.T) {
	sequencer, consumer := newTestSharedSequencer(8)
	for i := 0; i < 2; i++ {
		upper := sequencer.Reserve(4)
		sequencer.Commit(upper-3, upper)
	}
	consumer.Store(7) // everything handled; the next lap may begin

	upper := sequencer.Reserve(4) // 8-11 reuses the slots (and stale markers) of 0-3
	if result := sequencer.Load(8); result != 7 {
		t.Fatalf("expected 7 while the next lap is uncommitted, got %d", result)
	}
	sequencer.Commit(upper-3, upper)
	if result := sequencer.Load(8); result != 11 {
		t.Fatalf("expected 11 once the next lap is committed, got %d", result)
	}
}
func TestSharedLoad_BatchSpanningRingBoundary(t *testing.T) {
	sequencer, consumer := newTestSharedSequencer(8)
	upper := sequencer.Reserve(6) // 0-5
	sequencer.Commit(upper-5, upper)
	consumer.Store(5)

	upper = sequencer.Reserve(4) // 6-9 wraps from slot 6 to slot 1
	sequencer.Commit(upper-3, upper)
	if result := sequencer.Load(6); result != 9 {
		t.Fatalf("expected 9 for a batch spanning the ring boundary, got %d", result)
	}
}
func TestSharedCommit_EmptyRangeIsIgnored(t *testing.T) {
	sequencer, _ := newTestSharedSequencer(8)
	upper := sequencer.Reserve(2) // 0-1
	sequencer.Commit(upper-1, upper)
	sequencer.Commit(upper-1, upper-2) // empty range (lower > upper) beginning at the committed batch
	if result := sequencer.Load(0); result != 1 {
		t.Fatalf("an empty commit must not hide the batch already committed at its lower bound; got %d", result)
	}
}

// A slot that stays in the interior of batches is never rewritten, so its marker can survive any number of laps; it
// must never be mistaken for a current marker when the slot later becomes the start of a batch. This jumps the
// reserved sequence 2^32 laps ahead (where a 32-bit round number would wrap around and falsely match).
func TestSharedLoad_RejectsMarkerStaleForManyLaps(t *testing.T) {
	sequencer, _ := newTestSharedSequencer(8)
	upper := sequencer.Reserve(5) // 0-4
	sequencer.Commit(upper-4, upper)
	upper = sequencer.Reserve(3) // 5-7: slot 5 now holds a lap-0 marker
	sequencer.Commit(upper-2, upper)

	lower := int64(5) + 8<<32                   // slot 5 again, 2^32 laps later
	sequencer.reservedSequence.Store(lower + 2) // reserved, but not committed
	if result := sequencer.Load(lower); result != lower-1 {
		t.Fatalf("a marker 2^32 laps stale was treated as committed: Load(%d) = %d", lower, result)
	}
}

// Concurrent writers reserving random batch sizes across many laps of a small ring buffer, through a two-group
// pipeline; every handler must observe every sequence exactly once, in order, with the value the writer stored.
func TestShared_RandomBatchesAcrossLaps(t *testing.T) { testRandomBatchesAcrossLaps(t, 4, 200_000) }
func TestBusySpin_RandomBatchesAcrossLaps(t *testing.T) {
	// Every writer and handler spins without yielding, so each needs a processor of its own (see BusySpinWaitStrategy).
	// 50,000 sequences still lap the 64-slot ring ~780 times, and keep the -race pass well inside make test's timeout.
	for _, writers := range []int{1, 4} {
		if spinning := writers + 4; runtime.GOMAXPROCS(0) <= spinning {
			t.Skipf("needs GOMAXPROCS > %d for %d spinning goroutines", spinning, spinning)
		}
		testRandomBatchesAcrossLaps(t, writers, 50_000, Options.WaitStrategy(BusySpinWaitStrategy{}))
	}
}
func testRandomBatchesAcrossLaps(t *testing.T, writers int, totalSequences int64, options ...option) {
	const capacity = 64
	var ring [capacity]atomic.Int64
	handlers := []*orderingHandler{{ring: ring[:]}, {ring: ring[:]}, {ring: ring[:]}, {ring: ring[:]}}

	subject, err := New(append([]option{
		Options.BufferCapacity(capacity),
		Options.WriterCount(uint8(writers)),
		Options.NewHandlerGroup(handlers[0], handlers[1]),
		Options.NewHandlerGroup(handlers[2], handlers[3])}, options...)...)
	if err != nil {
		t.Fatal(err)
	}

	var remaining atomic.Int64
	remaining.Store(totalSequences)
	var finished sync.WaitGroup
	finished.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func() {
			defer finished.Done()
			random := rand.New(rand.NewPCG(uint64(writer), 1))
			for {
				size := int64(1 + random.IntN(16))
				if remaining.Add(-size) < 0 {
					return
				}
				var upper int64
				if random.IntN(2) == 0 {
					upper = subject.Reserve(uint32(size))
				} else {
					for upper = subject.TryReserve(uint32(size)); upper < 0; upper = subject.TryReserve(uint32(size)) {
					}
				}
				for sequence := upper - size + 1; sequence <= upper; sequence++ {
					ring[sequence&(capacity-1)].Store(sequence)
				}
				subject.Commit(upper-size+1, upper)
			}
		}()
	}
	go func() { finished.Wait(); _ = subject.Close() }()
	subject.Listen()

	for index, handler := range handlers {
		if handler.failure != "" {
			t.Fatalf("handler %d: %s", index, handler.failure)
		}
		if handler.handled < totalSequences-int64(writers)*16 {
			t.Fatalf("handler %d: handled only %d sequences", index, handler.handled)
		}
	}
}

func newTestDisruptor(t *testing.T, writerCount uint8) Disruptor {
	t.Helper()
	d, err := New(
		Options.BufferCapacity(1024),
		Options.WriterCount(writerCount),
		Options.NewHandlerGroup(nopHandler{}))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type pointerHandler struct{ handled int64 }

func (this *pointerHandler) Handle(_, upper int64) { this.handled = upper }

type handlerFunc func(lower, upper int64)

func (this handlerFunc) Handle(lower, upper int64) { this(lower, upper) }

type recordingHandler struct{ handled *int64 }

func (this recordingHandler) Handle(_, upper int64) { *this.handled = upper }

// racingCommitBarrier simulates a producer that commits and then closes the listener immediately after the
// listener's first read of the committed barrier.
type racingCommitBarrier struct {
	committed *atomicSequence
	listener  *ListenCloser
	fired     bool
}

func (this *racingCommitBarrier) Load(_ int64) int64 {
	value := this.committed.Load()
	if !this.fired {
		this.fired = true
		this.committed.Store(0) // producer: Commit(0, 0)
		_ = (*this.listener).Close()
	}
	return value
}

// committingBarrier simulates a producer that commits sequence 0 immediately after the listener's first barrier read.
type committingBarrier struct {
	committed *atomicSequence
	fired     bool
}

func (this *committingBarrier) Load(_ int64) int64 {
	value := this.committed.Load()
	if !this.fired {
		this.fired = true
		this.committed.Store(0) // producer: Commit(0, 0)
	}
	return value
}

// closingWaitStrategy counts Gate calls and closes the listener the first time it idles.
type closingWaitStrategy struct {
	defaultWaitStrategy
	listener *ListenCloser
	gates    int
}

func (this *closingWaitStrategy) Gate(int64) { this.gates++ }
func (this *closingWaitStrategy) Idle(int64) { _ = (*this.listener).Close() }

type orderingHandler struct {
	ring    []atomic.Int64
	next    int64
	handled int64
	failure string
}

func (this *orderingHandler) Handle(lower, upper int64) {
	if this.failure != "" {
		return
	}
	if lower != this.next {
		this.failure = "batch began at an unexpected sequence"
		return
	}
	for sequence := lower; sequence <= upper; sequence++ {
		if this.ring[sequence&int64(len(this.ring)-1)].Load() != sequence {
			this.failure = "slot did not hold the committed sequence"
			return
		}
	}
	this.handled += upper - lower + 1
	this.next = upper + 1
}

func newTestSharedSequencer(capacity uint32) (*sharedSequencer, *atomicSequence) {
	consumer := newSequence()
	sequencer := newSharedSequencer(capacity, newSequence(), defaultWaitStrategy{})
	sequencer.consumerBarrier = newAtomicBarrier(consumer)
	return sequencer, consumer
}
