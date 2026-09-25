package disruptor

import "testing"

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
