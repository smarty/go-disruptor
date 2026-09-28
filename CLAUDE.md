# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
make build          # run tests then compile
make test           # go test -timeout=1s -short -race -covermode=atomic ./..., then again without -race
make test.long      # go test -run TestEndToEnd -timeout=120s -race -covermode=atomic -v ./..., then without -race
make benchmark      # go test -bench=. -benchmem
make compile        # go build ./...
go run ./example    # run the example
```

Benchmarks live in `assert_benchmark_test.go`. Unit tests live in `assert_unit_test.go`. Long-running integration tests
live in `assert_integration_test.go` and are guarded by `testing.Short()` — `make test` skips them, `make test.long`
runs them. Both run every test twice, with and without `-race`, because non-race amd64 builds use the plain release
store (see `store_release_*`) that race builds never compile.

`bench/` holds the tooling behind the Performance Findings: `suite` (builds and runs the standard reserved suite),
`compare` (a git revision against the working tree, reserved), `revisions` (several git revisions against each other,
reserved), `reserve` (reserves cores and disables turbo, as root), `interleave` (shuffled rounds of prebuilt test
binaries, then `benchstat`), `msr-prefetch` (Intel prefetcher controls, with the measured Golden Cove bit map), and
`bench/prefetch` (the `PairChase` positive control for prefetcher experiments). `bench/README.md` documents the method,
including `perf` event choices and why attribution must use precise (`:pp`) sampling. Performance claims should come
from that method, with `environment.txt` kept alongside.

## Architecture

This is a Go port of the [LMAX Disruptor](https://github.com/LMAX-Exchange/disruptor) — a lock-free, high-performance
message passing pattern between goroutines. Zero external dependencies; module is `github.com/smarty/go-disruptor`
(Go 1.25).

### Core Flow

```
Producer → Sequencer.Reserve(N) → write to ring buffer → Sequencer.Commit(lower, upper)
                                                                        ↓
                                              Listener goroutines poll committed sequences
                                                        ↓
                                              Handler.Handle(lower, upper) processes events
```

The library does NOT manage the ring buffer — the application creates its own (typically a package-level fixed-size
array) and indexes into it using `sequence & mask`.

### Key Components

- **`interfaces.go`** — Core interfaces and types:
  - `Disruptor` (embeds `Sequencer` + `ListenCloser`)
  - `Sequencer`: `Reserve(uint32) int64`, `TryReserve(uint32) int64`, `Commit(lower, upper int64)`
  - `ListenCloser`: `Listen()` + `io.Closer`
  - `Handler` (consumer callback) with `Handle(lowerSequence, upperSequence int64)`
  - `WaitStrategy`: `Gate(int64)`, `Idle(int64)`, `Reserve(int64)`
  - Error sentinels: `ErrReservationSize` (-1), `ErrCapacityUnavailable` (-2)
- **`config.go`** — `New()` constructor returns `(Disruptor, error)`. Functional options via `Options` singleton.
  `BufferCapacity` is required (no default since v0.5.0's 1024; 65536 recommended). Default wait: `Gosched()` on gate /
  500ns sleep on idle / 1ns sleep on reserve. Also contains `defaultWaitStrategy` and `defaultDisruptor`.
- **`wait_strategy_spin.go`, `spin_hint_*`** — `BusySpinWaitStrategy` (exported, opt-in): every wait is one
  `spinHint()`, eight `PAUSE`s on amd64 (eight `YIELD`s on arm64, unmeasured; a no-op elsewhere). Eight, not one: a
  single `PAUSE` let the polling consumer steal the producer's lines often enough to make the single writer ~40%
  slower. Needs a dedicated CPU per waiting goroutine.
- **`sequence.go`** — `atomicSequence`: cache-line-padded `atomic.Int64`. Padding is `[CacheLineBytes - 8]byte` placed
  before the embedded `atomic.Int64` (one-sided padding — the next allocation's leading padding provides the trailing
  separation). `newSequences(count)` over-allocates one extra cache line as a single contiguous `[]byte` and starts the
  sequences at the first aligned offset (deterministic; does not depend on allocator size classes, which do *not*
  yield aligned addresses for some counts on s390x), returning a `[]*atomicSequence` view into it for cache locality
  across multiple sequences. `newSequence()` is `newSequences(1)[0]`. `defaultSequenceValue = -1` is the initial value
  for all sequences.
- **`cpu_padding_*bit.go`** — Platform-specific `CacheLineBytes` constant selected by build tags:
  - `cpu_padding_32bit.go` — 32B for `arm`, `mips`, `mipsle`, `mips64`, `mips64le`
  - `cpu_padding_64bit.go` — 64B for `386`, `amd64`, `arm64` (non-Darwin), `loong64`, `riscv64`, `wasm`
  - `cpu_padding_128bit.go` — 128B for `arm64` (Darwin), `ppc64`, `ppc64le`
  - `cpu_padding_256bit.go` — 256B for `s390x`
- **`store_release_amd64.go`/`store_release_atomic.go`** — `storeRelease(*atomic.Int64, int64)`, the single
  sequencer's commit store. On amd64 without `-race` it is a plain Go store through `unsafe.Pointer` (a `MOVQ`, which
  is a release store under TSO, where `atomic.Int64.Store` is an `XCHG`); everywhere else, including every `-race`
  build, it is `atomic.Int64.Store`. The race detector sees only instrumented `sync/atomic` calls, so the plain store
  must stay behind `!race` (a race build would report it as a data race). It inlines into `defaultSequencer.Commit`,
  which is `//go:noinline`: that call boundary is the only thing keeping the compiler from moving the caller's
  ring-buffer writes past the store (PGO would otherwise inline a hot `Commit`, even after devirtualizing it). Never
  remove the `//go:noinline`, and never call `storeRelease` from a function that can be inlined. It replaced a
  one-instruction assembly stub whose ABI0 call cost more than the `XCHG` on Zen 2 (see Performance Findings). The
  shared sequencer does not use it.
- **`sequencer.go`** — Single-writer sequencer (64B, one cache line). Commits with `storeRelease`. Spins with
  `WaitStrategy.Reserve()` when waiting for consumers to advance. `TryReserve` is non-blocking: single barrier check,
  returns `ErrCapacityUnavailable` if no room.
- **`sequencer_shared.go`** — Multi-writer sequencer (128B, two cache lines). Commits with a sequentially consistent
  `atomic.Int64.Store` (`XCHG`), not `storeRelease` (see Performance Findings). Uses atomic Add for `Reserve` (not CAS —
  irrevocable but scales under contention); `TryReserve` is non-blocking (lock-free CAS loop, like Java's `tryNext`): a
  lost CAS is contention, not a full buffer, so it re-checks capacity and retries, returning `ErrCapacityUnavailable`
  only when capacity is genuinely exhausted. Tracks commits with one marker per *batch* in
  `committedSlots []atomic.Int64`: `Commit(lower, upper)` stores `upper` in the slot of `lower`, and `Load` jumps from
  marker to marker, stopping at the first slot whose marker is `< lower` (stale from an earlier lap, or the initial -1;
  int64 sequences never wrap, so a marker can go unrewritten for any number of laps). This relies on every `Load`
  starting at a batch boundary, which holds because callers only pass `handledSequence+1` (see the struct comment).
  `Load` advances single-slot batches with `lower++` behind a branch, not `lower = marker+1`, to avoid a serial
  dependent-load chain (see Performance Findings). `Load` reads the first marker before `reservedSequence` and returns
  at once when it is stale, so an empty poll never touches the writers' contended line. `cachedConsumerSequence` is
  `*atomicSequence` (atomic because multiple writers may update it concurrently). Also implements `sequenceBarrier`
  (the `Load` method).
- **`listener.go`** — Runs a consumer loop (blocks calling goroutine). Has two barriers: `committedBarrier` (how far
  producers have committed) and `upstreamBarrier` (how far the prior handler group has advanced). For the first group
  both are the same barrier, so `gated` is false and the empty path skips the committed-barrier (`Gate`) check; the
  drain re-check after observing Close still runs for every group. Uses `WaitStrategy` for backpressure. Closed state
  tracked via `*atomic.Int64` with `stateRunning`/`stateClosed` constants. Close is a graceful drain — the listener
  continues processing committed events before exiting.
- **`listener_composite.go`** — Slice type (`type compositeListener []ListenCloser`). Manages multiple listeners with
  WaitGroup coordination. Constructor unwraps single-element slices to avoid indirection.
- **`sequence_barrier.go`** — `sequenceBarrier` interface (`Load(int64) int64`) and `atomicBarrier`: single-sequence
  optimization, avoids iteration overhead of `compositeBarrier`.
- **`sequence_barrier_composite.go`** — Slice type (`type compositeBarrier []*atomicSequence`). Returns minimum
  sequence across multiple atomic sequences. Constructor collapses zero-sequence to an empty barrier and single-sequence
  to an `atomicBarrier`.

### Lifecycle

`New()` returns `(Disruptor, error)` — validates capacity (power of 2) and requires at least one handler group. Returns
a `defaultDisruptor` struct that embeds `ListenCloser` + `Sequencer`. Call `Reserve`/`Commit`/`TryReserve` directly on
the `Disruptor`. Call `Listen()` to start consumers (blocks the calling goroutine). Call `Close()` to signal graceful
shutdown — listeners drain all committed events before `Listen()` returns.

`Options.WriterCount(n)` selects the sequencer: `n <= 1` uses `defaultSequencer` (single producer), `n >= 2` uses
`sharedSequencer` (multi-producer, goroutine-safe). Capacity uses `uint32` throughout (`Options.BufferCapacity(uint32)`,
`Reserve(uint32)`).

### Handler Groups

Handler groups form a pipeline: each group must fully process sequences before the next group can consume them. Within
a group, each handler runs on its own goroutine and sees every message (fan-out). This is configured via multiple
`Options.NewHandlerGroup(...)` calls. `compositeListener` is used at two levels: within a group (one listener per
handler) and across groups (one composite per group).

### Cache Line Padding

False sharing (two goroutines writing to different variables on the same cache line) is a major source of latency in
lock-free code. The codebase uses explicit padding to prevent it:

- **`CacheLineBytes` constant** — Platform-specific, selected by build tags in the `cpu_padding_*bit.go` files. The
  value is 64B on amd64/arm64-linux but is 128B on arm64-Darwin (Apple Silicon) and other architectures, so padding
  declarations must always reference the constant rather than hard-coding 64.
- **`atomicSequence`** — The core shared counter. Wraps `atomic.Int64` with one-sided `[CacheLineBytes - 8]byte`
  padding (a single leading pad; the trailing pad is supplied by the next sequence's leading pad in a contiguous
  layout). `newSequences()` over-allocates and offsets to a cache-aligned address, ensuring the counter sits alone on
  its own cache line.
- **Struct field layout** — Structs like `defaultSequencer` (64B, one cache line) and `sharedSequencer` (128B, two
  cache lines) annotate each field with its size and access frequency. Fields are ordered by hot-path access pattern,
  with hot fields on the first cache line and slow-path fields on the second. When modifying these structs, preserve
  the size/access annotations and keep fields within their cache line boundaries.

### Performance Findings

Measured 2026-09-25 on a Threadripper 3970X (Zen 2), boost off, benchmarks pinned to reserved cores with all other
userspace and IRQs moved off them, variants interleaved in shuffled order across 10 rounds and compared with benchstat.
Single-producer results were stable to ±0-3%; multi-producer (4 writers across two CCXs) is inherently ±10-25% because
goroutine placement across CCXs varies per run, so only large, replicated multi-producer deltas mean anything.

- **Adopted — `Reserve` fast path shape.** The Java-derived `cached <= previousReservedSequence` check is always true
  in this port (Java needs it only for `claim()`/rewind, which does not exist here). Removing it *and* moving the spin
  loop into a separate `waitForConsumers` method made single-producer end-to-end benchmarks 6.3-6.5% faster
  (replicated). Removing the check alone gave only ~2.5% (the compiler also flipped the branch layout). The isolated
  `Sequencer/Reserve` micro-benchmark gets ~2.7% *slower* either way; trust the end-to-end benchmarks.
- **Rejected — 128B padding on amd64** (to defeat the adjacent-line prefetcher, as Java LMAX does on x86). A -9.5%
  result on one multi-producer benchmark did not replicate in a second run; everything else was neutral on Zen 2.
  Also rejected on Intel (i7-12700K, 2026-09-26, one thread per P-core, turbo off, P-cores reserved, 10 shuffled
  rounds): no row improved, and `SP4MC` was *slower* in two independent runs (+3.0% unreserved, +7.8% reserved;
  geomean +1.5%). Mostly this is by construction: `newSequence()` makes a 128B `[]byte`, which Go's 128B size class
  places 128B-aligned, so every single sequence already owns its whole 128B pair at 64B padding. Only
  `newSequences(n >= 2)` (the handler sequences) can share a pair, and only about half the time (a 192B allocation
  alternates between 0 and 64 mod 128). Even then, the spatial prefetcher runs only on L2 fills, and a consumer's own
  line rarely misses because the producer reads it only on the full-buffer slow path.
- **Rejected — shared `Load` scanning up to `lower+capacity-1` instead of reading `reservedSequence`** (to avoid
  reading the writers' contended cache line). Mixed: single-consumer 7-10% slower, multi-consumer ~4% faster; an
  earlier unpinned run was 16-35% slower. Likely the consumer reads ahead into slot lines that writers are storing to.
- **The correctness fixes** (listener drain re-check, alignment) measured no change. `TryReserve` has no benchmark.
- **Adopted — one commit marker per batch in the shared sequencer** (was one atomic store per slot in `Commit` and
  one load per slot in `Load`). Multi-producer reserve-16 is 19-25% faster (replicated across three runs); reserve-1
  is unchanged, and the shared sequencer with one writer is 2.7% faster. The first version stored a 32-bit round
  number plus length, which is only safe when every slot is rewritten every lap; with markers, interior slots may never
  be rewritten, so the round wrapped after 2^32 laps (~4 hours at 300M events/s on a 1024-slot ring) and falsely
  matched. Storing the batch's `upper` sequence fixes it (`TestSharedLoad_RejectsMarkerStaleForManyLaps`).
- **Dependent-load chains matter.** With `lower = marker+1`, each slot's address depends on the previous load, so the
  consumer's scan becomes serial cross-core fetches: reserve-1 was 5.6% *slower* than per-slot commits (and 9.1% with a
  16K ring, which disproved an L2-footprint explanation). Advancing single-slot batches with `lower++` behind a
  predictable branch (verify there is no `CMOV` in the assembly) restored parallel loads and turned it into a 2.7% win.
- **Rejected — concrete dispatch** (`New()` returning structs that embed `*defaultSequencer`/`*sharedSequencer`
  instead of the `Sequencer` interface, removing one indirect call and inlining `Commit` into the wrapper). 8.5% fewer
  instructions but 7.2% more cycles, so single-producer was 7% slower (replicated). Code alignment was ruled out. The
  single-writer path is bound by `Commit`'s `XCHG` (51-58% of cycles in `perf`); shortening the work before the
  barrier cannot help. Untested on Intel.
- **Seq-cst stores are not free on x86.** A Go `atomic.Int64.Store` compiles to `XCHG` (a full barrier, ~19 cycles on
  Zen 2) and dominates single-writer throughput. Only seq-cst *loads* are plain `MOV`s on x86.
- **Adopted — release-store `Commit` on amd64** (`storeRelease`, an assembly `MOVQ` behind `amd64 && !race`). On the
  i7-12700K (P-cores reserved, turbo off, 10 shuffled rounds, `bench/compare`): single writer 37-44% faster (`SP SC`
  8.93 → 5.00ns, `SP MC` 8.06 → 5.04ns), `SP4MC` 23%, ring buffer with data `SP SC/R1` 44% faster (10.46 → 5.86ns),
  shared sequencer with one writer 32% faster, and every multi-producer row 2-5% faster (all p=0.000-0.005). Base
  single-producer variance of ±10-35% fell to ±0-2%. In an unreserved pilot, an unsafe inline plain store (the upper
  bound) beat the stub by only another ~10 points, so the non-inlinable call keeps about three quarters of the gain.
  Untested on Zen 2.
- **Rejected — phased-backoff default wait strategy; adopted — opt-in `BusySpinWaitStrategy`.** Spinning before the
  default's sleep made dense traffic far faster but latency after idle gaps 2-8x worse (50us gap: median 1.1us to
  648us): Go parks a thread waiting on a sub-millisecond timer in `epoll` with a 1ms timeout, so no sleep-based idle
  phase can promise sub-millisecond wake-ups. Busy-spin (P-cores reserved, turbo off, 10 rounds) wakes in 261-413ns at
  p50 and 296-578ns at p99 at every gap, against the default's 1.5-2.2us p50 and 9us-1.04ms p99; a 1K ring at 16
  slots runs 13x faster; the single writer is 5-16% slower, four writers 16% slower at R1 and even at R16, and the
  shared sequencer with one writer 57% slower (polling contention). `BenchmarkWakeLatency` and `BenchmarkBusySpin`
  measure it. CPU (`BenchmarkIdleCPU`): busy-spin costs exactly 1 core per waiting goroutine, but the default is not
  free either: an idle default consumer uses 0.55 cores (0.84 for two), because Go's `time.Sleep(500ns)` keeps
  scheduler threads spinning (`Sleep(50us)` costs 0.02 cores).
- **Adopted — shared `Load` returns early on an empty poll** (first marker stale, so `reservedSequence` is never read).
  With busy-spin, the shared sequencer with one writer went from 25.5 to 11.5ns (pilot). With the default strategy,
  reserved (`bench/compare`, 10 rounds): `MP MC/R1` -4.8%, `MP MC/R4` -7.3%, other multi-producer rows unchanged, the
  shared sequencer driven by one writer +1.6%, geomean -2.2%; an unreserved pilot also showed the 1K ring 28% faster.
  Single-writer rows got 5-9% faster (and `Sequencer/Commit` 2.3% slower) between the two binaries with no code
  change: treat that size as layout noise.
- **Adopted — `BufferCapacity` required, 64K recommended (the default was 1024).** A ring must hold more events than
  producers publish while a sleeping consumer wakes: `time.Sleep(500ns)` in `Idle` takes ~1.9us median and ~9us p99 on
  the i7-12700K. At 1K slots a batching single writer (R16) was 23x slower than at 16K (7.9ns vs 0.34ns), with consumers
  sleeping ~33,000 times per million events instead of ~20. Every shape plateaus by 64K; the shared sequencer at R1 is
  still 19% faster at 64K than 16K, while 256K adds only 1-7% for 2MB of `committedSlots`. Single-writer R1 is
  capacity-insensitive (the consumer is the slower side, so the ring stays full). Measured pinned but unreserved, turbo
  on, 3 runs; the deltas dwarf the noise. Rather than raising the default, `New()` now requires the capacity: an
  application that omitted it sized its own ring buffer at 1024, and a larger default would let writers silently
  overwrite unconsumed entries. Failing fast in `New()` is the only safe way to change it.

### Conventions

- Refer to `example/main.go` and the README for current API usage.
- Buffer capacity must be a power of 2.
- Match naming and formatting style with surrounding code (per CONTRIBUTING.md).
- Receiver variable is `this` throughout the codebase.
- Sequences start at -1 (`defaultSequenceValue`).
