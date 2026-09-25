package disruptor

import (
	"sync"
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
	listener = newListener(newSequence(), barrier, newAtomicBarrier(committed), defaultWaitStrategy{}, recordingHandler{handled: &handled})

	listener.Listen()

	if handled != 0 {
		t.Fatalf("sequence 0 was committed before Close but never handled; handled=%d", handled)
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
