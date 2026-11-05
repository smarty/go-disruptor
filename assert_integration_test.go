package disruptor

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEndToEnd_SingleWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test")
	}
	coordinator := newEndToEndDisruptor(1)

	go func() {
		defer func() { _ = coordinator.Close() }()
		for i := int64(0); i < endToEndSequences; i++ {
			sequence := coordinator.Reserve(1)
			writeEndToEndEntry(sequence)
			coordinator.Commit(sequence, sequence)
		}
	}()

	coordinator.Listen()
	verifyEndToEndBuffer(t)
}
func TestEndToEnd_SharedWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test")
	}
	coordinator := newEndToEndDisruptor(endToEndWriterCount)

	go func() {
		var waiter sync.WaitGroup
		waiter.Add(endToEndWriterCount)
		defer func() { waiter.Wait(); _ = coordinator.Close() }()

		var remaining atomic.Int64
		remaining.Store(endToEndSequences)
		for writerIndex := 0; writerIndex < endToEndWriterCount; writerIndex++ {
			go func() {
				defer waiter.Done()
				for remaining.Add(-1) >= 0 {
					sequence := coordinator.Reserve(1)
					writeEndToEndEntry(sequence)
					coordinator.Commit(sequence, sequence)
				}
			}()
		}
	}()

	coordinator.Listen()
	verifyEndToEndBuffer(t)
}

func newEndToEndDisruptor(writerCount uint8) Disruptor {
	endToEndBuffer = [endToEndBufferSize]endToEndValues{}
	value, err := New(
		Options.BufferCapacity(endToEndBufferSize),
		Options.WriterCount(writerCount),
		Options.NewHandlerGroup(newEvenSequenceHandler(), newOddSequenceHandler()),
		Options.NewHandlerGroup(newEvenSequenceHandler(), newOddSequenceHandler()),
	)
	if err != nil {
		panic(err)
	}
	return value
}
func writeEndToEndEntry(sequence int64) {
	entry := &endToEndBuffer[sequence&EndToEndBufferMask]
	entry.Sequence = sequence
	entry.Value1 += sequence + 1
	entry.Value2 += sequence + 2
	entry.Value3 += sequence + 3
	entry.Value4 += sequence + 4
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

type endToEndHandler bool
type endToEndValues struct {
	Sequence int64    // 8B
	Value1   int64    // 8B
	Value2   int64    // 8B
	Value3   int64    // 8B
	Value4   int64    // 8B
	_        [3]int64 // 24B padding to 64B cache line
}

func newEvenSequenceHandler() Handler { return endToEndHandler(true) }
func newOddSequenceHandler() Handler  { return endToEndHandler(false) }
func (this endToEndHandler) Handle(lower, upper int64) {
	for sequence := lower; sequence <= upper; sequence++ {
		if (sequence&1 == 0) != this {
			continue
		}

		entry := &endToEndBuffer[sequence&EndToEndBufferMask]
		entry.Value1 *= 2
		entry.Value2 *= 2
		entry.Value3 *= 2
		entry.Value4 *= 2
	}
}

func verifyEndToEndBuffer(t *testing.T) {
	t.Helper()
	for i := range endToEndBuffer {
		assertEndToEndEntry(t, i)
	}
}
func assertEndToEndEntry(t *testing.T, index int) {
	t.Helper()
	entry := endToEndBuffer[index]
	assertEndToEndField(t, index, "Value1", entry.Value1, expectedEndToEndValue(index, 1))
	assertEndToEndField(t, index, "Value2", entry.Value2, expectedEndToEndValue(index, 2))
	assertEndToEndField(t, index, "Value3", entry.Value3, expectedEndToEndValue(index, 3))
	assertEndToEndField(t, index, "Value4", entry.Value4, expectedEndToEndValue(index, 4))
}
func expectedEndToEndValue(index int, offset int64) int64 {
	var value int64
	for wrap := int64(0); wrap < endToEndWraps; wrap++ {
		sequence := int64(index) + wrap*endToEndBufferSize
		value = (value + sequence + offset) * endToEndMultiplier
	}
	return value
}
func assertEndToEndField(t *testing.T, index int, name string, actual, expected int64) {
	t.Helper()
	if actual != expected {
		t.Errorf("entry %d: %s=%d, want %d", index, name, actual, expected)
	}
}

var endToEndBuffer [endToEndBufferSize]endToEndValues

const (
	endToEndBufferSize    = 1024 * 128
	EndToEndBufferMask    = endToEndBufferSize - 1
	endToEndSequences     = endToEndBufferSize * 4
	endToEndWraps         = endToEndSequences / endToEndBufferSize
	endToEndHandlerGroups = 2
	endToEndMultiplier    = 1 << endToEndHandlerGroups
	endToEndWriterCount   = 2
)

// Four writers commit random batches with random idle gaps, so listeners park and wake at every gap length; every
// handler must see every committed sequence exactly once, in order. The long Timeout means only a notify can wake a
// listener. A lost wake-up would only delay a handler until the next Commit, and Close drains everything, so before
// every 64th gap each writer also waits until every handler reaches its last commit: a lost wake-up misses the 1s
// deadline, far under the 10s Timeout.
func TestEndToEnd_ParkingRandomGaps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test")
	}
	const capacity, writers, totalSequences = 1024, 4, 200_000
	var ring [capacity]atomic.Int64
	handlers := []*orderingHandler{{ring: ring[:]}, {ring: ring[:]}, {ring: ring[:]}, {ring: ring[:]}}
	subject, err := New(
		Options.BufferCapacity(capacity),
		Options.WriterCount(writers),
		Options.WaitStrategy(ParkingWaitStrategy{Timeout: 10 * time.Second}),
		Options.NewHandlerGroup(handlers[0], handlers[1]),
		Options.NewHandlerGroup(handlers[2], handlers[3]))
	if err != nil {
		t.Fatal(err)
	}

	gaps := []time.Duration{0, 10 * time.Microsecond, time.Millisecond, 5 * time.Millisecond}
	listeners := listenersOf(subject) // in the same order as handlers
	var failure atomic.Pointer[string]
	var remaining, committed atomic.Int64
	remaining.Store(totalSequences)
	var finished sync.WaitGroup
	finished.Add(writers)
	for writer := 0; writer < writers; writer++ {
		go func() {
			defer finished.Done()
			random := rand.New(rand.NewPCG(uint64(writer), 2))
			for commits := 1; ; commits++ {
				size := int64(1 + random.IntN(16))
				if remaining.Add(-size) < 0 {
					return
				}
				upper := subject.Reserve(uint32(size))
				for sequence := upper - size + 1; sequence <= upper; sequence++ {
					ring[sequence&(capacity-1)].Store(sequence)
				}
				subject.Commit(upper-size+1, upper)
				committed.Add(size)
				if commits%64 == 0 {
					if message := awaitHandled(listeners, upper, time.Second); message != "" {
						failure.CompareAndSwap(nil, &message)
						return
					}
				}
				if gap := gaps[random.IntN(len(gaps))]; gap > 0 {
					time.Sleep(gap)
				}
			}
		}()
	}
	go func() { finished.Wait(); _ = subject.Close() }()

	done := make(chan struct{})
	go func() { subject.Listen(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not end within 60s")
	}

	if message := failure.Load(); message != nil {
		t.Fatal(*message)
	}
	for index, handler := range handlers {
		if handler.failure != "" {
			t.Fatalf("handler %d: %s", index, handler.failure)
		}
		if handler.handled != committed.Load() {
			t.Fatalf("handler %d: handled %d sequences, but %d were committed", index, handler.handled, committed.Load())
		}
	}
}

// awaitHandled waits until every listener has handled sequence, and returns a failure message naming the first listener
// that did not within the deadline, or "" when all did.
func awaitHandled(listeners []*defaultListener, sequence int64, within time.Duration) string {
	deadline := time.Now().Add(within)
	for index, listener := range listeners {
		for listener.handledSequence.Load() < sequence {
			if time.Now().After(deadline) {
				return fmt.Sprintf("handler %d did not reach sequence %d within %v (it is at %d): a lost wake-up",
					index, sequence, within, listener.handledSequence.Load())
			}
			time.Sleep(10 * time.Microsecond)
		}
	}
	return ""
}
