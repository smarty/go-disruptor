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

### Benchmark 128B sequence padding on Intel

Intel's L2 spatial ("adjacent-line") prefetcher fetches cache lines in 128B-aligned pairs, so two `atomicSequence`
values on neighboring 64B lines can still interfere even though each sits alone on its own line. `newSequences()`
allocates all handler sequences contiguously, so adjacent consumers share a 128B pair. The separately allocated
producer sequences (`newSequence()`) may also land next to one another, depending on the allocator. Java LMAX pads to
128B on x86 for this reason. On a Threadripper 3970X (Zen 2) the change was neutral: one -9.5% multi-producer result
did not replicate (see *Performance Findings* in `CLAUDE.md`). It has never been measured on Intel, where the
prefetcher is documented. Run it on the i7-12700K.

**The experiment** is a single constant. `CacheLineBytes` is only used by `atomicSequence` padding and alignment in
`sequence.go`, so changing it to `128` in `cpu_padding_64bit.go` changes nothing else. If it wins, don't ship it that
way: that file also covers arm64-linux, loong64, and riscv64, and the exported constant would no longer mean "cache line
size". Introduce a separate, amd64-only padding constant for `atomicSequence` instead.

**What to watch:** mainly the multi-consumer rows (`SP MC`, `SP4MC`, `MP MC/*`), where adjacent consumer sequences
are guaranteed to share a 128B pair; also the multi-producer rows, where any benefit depends on where the allocator put
the producer sequences.

**Method.** Build both variants as test binaries once, so nothing compiles during timed runs. Pin the benchmarks to
physical P-cores, turn turbo off, interleave the variants in shuffled order across rounds so machine-state drift spreads
evenly across them, and compare with `benchstat` (`go install golang.org/x/perf/cmd/benchstat@latest`):

```bash
git worktree add ../go-disruptor-pad128 HEAD
sed -i 's/const CacheLineBytes = 64/const CacheLineBytes = 128/' ../go-disruptor-pad128/cpu_padding_64bit.go
go test -c -o /tmp/base.test . && (cd ../go-disruptor-pad128 && go test -c -o /tmp/pad128.test .)

echo 1 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo  # restore with 0 afterwards
rm -f /tmp/base.txt /tmp/pad128.txt  # results are appended across rounds
for _round in $(seq 10); do
	for _variant in $(printf 'base\npad128\n' | shuf); do
		taskset -c "${_single_producer_cpus}" "/tmp/${_variant}.test" -test.run '^$' -test.count 1 -test.cpu 4 \
			-test.bench 'Sequencer/(SP|Reserve|NextWrapPoint|Commit)' >> "/tmp/${_variant}.txt"
		taskset -c "${_multi_producer_cpus}" "/tmp/${_variant}.test" -test.run '^$' -test.count 1 -test.cpu 8 \
			-test.bench 'SharedSequencer/MP' >> "/tmp/${_variant}.txt"
	done
done
benchstat /tmp/base.txt /tmp/pad128.txt
git worktree remove ../go-disruptor-pad128
```

**Choosing CPUs.** The 12700K is hybrid, with 8 P-cores (hyperthreaded) and 4 E-cores. Never let a benchmark land on an
E-core. Read the layout from `lscpu -e=CPU,CORE,MAXMHZ`: P-core threads report the higher `MAXMHZ`, and SMT siblings
share a `CORE` value. Use one thread per physical P-core: 4 P-cores for `_single_producer_cpus`, all 8 for
`_multi_producer_cpus` (the multi-producer benchmarks keep 5-6 goroutines busy).

**Reserving the cores** matters more than pinning. `taskset` only keeps the benchmark on its cores; it doesn't keep
other processes off them. To move everything else away, run
`systemctl set-property --runtime <unit> AllowedCPUs=<other cpus>` for `system.slice`, `init.scope`, and `user.slice`.
Then launch the benchmark through `sudo systemd-run --slice=bench.slice -p AllowedCPUs=<benchmark cpus> --uid=<you>`,
because a session inside `user.slice` is confined along with everything else. Undoing this has two traps:

- Clearing `AllowedCPUs=` leaves already-running processes with their narrowed affinity. Set it to all CPUs *first*,
  then clear it.
- Even that doesn't reach processes under the per-user managers (`user@UID.service`), which have no cpuset delegated.
  Reset their threads directly with `taskset -cp <all cpus> <tid>`, as root for other users' processes.

**Reading the results.** Single-producer rows were stable to ±0-3% on the Threadripper when the cores were reserved.
Multi-producer rows are noisier. Only trust a multi-producer delta that survives a second, independent run.

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

`sharedSequencer.committedSlots` is the only Commit→Load ordering edge in the shared producer (as is `committedSequence`
for the single producer), but is currently typed as `[]atomic.Int64`—every `Store`/`Load` pays sequential-consistency
cost. (Since the prototype below, the shared sequencer moved from one `atomic.Int32` per slot to one `atomic.Int64`
marker per batch, so the prototype would need to be redone with 64-bit operations.) The pairwise Commit→Load
relationship only requires release/acquire. The `load-acquire` branch (commit `bc78c07`) prototypes this by changing the
field to `[]uint32` and reaching into the runtime via `go:linkname` to call `internal/runtime/atomic.LoadAcq` /
`StoreRel` directly:

```go
//go:linkname loadAcq32 internal/runtime/atomic.LoadAcq
func loadAcq32(ptr *uint32) uint32

//go:linkname storeRel32 internal/runtime/atomic.StoreRel
func storeRel32(ptr *uint32, val uint32)
```

Measured ~3-5% throughput improvement on the single-slot Reserve path on Apple M5 (ARM64, weakly-ordered); negligible on
batched `ReserveMany` paths because the per-slot Commit/Load loop is amortized. On x86 (TSO), seq-cst *loads* are
already plain `MOV`s, but seq-cst *stores* are not: Go compiles `atomic.Int64.Store` to `XCHG`, a full barrier, and
`perf` on a Zen 2 showed `Commit`'s `XCHG` taking 51-58% of all cycles in the single-writer benchmarks (about 19 cycles
per event). A release store is a plain `MOV` on x86, so the x86 gain from this item could be the largest remaining one,
for the single writer's `committedSequence` as well as for `committedSlots`. Measure before assuming it.

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
