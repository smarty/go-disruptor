## TODO Items:

Each item says what to do, why, and what result would settle it. Performance items are judged with the method in
`bench/README.md` (reserved cores, turbo off, shuffled rounds, `benchstat`), and their results go into `FINDINGS.md`,
plus `CLAUDE.md` if the change is adopted. Items tagged FUTURE are worth doing but are not blocking anything.

## Release

### TODO: Tag `v0.6.0` with release notes

Everything since `v0.5.0` is unreleased. Create an annotated tag (`git tag -a`), with notes covering:

- **Breaking:** `New()` now requires `Options.BufferCapacity`. The old default of 1024 is gone, because raising it would
  have silently corrupted applications whose own ring buffer is 1024 slots. Callers that relied on the default must
  pass `Options.BufferCapacity(1024)` to keep the old behavior; 65536 is the recommended value.
- Release-store `Commit` on amd64: single writer 37-44% faster, reserved.
- `BusySpinWaitStrategy` (opt-in), the empty-poll `Load`, and batching guidance for multiple writers.

Done when the tag exists on the release commit and the notes name the migration step for the capacity change.

### TODO: Refresh the README's benchmark tables

The "Benchmarks" section still shows Go 1.25 numbers from before the release store (single writer 6.5 ns; it is now
about 3.7 ns with turbo and 4.7 ns without). Rerun the listed scenarios with `bench/suite` or `bench/interleave`,
state the conditions (CPU, Go version, turbo, reserved or not), and replace the tables. Consider adding a busy-spin
column and the wake-latency p50/p99, since latency is the Disruptor's selling point and the tables only show
throughput. Done when every number in the README has a stated method and date.

## Robustness

### TODO: A `Reserve` blocked on a full ring hangs forever after `Close`

Both sequencers' slow paths (`waitForConsumers`) loop until the consumers advance, with no exit. `Close()` makes the
listeners drain and return, so after that nothing ever advances, and a producer waiting in `Reserve` on a full ring
spins or sleeps forever. The shared sequencer is worse: its `reservedSequence.Add` is irrevocable, so it cannot back
out of a reservation the way Java's CAS-based `MultiProducerSequencer.next()` can. This is the most dangerous issue in
the codebase, because a shutdown under load can leave goroutines stuck permanently, with no signal.

What we are looking for:

1. A test that reproduces the hang for both sequencers (fill the ring, close, call `Reserve`, and assert that it
   returns within a deadline). It should fail today.
2. A way out: most likely `Reserve` observing closed state in its slow path and returning a new sentinel (say,
   `ErrClosed`), plus an optional timeout or cancellation. For the shared sequencer, decide what happens to slots
   already claimed by `Add`: consumers must never wait on a reservation that will never be committed.
3. Proof that the fast path is untouched: `bench/compare` shows no regression on the single-writer and multi-writer
   rows (the check belongs only in the slow path).

This also closes the "Blocking Reserve timeout / cancellation" gap in the Java comparison below.

### TODO: No panic recovery in handlers

If a `Handler.Handle()` panics, it kills the goroutine. `defer waiter.Done()` in `compositeListener.Listen()` fires, but
the panic propagates and crashes the process. Java LMAX has an `ExceptionHandler` on `BatchEventProcessor` that
catches exceptions, invokes a callback, and optionally continues. Here, a single misbehaving handler takes down
everything with no hook point.

What we are looking for: an option (for example, `Options.PanicHandler(func(recovered any, lower, upper int64))`) that
recovers, reports the batch, and lets the application choose whether to skip the batch or stop the disruptor. Decide
how a stopped listener releases downstream groups and blocked producers (see the `Reserve` hang above). The recovery
must not cost anything measurable per batch; `recover` in a deferred function on the `Listen` loop, not per call, is
the likely shape.

### TODO: Error sentinels are bare `int64` values

`ErrReservationSize = -1` and `ErrCapacityUnavailable = -2` are plain `int64` constants, not `error` values. A caller
that forgets to check the return value will use -1 or -2 as a sequence number, silently indexing the ring buffer at
`(-1 & mask)` or `(-2 & mask)` and corrupting data with no signal. Java throws `InsufficientCapacityException`.

What we are looking for: an API that makes the mistake hard, weighed against the hot path. Options include a
`(sequence int64, err error)` return (measure the cost in `bench/compare`; it may be free once inlined), or a separate
checked method. This is a breaking change, so it should be decided before `v1.0.0`.

## Performance

### TODO: Escalating idle sleep for the default wait strategy

`BenchmarkIdleCPU` showed that an idle default consumer burns 0.55 cores (0.84 for two consumers): `time.Sleep(500ns)`
keeps the Go scheduler's threads spinning, while `time.Sleep(50us)` costs 0.02 cores. Meanwhile the default's p99 wake
latency after a 50us idle gap is already about 1ms, because Go parks a thread waiting on a sub-millisecond timer in
`epoll` with a 1ms timeout (`FINDINGS.md` section 6a).

What we are looking for: an `Idle` that keeps short sleeps for the first few calls of a wait and then escalates to
tens of microseconds. Success is idle CPU cut by an order of magnitude (`BenchmarkIdleCPU`), with wake-latency p50 and
p99 no worse than today after short gaps and not meaningfully worse after long ones (`BenchmarkWakeLatency`), and no
throughput change (`bench/compare`). The phased backoff tried earlier failed because it spun *before* sleeping; this
changes only what happens after the consumer has already been idle for a while.

### TODO: Producer-triggered wake-up for low latency without a dedicated core

The default strategy wakes in 1.5-2.2us at p50 and up to 1ms at p99; `BusySpinWaitStrategy` wakes in a few hundred
nanoseconds but costs a core per waiting goroutine. A third option is for the consumer to block (on a futex, a
channel, or `sync.Cond`) once idle, and for the producer to wake it when it commits into an empty ring.

What we are looking for: a design where the producer pays only when a consumer is actually parked (one atomic load of
a "sleeping" flag on the common path), and where no wake-up can be lost between the consumer's last empty check and
its parking. Judge it on wake-latency p50/p99 after long gaps against the default, idle CPU against busy-spin, and the
cost on `Commit` in `bench/compare`. It may belong in a new wait strategy (or need a new hook, since `WaitStrategy`
has no producer-side signal today).

### TODO: Busy-spin's cost on the shared sequencer with one writer

With `BusySpinWaitStrategy`, the shared sequencer driven by one writer is 57% slower than with the default strategy
(15.4 vs 9.9 ns, reserved). The empty-poll `Load` removed the worst of it (the consumer no longer reads
`reservedSequence` when the ring is empty), but the consumer still polls the `committedSlots` marker line that the
writer stores to. What we are looking for: `perf` counters (`l2_rqsts.rfo_miss`, `mem_load_l3_hit_retired.xsnp_fwd`)
confirming where the remaining cost is, then either a fix (for example, backing off the spin when a poll finds
nothing several times in a row) or a documented limit. Four writers at 16 slots already break even, so this matters
mostly for single-writer use of the shared sequencer.

### TODO: Writer batch-boundary false sharing

`RingBuffer MP SC/R4` (four writers, 16-slot batches of 64B entries) ran 15-18% *faster* with the hardware prefetchers
off, replicated (`FINDINGS.md` section 5). The hypothesis is that each writer's prefetcher fetches the neighbouring
writer's batch lines within the page and takes them away. What we are looking for: store-miss counters with the
prefetchers on and off, attributed to the writer's entry store with precise sampling (`:pp`), and a sweep of batch
size and entry alignment. If confirmed, the fix is probably guidance for applications (pad or align batches) rather
than a library change.

### FUTURE: Cache-line layout on 128B and 32B platforms

`defaultSequencer` is annotated as "64B total — one cache line" and `sharedSequencer` as "128B — two cache lines." On
Apple Silicon (128B lines), both structs fit in a single cache line, so the hot/cold field separation in
`sharedSequencer` provides no isolation. On 32B platforms (ARM32, MIPS), `defaultSequencer` spans two cache lines and
the hot fields straddle the boundary. The code is correct everywhere, but the layout optimization only works as
intended on x86. What we are looking for: padding driven by `CacheLineBytes`, measured on the M5 laptop to show it
matters before adding it.

## Platform confirmation

### TODO: Measure the arm64 spin hint

`BusySpinWaitStrategy` executes eight `YIELD`s on arm64 (`spin_hint_arm64.s`), mirroring the eight `PAUSE`s that made
busy-spin match the default's throughput on amd64. It is unmeasured, and `YIELD` may retire almost immediately on
Apple cores, which would make the spin far tighter than intended. What we are looking for, on the M5 laptop: run
`BenchmarkWakeLatency`, `BenchmarkBusySpin`, and the default-strategy counterparts (`^BenchmarkSequencer$/SP_(SC|MC)$`,
`SharedSequencer/SP`) with `-count 6`, for 1, 8, and 32 `YIELD`s and for `ISB` (Rust's arm64 spin hint), and pick the
variant whose throughput is closest to the default without giving up sub-microsecond wake-ups. `bench/interleave` and
`bench/reserve` are Linux-only, so use plain `go test` and `benchstat` there.

### FUTURE: arm64 `LDAPR` for barrier loads

amd64 now commits with `storeRelease`, a one-instruction assembly `MOVQ` behind `//go:build amd64 && !race` (37-44%
faster single writer; see `FINDINGS.md` section 6). arm64 needs no equivalent: Go already compiles `atomic.Int64.Store`
to `STLR` (a release store) and `Load` to `LDAR` (`internal/runtime/atomic/atomic_arm64.s`). The one remaining lever is
the load side: `LDAR` is RCsc, so it cannot complete ahead of the same core's earlier `STLR`, while `LDAPR` (ARMv8.3,
RCpc, present on Apple M-series) can. That matters only where one goroutine both commits and loads (for example, a
producer reading the consumer barrier right after a commit). The older `load-acquire` prototype measured ~3-5% on
Apple M5, but it predates the batch markers and its source of gain was never isolated. Measure before adding a stub.

### TODO: Confirm on Zen 2

Everything adopted since 2026-09-26 was measured only on the i7-12700K: the release-store `Commit`, the empty-poll
`Load`, and `BusySpinWaitStrategy`. The earlier Performance Findings came from the Threadripper 3970X (Zen 2), where
`XCHG` was 51-58% of single-writer cycles, so the release store should help at least as much there. What we are
looking for: `bench/compare` against the commit before each change, and `bench/suite baseline`, on the Threadripper.
Both scripts currently hard-code the i7-12700K's CPU layout: `bench/suite` accepts the CPU options, but
`bench/compare` needs them added first. Multi-producer rows there are ±10-25% across CCXs, so only large, replicated
deltas count.

### TODO: Pin benchmark producers and consumers to chosen cores

`bench/interleave` pins each benchmark set to a CPU list with `taskset`, but inside that list the Go scheduler decides
which CPU runs each producer and consumer, and the choice changes from run to run. On the Threadripper this likely
accounts for most of the multi-producer spread (±10-25%): whether a writer and the consumer share a CCX changes the
cost of every cache-line transfer. It also blocks the most direct test of the Zen 2 release-store regression (an
unreserved pilot on 2026-09-28 had `c104c6e` → `41f7bc0` making `Sequencer/SP SC` 19% slower on one CCX and ~40%
slower across CCXs, while `Commit` alone got 65% faster): same CCX versus the next CCX, on purpose.

The likely shape:

- Producers are the benchmark's own goroutines (`benchmarkDisruptorWith`), so each pins itself before its loop:
  `runtime.LockOSThread()`, then `sched_setaffinity` on its own thread via `syscall.RawSyscall` (Linux only, behind a
  build tag with a no-op elsewhere; no `golang.org/x/sys`, to keep the module dependency-free).
- Consumers run on goroutines the library owns (the benchmark goroutine for a single handler, `compositeListener`'s
  goroutines for several), so the handler pins itself on its first `Handle` call. No library API change.
- Opt-in, for example `DISRUPTOR_BENCH_PIN='producers=0,1,2,3;consumers=4,5'`. Unset, every benchmark behaves exactly
  as today, so existing rows stay comparable with earlier runs.
- Fail the benchmark on a pinning error rather than silently running unpinned. `sched_setaffinity` returns `EINVAL`
  for a CPU outside the set's `taskset` mask.

The caveat to measure, not assume: a locked goroutine can only run on its own thread, so the default strategy's
`Gosched()` (`Gate`) and `time.Sleep` (`Idle`) must hand it back to that exact thread (`startlockedm`, a futex wake)
instead of letting any idle thread pick it up. Pinned default-strategy rows may therefore measure a configuration
applications do not run. `BusySpinWaitStrategy` never yields, so pinning it reflects the hardware faithfully.

Done when: (1) the pinned layout is selectable per `--set` (or per variant) in `bench/interleave`; (2) a pinned run
of `c104c6e` against `41f7bc0`, busy-spin and default, with the consumer on the producer's CCX and then on the next CCX,
says whether the Zen 2 regression is an intra-CCX or an Infinity Fabric effect; (3) repeating the multi-producer rows
pinned shows whether their spread narrows enough to trust smaller deltas; and (4) the default-strategy cost of pinning
is measured once (pinned against unpinned, same CPUs) and recorded in `FINDINGS.md`.

## Research questions

### FUTURE: Open questions from the Intel investigation

These do not block anything, but each would sharpen a Performance Finding. Details are in `FINDINGS.md`.

- Why was `SP4MC` *slower* with 128B sequence padding (+3.0% and +7.8% in two runs), when nothing else moved?
- With the prefetchers off, which cache level serves the `committedSlots` lines to the consumer?
- What is the mechanism behind `MP MC/R4` running about 9% slower without prefetching? No counters were collected.
- Is MSR `0x1a4` bit 5 really Intel's "AMP" prefetcher, and what does bit 4 control on Golden Cove?
- Why does a spin phase before sleeping send more wake-ups into Go's 1ms `epoll` path? The explanation (the runtime's
  other threads have parked by the time the consumer finally sleeps) is plausible but unverified; a runtime trace
  (`go tool trace`) of both strategies would show it.

## Features

### FUTURE: Gaps compared to Java LMAX

| Feature                                                  | Java LMAX                             | This port                                        |
|----------------------------------------------------------|---------------------------------------|--------------------------------------------------|
| **WorkerPool** (events partitioned across handlers)      | Yes                                   | No—fan-out only (every handler sees every event) |
| **Handler panic/exception recovery**                     | `ExceptionHandler` callback           | Process crash (see Robustness)                   |
| **Dependency DAGs**                                      | Arbitrary via `SequenceBarrier`       | Linear pipeline only                             |
| **Blocking Reserve timeout / cancellation**              | Via `WaitStrategy` + `AlertException` | Blocks forever, no exit (see Robustness)         |
| **`SequenceReportingEventHandler`** (mid-batch progress) | Yes                                   | Always publishes full batch                      |
| **EventTranslator** (structured publish API)             | Yes                                   | User manages ring buffer directly                |

The biggest functional gap is **WorkerPool**: many real-world uses need work distribution (one event to one handler),
not just fan-out. What we are looking for first is a design that keeps the per-event cost of fan-out unchanged; a
work pool needs a claim per event (or per batch) among its workers, which is a new contention point to measure.
Mid-batch progress would let a handler with a long batch release slots before the batch ends, which matters for small
rings.

### FUTURE: Metrics

- The largest number of events handed to a single `Handle()` call, per handler (a cheap signal of consumer lag).
- Possibly counts of slow-path entries (`Reserve` waits and `Idle` calls), which the capacity investigation had to
  measure with a temporary wait-strategy wrapper.

What we are looking for: metrics that cost nothing when unused (no atomic on the hot path unless enabled).
