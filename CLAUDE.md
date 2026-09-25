# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
make build          # run tests then compile
make test           # go test -timeout=1s -short -race -covermode=atomic ./...
make test.long      # go test -run TestEndToEnd -timeout=120s -race -covermode=atomic -v ./...
make benchmark      # go test -bench=. -benchmem
make compile        # go build ./...
go run ./example    # run the example
```

Benchmarks live in `assert_benchmark_test.go`. Unit tests live in `assert_unit_test.go`. Long-running integration tests
live in `assert_integration_test.go` and are guarded by `testing.Short()` — `make test` skips them, `make test.long`
runs them. The test command runs with race detection enabled.

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
  Default capacity: 1024, default wait: `Gosched()` on gate / 500ns sleep on idle / 1ns sleep on reserve. Also contains
  `defaultWaitStrategy` and `defaultDisruptor`.
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
- **`sequencer.go`** — Single-writer sequencer (64B, one cache line). Uses atomic Store for commit. Spins with
  `WaitStrategy.Reserve()` when waiting for consumers to advance. `TryReserve` is non-blocking: single barrier check,
  returns `ErrCapacityUnavailable` if no room.
- **`sequencer_shared.go`** — Multi-writer sequencer (128B, two cache lines). Uses atomic Add for `Reserve` (not CAS —
  irrevocable but scales under contention); `TryReserve` is non-blocking with a single capacity check plus a single CAS
  attempt, returning `ErrCapacityUnavailable` on failure. Uses per-slot commit tracking via
  `committedSlots []atomic.Int32` with round numbers (`sequence >> shift`) to disambiguate laps around the ring buffer.
  `Load` scans committed slots from lower to upper, stopping at the first uncommitted slot. `cachedConsumerSequence` is
  `*atomicSequence` (atomic because multiple writers may update it concurrently). Also implements `sequenceBarrier`
  (the `Load` method).
- **`listener.go`** — Runs a consumer loop (blocks calling goroutine). Has two barriers: `committedBarrier` (how far
  producers have committed) and `upstreamBarrier` (how far the prior handler group has advanced). Uses `WaitStrategy`
  for backpressure. Closed state tracked via `*atomic.Int64` with `stateRunning`/`stateClosed` constants. Close is a
  graceful drain — the listener continues processing committed events before exiting.
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

### Conventions

- Refer to `example/main.go` and the README for current API usage.
- Buffer capacity must be a power of 2.
- Match naming and formatting style with surrounding code (per CONTRIBUTING.md).
- Receiver variable is `this` throughout the codebase.
- Sequences start at -1 (`defaultSequenceValue`).
