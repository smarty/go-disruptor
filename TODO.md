## TODO Items:

### Metrics

- Max number of messages/events to handle per invocation to Handle()

### No panic recovery in consumers

If a `Handler.Handle()` panics, it kills the goroutine. `defer waiter.Done()` in `compositeListener.Listen()` fires, but
the panic propagates and crashes the process. The Java LMAX has `ExceptionHandler` on `BatchEventProcessor` that catches
exceptions, invokes a callback, and optionally continues. Here, a single misbehaving handler takes down everything with
no hook point.

### `sharedSequencer.Reserve`—irrevocable reservation blocks forever on shutdown

`sequencer_shared.go:80`: `this.reservedSequence.Add(slots)` is irrevocable. If the buffer is full and `Close()` is
called (consumers stop draining), the producer spins in the slow-path loop forever. There's no exit path—the wait
strategy has no cancellation signal, and the loop has no check for closed state. Java's `MultiProducerSequencer.next()`
uses a CAS loop that could be made interruptible; the `Add` approach here trades that for throughput.

This is the most dangerous issue in the codebase. A producer calling `Reserve` on a full buffer after `Close()` will
hang the goroutine permanently.

### Cache line layout is x86-centric

`defaultSequencer` is annotated as "64B total — one cache line" and `sharedSequencer` as "128B — two cache lines." On
Apple Silicon (128B lines), both structs fit in a single cache line, which means the hot/cold field separation in
`sharedSequencer` provides no isolation. On 32B platforms (ARM32, MIPS), `defaultSequencer` spans two cache lines and
the hot fields straddle the boundary. The code is correct everywhere but the performance optimization only works as
intended on x86.

### Error sentinels as magic int64 values

`ErrReservationSize = -1` and `ErrCapacityUnavailable = -2` are bare int64 constants, not `error` types. A caller that
forgets to check the return value will use -1 or -2 as a sequence number, silently indexing the ring buffer at
`(-1 & mask)` or `(-2 & mask)`—corrupting data with no signal. Java throws `InsufficientCapacityException`. The current
API makes misuse easy and silent.

### Redundant barrier check for first handler group

In `listener.go:60`, for the first handler group, `upstreamBarrier` and `committedBarrier` are the same object (both set
to `committedBarrier` in `config.go:34`). So the gated branch (`else if upperSequence = this.committedBarrier.Load(...)`)
is unreachable for group 0—it will always return the same result as the first check. Minor waste, not a bug.

### Release/acquire semantics for `committedSlots` via `go:linkname`

`sharedSequencer.committedSlots` is the only Commit→Load ordering edge in the shared producer, but is currently typed as
`[]atomic.Int32`—every `Store`/`Load` pays sequential-consistency cost. The pairwise Commit→Load relationship only
requires release/acquire. The `load-acquire` branch (commit `bc78c07`) prototypes this by changing the field to
`[]uint32` and reaching into the runtime via `go:linkname` to call `internal/runtime/atomic.LoadAcq` / `StoreRel`
directly:

```go
//go:linkname loadAcq32 internal/runtime/atomic.LoadAcq
func loadAcq32(ptr *uint32) uint32

//go:linkname storeRel32 internal/runtime/atomic.StoreRel
func storeRel32(ptr *uint32, val uint32)
```

Measured ~3-5% throughput improvement on the single-slot Reserve path on Apple M5 (ARM64, weakly-ordered); negligible on
batched `ReserveMany` paths because the per-slot Commit/Load loop is amortized. On x86 (TSO) the gain would be near
zero, since plain loads/stores already imply acquire/release semantics there.

Reasons it lives in a branch rather than `master`:

- **Internal package path.** `internal/runtime/atomic` is internal to the Go runtime, and was itself renamed from
  `runtime/internal/atomic` in Go 1.26. `LoadAcq` and `StoreRel` are not declared with matching `//go:linkname`
  directives on the runtime side, so we are reaching across the boundary one-directionally—any future restructuring
  breaks the build with no deprecation warning.
- **Race detector.** Switching off `sync/atomic` types makes `go test -race` report false positives on legitimate ring
  buffer reads/writes, because the linkname'd functions are invisible to the race instrumentation.
- **Audience.** A 3-5% gain on one architecture for one access pattern is narrow; it would need to outweigh losing
  race-detector coverage for everyone.

A more durable path forward would be either first-class `LoadAcquire` / `StoreRelease` operations in `sync/atomic`
(proposed but not landed as of Go 1.26), or per-arch assembly stubs colocated in this package so the race detector still
sees the operations.

### Compared to Java LMAX—missing features

| Feature                                                  | Java LMAX                             | This port                                        |
|----------------------------------------------------------|---------------------------------------|--------------------------------------------------|
| **WorkerPool** (events partitioned across handlers)      | Yes                                   | No—fan-out only (every handler sees every event) |
| **Handler panic/exception recovery**                     | `ExceptionHandler` callback           | Process crash                                    |
| **Dependency DAGs**                                      | Arbitrary via `SequenceBarrier`       | Linear pipeline only                             |
| **Blocking Reserve timeout / cancellation**              | Via `WaitStrategy` + `AlertException` | Blocks forever, no exit                          |
| **`SequenceReportingEventHandler`** (mid-batch progress) | Yes                                   | Always publishes full batch                      |
| **EventTranslator** (structured publish API)             | Yes                                   | User manages ring buffer directly                |

The biggest functional gap is **WorkerPool**—many real-world use cases need work distribution (one event to one
handler), not just fan-out. The biggest robustness gap is the inability to interrupt a blocked `Reserve`.
