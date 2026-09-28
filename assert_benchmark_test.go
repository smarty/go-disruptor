package disruptor

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// Key:
// SP   = single-producer
// MP   = multi-producer
// SC   = single-consumer
// MC   = multi-consumer
// R[N] = reserve N number of slots

func BenchmarkChannel(b *testing.B) {
	b.Run("SPSC/Blocking", func(b *testing.B) { benchmarkChannelBlocking(b, 1) })
	b.Run("MPSC/Blocking", func(b *testing.B) { benchmarkChannelBlocking(b, 4) })
	b.Run("SPSC/NonBlocking", func(b *testing.B) { benchmarkChannelNonBlocking(b, 1) })
	b.Run("MPSC/NonBlocking", func(b *testing.B) { benchmarkChannelNonBlocking(b, 4) })
}
func benchmarkChannelBlocking(b *testing.B, writers int64) {
	channel := make(chan int64, 1024*16)
	iterations := int64(b.N)

	b.ReportAllocs()
	b.ResetTimer()

	for x := int64(0); x < writers; x++ {
		go func() {
			for i := int64(0); i < iterations; i++ {
				channel <- i
			}
		}()
	}

	for i := int64(0); i < iterations*writers; i++ {
		msg := <-channel
		if writers == 1 && msg != i {
			panic("out of sequence")
		}
	}
}
func benchmarkChannelNonBlocking(b *testing.B, writers int64) {
	iterations := int64(b.N)
	maxReads := iterations * writers
	channel := make(chan int64, 1024*16)

	b.ReportAllocs()
	b.ResetTimer()

	for x := int64(0); x < writers; x++ {
		go func() {
			for i := int64(0); i < iterations; {
				select {
				case channel <- i:
					i++
				default:
					continue
				}
			}
		}()
	}

	for i := int64(0); i < maxReads; {
		select {
		case msg := <-channel:
			if writers == 1 && msg != i {
				panic("out of sequence")
			}
			i++
		default:
			continue
		}
	}
}

func BenchmarkSequence(b *testing.B) {
	b.Run("Store", func(b *testing.B) {
		sequence := newSequence()
		b.ReportAllocs()
		b.ResetTimer()
		for i := int64(0); i < int64(b.N); i++ {
			sequence.Store(i)
		}
	})
	b.Run("Load", func(b *testing.B) {
		sequence := newSequence()
		b.ReportAllocs()
		b.ResetTimer()
		for i := int64(0); i < int64(b.N); i++ {
			_ = sequence.Load()
		}
	})
	b.Run("LoadAsBarrier", func(b *testing.B) {
		var barrier sequenceBarrier = newAtomicBarrier(newSequence())
		b.ReportAllocs()
		b.ResetTimer()
		for i := int64(0); i < int64(b.N); i++ {
			_ = barrier.Load(0)
		}
	})
	b.Run("CompositeBarrier", func(b *testing.B) {
		barrier := newCompositeBarrier(newSequence(), newSequence(), newSequence(), newSequence())
		b.ReportAllocs()
		b.ResetTimer()
		for i := int64(0); i < int64(b.N); i++ {
			barrier.Load(0)
		}
	})
}
func BenchmarkSequencer(b *testing.B) {
	b.Run("Reserve", func(b *testing.B) {
		read, written := newSequence(), newSequence()
		writer := newSequencer(1024, written, newAtomicBarrier(read), defaultWaitStrategy{})
		b.ReportAllocs()
		b.ResetTimer()
		for i := int64(0); i < int64(b.N); i++ {
			sequence := writer.Reserve(1)
			read.Store(sequence)
		}
	})
	b.Run("NextWrapPoint", func(b *testing.B) {
		read, written := newSequence(), newSequence()
		writer := newSequencer(1024*16, written, newAtomicBarrier(read), defaultWaitStrategy{})
		b.ReportAllocs()
		b.ResetTimer()
		for i := int64(0); i < int64(b.N); i++ {
			sequence := writer.Reserve(1)
			read.Store(sequence)
		}
	})
	b.Run("Commit", func(b *testing.B) {
		writer := newSequencer(1024, newSequence(), nil, defaultWaitStrategy{})
		b.ReportAllocs()
		b.ResetTimer()
		for i := int64(0); i < int64(b.N); i++ {
			writer.Commit(i, i)
		}
	})
	b.Run("SP SC", func(b *testing.B) { benchmarkDisruptor(b, reserve1, 1, nopHandler{}) })
	b.Run("SP4SC", func(b *testing.B) { benchmarkDisruptor(b, reserve4, 1, nopHandler{}) })
	b.Run("SP MC", func(b *testing.B) { benchmarkDisruptor(b, reserve1, 1, nopHandler{}, nopHandler{}) })
	b.Run("SP4MC", func(b *testing.B) { benchmarkDisruptor(b, reserve4, 1, nopHandler{}, nopHandler{}) })
}
func BenchmarkSharedSequencer(b *testing.B) {
	b.Run("SP SC/R1", func(b *testing.B) { // one writer goroutine, but WriterCount(2) forces the shared sequencer
		benchmarkDisruptorWith(b, reserve1, 1, []Handler{nopHandler{}}, Options.WriterCount(2))
	})
	b.Run("MP SC/R1", func(b *testing.B) { benchmarkDisruptor(b, reserve1, 4, nopHandler{}) })
	b.Run("MP SC/R4", func(b *testing.B) { benchmarkDisruptor(b, reserve4, 4, nopHandler{}) })
	b.Run("MP MC/R1", func(b *testing.B) { benchmarkDisruptor(b, reserve1, 4, nopHandler{}, nopHandler{}) })
	b.Run("MP MC/R4", func(b *testing.B) { benchmarkDisruptor(b, reserve4, 4, nopHandler{}, nopHandler{}) })
}

// BenchmarkSharedSequencerCapacity sweeps the ring capacity, and with it the footprint of committedSlots (8B per slot:
// 8KB at 1K slots, 512KB at the default 64K, 8MB at 1M), relative to the core's private L2.
func BenchmarkSharedSequencerCapacity(b *testing.B) {
	for _, capacity := range []uint32{1 << 10, 1 << 14, 1 << 16, 1 << 18, 1 << 20} {
		name := fmt.Sprintf("%dK", capacity>>10)
		b.Run("SP SC/R1/"+name, func(b *testing.B) {
			benchmarkDisruptorWith(b, reserve1, 1, []Handler{nopHandler{}}, Options.WriterCount(2), Options.BufferCapacity(capacity))
		})
		b.Run("MP SC/R1/"+name, func(b *testing.B) {
			benchmarkDisruptorWith(b, reserve1, 4, []Handler{nopHandler{}}, Options.WriterCount(4), Options.BufferCapacity(capacity))
		})
	}
}

// BenchmarkSequencerCapacity sweeps the single writer's ring capacity. The single writer allocates nothing per slot,
// so capacity matters only through how often the producer waits for a sleeping consumer: at 1K slots, reserving 16 at
// a time was 23x slower than at 16K with the original wait strategy.
func BenchmarkSequencerCapacity(b *testing.B) {
	for _, capacity := range []uint32{1 << 10, 1 << 12, 1 << 14, 1 << 16} {
		name := fmt.Sprintf("%dK", capacity>>10)
		b.Run("SP SC/R1/"+name, func(b *testing.B) {
			benchmarkDisruptorWith(b, reserve1, 1, []Handler{nopHandler{}}, Options.BufferCapacity(capacity))
		})
		b.Run("SP SC/R16/"+name, func(b *testing.B) {
			benchmarkDisruptorWith(b, reserve4, 1, []Handler{nopHandler{}}, Options.BufferCapacity(capacity))
		})
	}
}

// BenchmarkWakeLatency measures how long a consumer takes to handle a single event published after the ring has been
// empty for a given gap, which is what the WaitStrategy's Idle phases determine. The producer busy-waits through the
// gap and for the event to be handled, so the reported latency-ns/op is the consumer's alone (plus ~20ns of clock
// reads); ns/op includes the gap.
func BenchmarkWakeLatency(b *testing.B) {
	for _, gap := range []time.Duration{0, 2 * time.Microsecond, 50 * time.Microsecond} {
		b.Run(fmt.Sprintf("SP SC/%v", gap), func(b *testing.B) { benchmarkWakeLatency(b, gap, 1) })
		b.Run(fmt.Sprintf("Shared SP SC/%v", gap), func(b *testing.B) { benchmarkWakeLatency(b, gap, 2) })
	}
}
func benchmarkWakeLatency(b *testing.B, gap time.Duration, writerCount uint8) {
	handler := &handledSequenceHandler{}
	handler.handled.Store(defaultSequenceValue)
	disruptor, _ := New(Options.BufferCapacity(ringBufferSize), Options.WriterCount(writerCount), Options.NewHandlerGroup(handler))
	defer disruptor.Listen()

	go func() {
		defer func() { _ = disruptor.Close() }()
		time.Sleep(time.Millisecond * 100) // let the Listen goroutine have time to start
		b.ReportAllocs()
		b.ResetTimer()

		var latency time.Duration
		for i := 0; i < b.N; i++ {
			for idleSince := time.Now(); time.Since(idleSince) < gap; {
			}
			published := time.Now()
			sequence := disruptor.Reserve(1)
			disruptor.Commit(sequence, sequence)
			for handler.handled.Load() < sequence {
			}
			latency += time.Since(published)
		}
		b.ReportMetric(float64(latency.Nanoseconds())/float64(b.N), "latency-ns/op")
	}()
}

type handledSequenceHandler struct{ handled atomic.Int64 }

func (this *handledSequenceHandler) Handle(_, upper int64) { this.handled.Store(upper) }

// BenchmarkRingBuffer adds the application's side of the traffic that the other benchmarks omit: writers store into a
// ring buffer of 64B entries (4MB at 64K slots) before committing, and the handler reads every entry it is given.
func BenchmarkRingBuffer(b *testing.B) {
	b.Run("SP SC/R1", func(b *testing.B) { benchmarkRingBuffer(b, reserve1, 1) })
	b.Run("SP SC/R4", func(b *testing.B) { benchmarkRingBuffer(b, reserve4, 1) })
	b.Run("MP SC/R1", func(b *testing.B) { benchmarkRingBuffer(b, reserve1, 4) })
	b.Run("MP SC/R4", func(b *testing.B) { benchmarkRingBuffer(b, reserve4, 4) })
}
func benchmarkRingBuffer(b *testing.B, count uint32, writerCount uint8) {
	iterations := int64(b.N)
	offset := int64(count) - 1

	entries := newBenchmarkEntries()
	handler := &ringBufferHandler{entries: entries}
	disruptor, _ := New(Options.BufferCapacity(ringBufferSize), Options.WriterCount(writerCount), Options.NewHandlerGroup(handler))
	defer disruptor.Listen()

	go func() {
		var waiter sync.WaitGroup
		waiter.Add(int(writerCount))
		defer func() { waiter.Wait(); _ = disruptor.Close() }()
		time.Sleep(time.Millisecond * 100) // let the Listen goroutine have time to start
		b.ReportAllocs()
		b.ResetTimer()

		for i := uint8(0); i < writerCount; i++ {
			go func() {
				defer waiter.Done()
				for sequence := int64(defaultSequenceValue); sequence < iterations; {
					sequence = disruptor.Reserve(count)
					for lower := sequence - offset; lower <= sequence; lower++ {
						entries[lower&ringBufferMask].Sequence = lower
					}
					disruptor.Commit(sequence-offset, sequence)
				}
			}()
		}
	}()
}

func benchmarkDisruptor(b *testing.B, count uint32, writerCount uint8, consumers ...Handler) {
	benchmarkDisruptorWith(b, count, int(writerCount), consumers, Options.WriterCount(writerCount))
}
func benchmarkDisruptorWith(b *testing.B, count uint32, writerCount int, consumers []Handler, opts ...option) {
	iterations := int64(b.N)
	slots := int64(count)
	offset := slots - 1

	opts = append([]option{Options.BufferCapacity(ringBufferSize), Options.NewHandlerGroup(consumers...)}, opts...)
	disruptor, _ := New(opts...)
	defer disruptor.Listen()

	go func() {
		var waiter sync.WaitGroup
		waiter.Add(writerCount)
		defer func() { waiter.Wait(); _ = disruptor.Close() }()
		time.Sleep(time.Millisecond * 100) // let the Listen goroutine have time to start
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < writerCount; i++ {
			go func() {
				defer waiter.Done()
				for sequence := int64(defaultSequenceValue); sequence < iterations; {
					sequence = disruptor.Reserve(count)
					disruptor.Commit(sequence-offset, sequence)
				}
			}()
		}
	}()
}

type nopHandler struct{}

func (this nopHandler) Handle(int64, int64) {}

type ringBufferHandler struct {
	entries  *[ringBufferSize]benchmarkEntry
	checksum int64
}

func (this *ringBufferHandler) Handle(lower, upper int64) {
	for sequence := lower; sequence <= upper; sequence++ {
		this.checksum += this.entries[sequence&ringBufferMask].Sequence
	}
}

type benchmarkEntry struct {
	Sequence int64    // 8B
	_        [7]int64 // 56B padding to 64B cache line
}

// newBenchmarkEntries returns a cache-line-aligned ring buffer. A package-level array would be placed wherever the
// linker chose (one build put it 32B past a line boundary, so every entry straddled two lines), which can differ
// between the variant binaries being compared.
func newBenchmarkEntries() *[ringBufferSize]benchmarkEntry {
	backing := make([]byte, (ringBufferSize+1)*unsafe.Sizeof(benchmarkEntry{}))
	offset := (CacheLineBytes - uintptr(unsafe.Pointer(&backing[0]))%CacheLineBytes) % CacheLineBytes
	return (*[ringBufferSize]benchmarkEntry)(unsafe.Pointer(&backing[offset]))
}

const (
	ringBufferSize = 1 << 16 // 64K
	ringBufferMask = ringBufferSize - 1
	reserve1       = 1
	reserve4       = 16
)
