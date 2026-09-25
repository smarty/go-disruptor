Overview
----------------------------

This is a port of the [LMAX Disruptor](https://github.com/LMAX-Exchange/disruptor) into the Go programming language. It retains the essence and spirit of the Disruptor and utilizes the same underlying abstractions and concepts, but does not maintain the names or API.

On modern desktop hardware it can pass many hundreds of millions of messages per second&mdash;yes hundreds of millions&mdash;from one goroutine to another goroutine.

Once initialized and running, one of the preeminent design considerations of the Disruptor is to process messages at a constant rate. It does this using two primary techniques. First, it avoids using locks which contend between CPU cores thus interfering with and ultimately preventing scaling. Secondly, the Disruptor produces no garbage. It does so by pre-allocating contiguous space on a ring buffer. By avoiding garbage altogether, the need for a garbage collection and the stop-the-world application pauses thus introduced can be almost entirely avoided.

In Go, the current implementation of a channel (`chan`) maintains a mutex around send, receive, and `len` operations. The consequence is that it has a maximum uncontended throughput of 20-30 million messages per second&mdash;more than an order of magnitude slower when compared to the Disruptor. The same channel, when contended between OS threads only pushes about 5-7 million messages per second with the Disruptor continuing to push at least an order of magnitude number of messages more than a similarly contended channel.

Example Usage
-------------

```go
package main

import (
	"fmt"

	"github.com/smarty/go-disruptor"
)

func main() {
	consumer1Group1 := exampleConsumer{Name: "Group 1, Consumer 1"}
	consumer2Group1 := exampleConsumer{Name: "Group 1, Consumer 2"}
	consumer1Group2 := exampleConsumer{Name: "Group 2, Consumer 1"}
	consumer2Group2 := exampleConsumer{Name: "Group 2, Consumer 2"}

	instance, err := disruptor.New(
		disruptor.Options.BufferCapacity(bufferSize),
		disruptor.Options.WriterCount(1), // set the value to 2+ if we intend to have a multiple, concurrent writers
		disruptor.Options.NewHandlerGroup(consumer1Group1, consumer2Group1), // these consumers run in parallel
		disruptor.Options.NewHandlerGroup(consumer1Group2, consumer2Group2)) // these consumers run in parallel AFTER the first group has completed

	if err != nil {
		panic(fmt.Errorf("configuration error: %w", err))
	}

	go func() {
		// Shut down the Disruptor instance after we have written to it.
		defer func() { _ = instance.Close() }()

		// EXAMPLE: Claim a single slot or entry
		reservedSequence := instance.Reserve(1)
		// now that we have a reserved slot, write to it
		ringBuffer[reservedSequence&bufferMask] = 42
		// commit the write to make the data available to the configured consumers
		instance.Commit(reservedSequence, reservedSequence)

		// EXAMPLE: Claim 16 slots
		upperReservation := instance.Reserve(16)
		lowerReservation := upperReservation - 16 + 1
		// write whatever application data is appropriate to the ring buffer
		for sequence := lowerReservation; sequence <= upperReservation; sequence++ {
			ringBuffer[sequence&bufferMask] = sequence + 42
		}
		// commit the write to make the data available to the configured consumers
		instance.Commit(lowerReservation, upperReservation)
	}()

	// block until explicitly closed and all enqueued work has been completed
	instance.Listen()
}

type exampleConsumer struct { Name string }

func (this exampleConsumer) Handle(lowerSequence, upperSequence int64) {
	for sequence := lowerSequence; sequence <= upperSequence; sequence++ {
		entry := ringBuffer[sequence&bufferMask]
		fmt.Printf("Consumer [%s], Sequence: [%d], Value: [%d]", this.Name, sequence, entry)
	}
}

const (
	bufferSize = 1024 * 64 // must be a power of 2
	bufferMask = bufferSize - 1
)

var ringBuffer = [bufferSize]int64{}
```

The above example is more complex than a typical channel implementation. It's demonstrating an entire pipeline with 4 separate consumers across 2 phases. When removing all the comments, instantiation, and extra fluff to explain what's happening, the code is very concise.  In fact, a given "publish" operation only takes three instructions&mdash;`Reserve` a slot, update the ring buffer, and `Commit` the reserved sequence range.  On the consumer side, there's a `for`-loop to handle all incoming items into your application.  Again, this not quite as concise as a channel (nor as flexible), but it's much, much faster.

Benchmarks
----------------------------
Each of the following benchmark tests sends an incrementing sequence message from one goroutine to another. The receiving goroutine asserts that the message received is the expected incrementing sequence value. Any failures cause a panic.

* CPU: `Intel Core i7-12700K`
* Operating System: `Linux 6.18`
* Go Runtime: `Go 1.25`

**Channels (16K buffer)**

| Scenario                                                  | Per Operation Time |
|-----------------------------------------------------------|--------------------|
| Single-Producer, Single-Consumer, Blocking                | 32.4 ns            |
| Multi-Producer, Single-Consumer, Blocking (4 writers)     | 141.8 ns           |
| Single-Producer, Single-Consumer, Non-blocking            | 159.0 ns           |
| Multi-Producer, Single-Consumer, Non-blocking (4 writers) | 531.9 ns           |

**Disruptor: Single Writer (64K buffer)**

| Scenario                                     | Per Operation Time |
|----------------------------------------------|--------------------|
| Single-Producer, Single-Consumer, Reserve 1  | 6.5 ns             |
| Single-Producer, Single-Consumer, Reserve 16 | 0.4 ns             |
| Single-Producer, Multi-Consumer, Reserve 1   | 6.8 ns             |
| Single-Producer, Multi-Consumer, Reserve 16  | 0.5 ns             |

**Disruptor: Multiple Writers (64K buffer, 4 writers)**

| Scenario                                    | Per Operation Time |
|---------------------------------------------|--------------------|
| Multi-Producer, Single-Consumer, Reserve 1  | 32.3 ns            |
| Multi-Producer, Single-Consumer, Reserve 16 | 3.1 ns             |
| Multi-Producer, Multi-Consumer, Reserve 1   | 34.3 ns            |
| Multi-Producer, Multi-Consumer, Reserve 16  | 3.1 ns             |

When In Doubt, Use Channels
----------------------------
Despite Go channels being significantly slower than the Disruptor, channels should still be considered the easiest, best, and most desirable choice for the vast majority of all use cases. The Disruptor's target use case is ultra-low latency environments where application response times are measured in nanoseconds and where stable, consistent latency is paramount and latency spikes cannot be tolerated.

Pre-Release Candidate
---------
Due to improvements in the [Go memory model](http://golang.org/ref/mem) as of version 1.19, this project was finally updated to utilize sync/atomic operations with happens-before guarantees, making the Disruptor possible in Go.

Performance Tweaks:
---------
To keep caches hot, each producer (where possible) and each consumer should have its goroutine pinned to a particular core via `runtime.LockOSThread()` and the underlying OS thread pinned to a particular CPU core using a CGo call to `sched_setaffinity`. 

Caveats
-------
In the Java-based Disruptor implementation, a ring buffer is created, pre-allocated, and prepopulated with instances of the class which serve as the message type to be transferred between threads. For simplicity, this implementation does not manage the actual ring buffer but defers that management to the caller, as seen in the example. Pre-populating the ring buffer at startup should ensure contiguous memory allocation for all items in the various ring buffer slots, whereas on-the-fly creation may introduce gaps in the memory allocation and subsequent CPU cache misses which introduce undesirable latency spikes.

As in this example, the reference to the ring buffer data structure can (but need not) be scoped as a package-level variable. Generally speaking package-level state is considered an anti-pattern or poor practice. Despite this, any given application should have very few Disruptor instances. The instances are designed to be created at startup, run for the duration of the application lifetime, and stopped during shutdown. Unlike channels, they are not typically meant to be created ad-hoc and passed around. It is the responsibility of the application developer to manage references to the ring buffer instances such that the producer can push messages to the buffer and consumers can receive messages from the buffer.

How the Disruptor Works
-----------------------

The Disruptor is a high-performance inter-thread messaging pattern originally developed by [LMAX Exchange](https://www.lmax.com/) for their financial exchange platform. It replaces traditional queues with a pre-allocated ring buffer and a system of sequence counters, eliminating the primary sources of latency in concurrent systems.

### Ring Buffer

The core data structure is a fixed-size ring buffer&mdash;a contiguous array whose length is always a power of 2. This power-of-2 constraint allows the use of a fast bitwise AND operation (`sequence & mask`) instead of an expensive modulo operation to map a sequence number to a slot index. Because the buffer is pre-allocated at startup, there are no allocations during operation and no garbage collection pressure.

### Sequences and Sequencers

Coordination between producers and consumers happens through atomic sequence counters rather than locks. A producer claims one or more slots by atomically advancing a shared sequence counter (via `Reserve`), writes data into those slots, and then signals completion (via `Commit`). Consumers independently track their own position in the ring buffer and advance as new committed data becomes available.

This design means producers never block on consumers and consumers never block on each other (within the same handler group). The only contention point is the atomic increment during `Reserve`, which on x86 compiles to a single `LOCK XADD` instruction.

### Why It's Fast

Traditional concurrent queues (including Go channels) suffer from several performance bottlenecks that the Disruptor avoids:

- **No locks.** Go channels use a mutex on every send and receive. The Disruptor uses lock-free atomic operations, which scale across CPU cores without contention-induced serialization.

- **No allocation.** Channels copy values into and out of an internal buffer, and the runtime must manage this memory. The Disruptor's ring buffer is pre-allocated once; producers write directly into it and consumers read directly from it.

- **Mechanical sympathy.** The Disruptor is designed around how modern CPUs actually work. The ring buffer is contiguous in memory, so sequential access follows the CPU's prefetch pattern. Sequence counters are padded to occupy their own cache lines, preventing false sharing&mdash;a phenomenon where two independent variables on the same cache line (typically 64 bytes, though processor-dependent) cause the CPU cores to constantly invalidate each other's caches.

- **Batching.** When a consumer falls behind, it can process multiple entries in a single call (`Handle(lower, upper)`), amortizing the overhead of synchronization across many items. This is why the "Reserve 16" benchmarks are dramatically faster per operation than "Reserve 1."

### Handler Groups and Pipelines

Consumers are organized into handler groups that form a processing pipeline. Within a group, each handler runs on its own goroutine and processes every message independently&mdash;useful for fan-out patterns where multiple consumers need to see the same data. Between groups, a dependency is enforced: all handlers in group N must finish processing a sequence before any handler in group N+1 can see it. This enables multi-stage processing pipelines without explicit coordination between stages.

### Single-Writer vs Multi-Writer

With a single producer (`WriterCount(1)`), the sequencer needs no atomic read-modify-write operations: `Reserve` only updates plain fields owned by the one producer goroutine, and `Commit` is a single uncontended atomic store that publishes the written slots to consumers. That store is still the most expensive step on the path (on x86 it compiles to `XCHG`, a full memory barrier), which is why reserving and committing in batches pays off even for a single producer. With multiple producers (`WriterCount(2+)`), the sequencer uses atomic add to claim slots and per-batch commit markers to handle out-of-order commits from concurrent writers. The single-writer path is significantly faster, so prefer it when your architecture allows a single producer goroutine.
