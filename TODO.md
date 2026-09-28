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

### Release stores for `Commit` via an amd64 assembly stub

`Commit` is the only Commit→Load ordering edge in each sequencer (`committedSequence` for the single producer,
`committedSlots` for the shared producer), and that edge only requires release/acquire. Both currently use
`atomic.Int64.Store`, which Go compiles to `XCHG` on x86: a locked instruction that cannot retire until the core owns
the cache line, which the consumer usually still holds. A release store is a plain `MOV` on x86 (TSO), which retires
into the store buffer while ownership is acquired in the background. Seq-cst *loads* are already plain `MOV`s on x86, so
only the store side needs to change.

**Measured upper bound** (i7-12700K, 2026-09-27, unreserved pilot, 10 shuffled rounds): an unsafe variant that replaces
both `Commit` stores with plain stores through `unsafe.Pointer` made single-writer rows 44-49% faster (`SP SC` 5.87 →
3.04 ns), shared `SP SC/R1` 33% faster, and `MP SC/R1` 5% faster; the other multi-producer rows did not move (they are
bound by contention on `reservedSequence.Add`). Single-producer variance also fell from ±7-25% to ±3-6%, suggesting
much of that noise came from `XCHG` stalls rather than the Go scheduler (untested). This agrees with `perf` on Zen 2,
where `Commit`'s `XCHG` was 51-58% of single-writer cycles. On Apple M5 (ARM64), the earlier `load-acquire` prototype
measured only ~3-5%. That prototype (branch `load-acquire`, commit `bc78c07`) predates the one-marker-per-batch
shared sequencer and used 32-bit operations.

**The remaining viable design:** a one-instruction amd64 assembly function (`TEXT ·storeRelease(SB)`, a `MOVQ`) in
this package, called from both `Commit` methods in a file built with `//go:build amd64 && !race`. Every other build,
including all `-race` builds, keeps `atomic.Int64.Store`. The build-tag split is what keeps the race detector working:

- The race detector models synchronization only through instrumented `sync/atomic` calls. Assembly is not
  instrumented, so an unconditional stub would erase the Commit→Load happens-before edge, and `go test -race` would
  report false positives on every legitimate ring-buffer read and write. The `load-acquire` branch hit exactly this.
- Under the split, race builds keep the real release/acquire edge, so genuine races in user code are still caught, and
  both paths implement the same contract (release on commit).
- The compiler treats a call into assembly as opaque, so earlier stores (the caller's ring-buffer writes) cannot move
  past it, and it cannot be inlined.
- The stub adds one call per commit. That cost has not been measured; there is about 2.8 ns of headroom on `SP SC`.

`make test` runs only with `-race`, so it would never exercise the stub. The Makefile needs a non-race pass as well
(e.g., `go test -short ./...` and `TestEndToEnd` without `-race`). Verify the split by running `TestEndToEnd` under
`-race` with and without the build tag: false positives should appear only without it. arm64 would need its own `STLR`
stub, which the M5 result suggests is worth much less.

**Rejected alternatives:**

- **`go:linkname` into `internal/runtime/atomic`** (the `load-acquire` branch). The Go 1.27 linker rejects it outright:
  `link: main: invalid reference to internal/runtime/atomic.StoreRel64`. It links only with
  `-ldflags=-checklinkname=0`, which a library cannot impose on the programs that import it.
- **A plain Go store behind `amd64 && !race`.** It costs nothing and does not affect race builds, but it is a data race
  under the Go memory model. It is safe only while `Commit` is never inlined. Profile-guided optimization can
  devirtualize the hot `Sequencer` interface call and inline it, and the compiler could then legally move the caller's
  ring-buffer writes past the store. That would fail only in a user's PGO build, never in our tests.
- **First-class release/acquire in `sync/atomic`.** Still not available as of Go 1.27: `atomic.Int64` offers only
  `Add`, `And`, `CompareAndSwap`, `Load`, `Or`, `Store`, and `Swap`.

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
