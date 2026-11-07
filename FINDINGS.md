# Findings: cache lines, prefetchers, and commit stores on Intel (2026-09-26 to 2026-09-27)

Everything learned in the session that started from the TODO "Benchmark 128B sequence padding on Intel". Numbers are
from the machine and method below unless stated otherwise. Raw results were written to `/tmp/go-disruptor-bench/`,
which does not survive a reboot; this file is the durable record.

## Machine and method

| Item          | Value                                                                                     |
|---------------|-------------------------------------------------------------------------------------------|
| CPU           | Intel i7-12700K: 8 Golden Cove P-cores (CPUs 0-15, SMT), 4 Gracemont E-cores (CPUs 16-19) |
| Caches        | 1.25MB private L2 per P-core, 25MB shared L3                                              |
| Software      | Arch Linux, kernel 7.2.7, Go 1.27.1                                                       |
| Default state | MSR `0x1a4` = `0x0` on all P-cores (all prefetchers on); `0x4` on the E-cores             |

"Reserved" runs used the method now scripted in `bench/` (see `bench/README.md`): P-cores 0-15 reserved with every other
process confined to the E-cores, turbo off, benchmarks pinned to one thread per P-core (`0,2,4,6` for single-producer
sets, all eight even CPUs for multi-producer sets), variants interleaved in shuffled order across 10 rounds, compared
with `benchstat`. "Pilot" runs were pinned but unreserved, with turbo on: fine for large effects, noisy for small ones.
Only multi-producer deltas that replicate across independent runs are trusted.

## 1. 128B sequence padding: rejected on Intel too

The hypothesis (from Java LMAX, folly, and crossbeam): Intel's spatial prefetcher fetches 128B-aligned line pairs, so
two sequences on adjacent 64B lines can interfere, and padding to 128B would prevent it.

| Benchmark       | Pilot delta (pad128 vs 64B) | Reserved delta   |
|-----------------|-----------------------------|------------------|
| `SP4MC`         | +2.99% (p=0.004)            | +7.82% (p=0.007) |
| Every other row | ~                           | ~                |
| Geomean         | +1.58%                      | +1.49%           |

No row improved; `SP4MC` got *slower* in both runs (unexplained). The null result is mostly by construction, verified by
printing addresses:

- `newSequence()` allocates a 128B `[]byte`, which Go's 128B size class places 128B-aligned. Every single sequence lands
  at `56 mod 128`: it occupies the first line of a 128B pair whose second line is its own unused padding. At 64B
  padding, every single sequence already owns its whole pair.
- Only `newSequences(n >= 2)` (the handler sequences) can share a pair, and only about half the time: a 192B allocation
  alternates between 0 and 64 mod 128 (observed `56/120` and `120/56`). The old TODO's claim that adjacent consumers are
  "guaranteed" to share a pair was wrong.
- A consumer's own line rarely misses in L2 (the producer reads it only on the full-buffer slow path), and the
  prefetcher acts on L2 fills, so there is almost nothing to trigger it.

Recorded in `CLAUDE.md` (commit `c4855f4`). The direct test in section 3 later confirmed the conclusion independently.

## 2. Prefetcher controls on Golden Cove differ from Intel's documentation

Intel documents MSR `0x1a4` bit 1 as the "L2 adjacent cache line prefetcher" disable. On these P-cores it does nothing
measurable. Found with a pointer-chase positive control (`bench/prefetch`, `PairChase`): each step loads the first line
of a random 128B pair in a 256MB buffer (always a DRAM miss), then, dependent on its value, the partner line. One DRAM
trip per step means the partner was prefetched; two means it was not.

Unprivileged probes first showed the neighbouring-line fetch is not limited to the 128B pair:

| Second dependent load  | ns/step |
|------------------------|---------|
| None (one load)        | ~83     |
| +64B (same 128B pair)  | ~90     |
| +128B (different pair) | ~90     |
| +2048B (same 4K page)  | ~89     |
| +4096B (next page)     | ~156    |

Any line in the same 4K page arrives nearly free after one miss. Masks applied to CPUs 2-3 (one core, both threads),
with per-step counters (setup subtracted):

| Mask   | SamePair+64 ns | L2 hit | DRAM miss | L2 HW prefetch |
|--------|----------------|--------|-----------|----------------|
| `0x0`  | 89.6           | 0.67   | 1.35      | 1.64           |
| `0x3`  | 90.0           | 0.66   | 1.36      | 1.47           |
| `0xf`  | 90.9           | 0.66   | 1.37      | 1.29           |
| `0x10` | 90.7           | 0.65   | 1.38      | 1.60           |
| `0x20` | **149.0**      | 0.02   | **2.00**  | 0.45           |

Bit map as measured (also in `bench/msr-prefetch --help`):

| Bit | Documented meaning       | Measured on Golden Cove P-cores                                    |
|-----|--------------------------|--------------------------------------------------------------------|
| 0   | L2 streamer              | Accepted; trims prefetch requests, neighbouring line still arrives |
| 1   | L2 adjacent-line         | Accepted; **no measurable effect**                                 |
| 2   | L1 next-line             | Accepted; little effect                                            |
| 3   | L1 IP-stride             | Accepted; little effect                                            |
| 4   | (undocumented here)      | Accepted; no measurable effect                                     |
| 5   | "AMP" (unconfirmed)      | Accepted; **carries the neighbouring-line fetch**                  |
| 6   | E-core LLC page prefetch | Rejected by the CPU (`EIO`)                                        |
| 7   | E-core AOP prefetch      | Rejected by the CPU (`EIO`)                                        |

Mask `0x23` (bits 0, 1, 5) is the "prefetchers off" setting used below. `PairChase` then goes from ~97ns to ~157ns
(+62%), and did so in every run; the suite asserts at least 1.3x before trusting any result.

## 3. Direct test: prefetch-induced false sharing does not exist here

With the working control, the existing benchmarks ran with the prefetchers on (`0x0`) vs off (`0x23`) on all P-cores:

| Benchmark       | On          | Off        | Delta (run 1) | Run 2 (replication)   |
|-----------------|-------------|------------|---------------|-----------------------|
| `SP SC`         | 7.58n ±39%  | 7.11n ±15% | ~             | ~                     |
| `SP MC`         | 7.80n ±7%   | 7.30n ±10% | ~             | —                     |
| `SP4MC`         | 0.538n ±16% | 0.565n ±5% | ~             | —                     |
| Shared SP SC/R1 | 15.3n ±7%   | 18.2n ±1%  | **+18.7%**    | **+25.5%**            |
| MP SC/R1        | 37.2n ±3%   | 36.8n ±2%  | ~             | ~                     |
| MP SC/R4        | 2.47n ±2%   | 2.57n ±2%  | +3.9%         | ~ (did not replicate) |
| MP MC/R1        | 39.4n ±5%   | 39.1n ±1%  | ~             | ~                     |
| MP MC/R4        | 2.74n ±1%   | 2.99n ±4%  | **+9.1%**     | **+9.1%**             |

If neighbouring-line prefetch were bouncing sequence lines between cores, turning it off would have made something
faster. Nothing got faster. The only rows that moved use `sharedSequencer`, and they got *slower*: the prefetchers help.

## 4. Why the shared sequencer depends on prefetching

`committedSlots` is a `[]atomic.Int64` of `capacity` entries (512KB at the benchmarks' 64K slots), written in increasing
order by producers (`Commit` stores one marker per batch) and read in increasing order by consumers (`Load`). It is the
only sizeable memory in the no-data benchmarks. Counters for shared SP SC/R1, per op, three repeats, setup subtracted:

| Counter                         | On          | Off         |
|---------------------------------|-------------|-------------|
| L2 hardware prefetch requests   | 0.41-0.45   | 0.24-0.26   |
| Store misses in L2 (RFO)        | 0.032-0.057 | 0.137-0.166 |
| Loads hitting an in-flight line | 0.15-0.18   | 0.74-0.76   |
| Loads served by another core    | 0.03-0.06   | 0.02-0.08   |

- Producer side: with the prefetchers off, store misses rise to about one per 64B line of `committedSlots` (8 slots per
  line; predicted 0.125/op), and `Commit`'s share of them rises from 61% to 90%. The prefetchers take ownership of about
  70% of those lines before `Commit` writes them.
- Consumer side: cross-core loads did not change (the predicted mechanism was wrong); instead, loads hitting an
  in-flight line rose 5x, 72% of them in `sharedSequencer.Load`. `Load` issues the next slots' loads speculatively
  (the `lower++` branch), so without prefetch the first slot of each line misses and the other seven wait on the same
  fill. Which level serves those lines was not fully accounted.
- Negative control: single SP SC's counters and timing did not change. It has no array.
- MP SC/R4 barely depends on prefetch: one marker per 16 slots, and its store misses (~0.12/op, unchanged) are
  presumably the writers contending on `reservedSequence` (unverified).

Precise attribution (`:pp`) at the default prefetcher state, shared SP SC/R1:

| Event                               | Where it lands                                                   |
|-------------------------------------|------------------------------------------------------------------|
| Loads hitting an in-flight line     | 47% `Commit`'s `XCHG`, 23% `Load`'s marker loads, 0.2% `Reserve` |
| Clean lines taken from another core | 55% `Reserve`'s `reservedSequence.Add`, 40% `Commit`'s store     |

An earlier imprecise sampling put 41-46% of in-flight hits in `Reserve`: skid. Always use `:pp` for attribution. The
`reservedSequence` bounce (consumer's `Load` reads it every poll) is real but small: ~0.008 transfers per op, so the
rejected "unbounded `Load` scan" could win at most ~3%.

## 5. Capacity sweep and real ring-buffer traffic

New benchmarks (`assert_benchmark_test.go`): `BenchmarkSharedSequencerCapacity` (shared sequencer at 1K-1024K slots, no
data) and `BenchmarkRingBuffer` (writers store into a 64B-entry ring buffer before committing, the handler reads every
entry; the single-writer rows use `defaultSequencer`). Baseline, prefetchers at default, run 2:

| Capacity (`committedSlots`) | Shared SP SC/R1 | Shared MP SC/R1 |
|-----------------------------|-----------------|-----------------|
| 1K (8KB)                    | 26.45n ±5%      | 43.69n ±2%      |
| 16K (128KB)                 | 17.47n ±7%      | 39.34n ±3%      |
| 64K (512KB)                 | 15.01n ±10%     | 37.56n ±3%      |
| 256K (2MB)                  | 13.20n ±16%     | 37.29n ±4%      |
| 1024K (8MB)                 | 13.22n ±2%      | 38.00n ±2%      |

| Ring buffer (64K slots, 4MB of entries) | ns/op       |
|-----------------------------------------|-------------|
| SP SC/R1 (`defaultSequencer`)           | 10.45n ±11% |
| SP SC/R4                                | 2.035n ±0%  |
| MP SC/R1                                | 38.30n ±2%  |
| MP SC/R4                                | 3.990n ±1%  |

- Bigger rings are faster, even far past L2: the shared sequencer at 1K slots is 2x slower than at 256K. `New()`
  defaulted to 1024 slots, the slowest point measured; the capacity is now required (section 5a).
- Real ring-buffer traffic adds ~4ns/event to the single writer at R1 (vs ~7ns without data) and dominates at R4
  (2.0ns vs ~0.5ns). Four writers at R1 stay bound by `reservedSequence` contention.

Prefetchers off vs on (two runs; run 1 had the ring buffer 32B off a line boundary, run 2 aligned it):

| Benchmark                  | Run 1      | Run 2      | Verdict                          |
|----------------------------|------------|------------|----------------------------------|
| Shared SP SC/R1, 1K        | ~          | ~          | No dependence (fits in L1)       |
| Shared SP SC/R1, 16K       | +9.5%      | +13.7%     | Replicated                       |
| Shared SP SC/R1, 64K       | +19.3%     | +22.1%     | Replicated                       |
| Shared SP SC/R1, 256K      | +27.7%     | +28.7%     | Replicated                       |
| Shared SP SC/R1, 1024K     | +21.8%     | +27.8%     | Replicated                       |
| Shared MP SC/R1, all sizes | ~          | ~          | No effect                        |
| RingBuffer SP SC/R1        | +4.4%      | +15.4%     | Real; size uncertain             |
| RingBuffer SP SC/R4        | +33.3%     | +33.5%     | Replicated                       |
| RingBuffer MP SC/R4        | **-18.2%** | **-14.8%** | Replicated: prefetchers **hurt** |

- The shared sequencer's dependence on prefetch grows with `committedSlots`' footprint: none in L1, ~28% from 256K up.
- The ring-buffer data stream benefits most (+33% at R4).
- **Four writers at R4 run 15-18% faster with the prefetchers off.** The only case where prefetching hurts. Hypothesis
  (unverified): each writer fills a 1KB batch adjacent to another writer's batch, and a writer's prefetcher fetches the
  next writer's lines within the page, taking them away. That is prefetch-induced false sharing, but in application
  data at batch boundaries, not in the library's sequences.

## 5a. Why small rings are slow, and the required capacity

A temporary probe wrapped the default wait strategy with counters, so each run also reported `Reserve` waits (producer
blocked on a full ring) and `Idle` calls (consumer asleep) per million events. Pinned to one thread per P-core (4 CPUs
single-producer, 8 multi-producer), unreserved, turbo on, 3 runs, medians in ns/op:

| Shape                     | 1K    | 4K    | 16K   | 64K   | 256K  |
|---------------------------|-------|-------|-------|-------|-------|
| SP SC/R16 (single writer) | 7.89  | 0.575 | 0.343 | 0.342 | 0.337 |
| SP SC/R1                  | 5.56  | 5.43  | 5.40  | 5.52  | 5.29  |
| SP MC/R1                  | 5.82  | 5.59  | 5.54  | 5.56  | 5.33  |
| Shared SP SC/R1           | 26.00 | 19.05 | 13.97 | 11.35 | 10.52 |
| MP SC/R1                  | 42.42 | 38.55 | 34.94 | 35.01 | 32.95 |
| MP SC/R16                 | 5.41  | 3.05  | 2.42  | 2.22  | 2.19  |

- The mechanism is consumer wake-up latency. `time.Sleep(500ns)` (the default `Idle`) takes ~1.9us median and ~9us p99
  here; `time.Sleep(1ns)` (`Reserve`) ~0.2us; `Gosched` ~56ns. At 0.34ns/event a producer fills 1K slots in ~350ns and
  then stalls on every consumer wake: consumers slept ~33,000 times per million events at 1K vs ~20 from 16K up.
- Sizing rule: capacity x ns/event must exceed the consumer's wake latency, i.e. ~6K slots for a median wake and ~26K
  for p99 at batching speed.
- Single-writer R1 is capacity-insensitive: the consumer is the slower side, so the ring stays full at any size.
- Every shape plateaus by 64K; 256K buys 1-7% more for 2MB of `committedSlots` per shared sequencer. **Adopted:
  `BufferCapacity` is now required, with 65536 recommended** (matching the README and `example/main.go`). Raising the
  default instead was rejected: an application that omitted the option sized its own ring buffer at 1024, and writers
  would silently overwrite its unconsumed entries. `New()` now fails fast instead.

## 6. Release stores in `Commit`: adopted

Go compiles `atomic.Int64.Store` to `XCHG` on amd64: locked, so it cannot retire until the core owns the line, which the
consumer usually still holds. A release store is a plain `MOV` on x86 (TSO), which retires into the store buffer while
ownership is acquired in the background. An unsafe variant replacing both `Commit` stores with plain stores through
`unsafe.Pointer` (verified in the disassembly: `XCHGQ` became `MOVQ`; unit and end-to-end tests passed without `-race`),
pilot, 10 rounds:

| Benchmark         | Base (`XCHG`) | Plain `MOV` | Delta                |
|-------------------|---------------|-------------|----------------------|
| SP SC             | 5.87n ±25%    | 3.04n ±3%   | **-48.3%** (p=0.000) |
| SP4SC             | 0.376n ±17%   | 0.191n ±6%  | **-49.3%**           |
| SP MC             | 5.61n ±7%     | 3.02n ±3%   | **-46.2%**           |
| SP4MC             | 0.349n ±5%    | 0.195n ±6%  | **-44.0%**           |
| Shared SP SC/R1   | 11.0n ±4%     | 7.37n ±3%   | **-33.1%**           |
| MP SC/R1          | 30.3n ±3%     | 28.7n ±5%   | -5.1% (p=0.002)      |
| MP SC/R4, MP MC/* | —             | —           | ~                    |

The single writer roughly doubles, consistent with `XCHG` being 51-58% of single-writer cycles on Zen 2.
Single-producer variance also fell from ±7-25% to ±3-6%, suggesting much of that noise was `XCHG` stalls rather than
the Go scheduler (untested). The unsafe variant is an upper bound, not shippable: it is a data race under the Go memory
model, and it is safe only while `Commit` is not inlined.

How the options compared (the design notes now live with `store_release_*` in `CLAUDE.md`):

- **Viable:** a one-instruction amd64 assembly stub called from both `Commit` methods in a file built with
  `//go:build amd64 && !race`, with `atomic.Store` everywhere else. The build-tag split is required: the race detector
  models synchronization only through instrumented `sync/atomic` calls, so any untagged non-atomic store (assembly,
  plain store, or linkname) erases the Commit→Load happens-before edge and produces false positives on every
  ring-buffer access. Assembly is opaque to the compiler (no reordering across it, no inlining). Cost to measure: one
  call per commit, against ~2.8ns of headroom. `make test` runs only with `-race`, so a non-race test pass is needed.
- **Rejected: a plain Go store behind the tag.** Profile-guided optimization can devirtualize and inline `Commit`, after
  which the compiler may legally move the caller's ring-buffer writes past the store.
- **Rejected: `go:linkname` to `internal/runtime/atomic.StoreRel64`.** The Go 1.27 linker refuses it
  (`link: main: invalid reference to internal/runtime/atomic.StoreRel64`) unless built with `-ldflags=-checklinkname=0`,
  which a library cannot impose.
- **Unavailable: `sync/atomic` release/acquire.** Go 1.27's `atomic.Int64` has only `Add`, `And`, `CompareAndSwap`,
  `Load`, `Or`, `Store`, and `Swap`.

**Shipped:** `storeRelease` (`store_release_amd64.s`, a `MOVQ`, behind `amd64 && !race`; `atomic.Store` elsewhere). The
compiler calls the ABI0 function directly, with no wrapper: the cost is a small frame, the stack check, and restoring
`X15`/`R14` after the call. Forcing the stub into a `-race` build produced 10,987 false data-race reports in
`TestEndToEnd`, confirming the tag split. Pinned, unreserved, turbo on, 10 shuffled rounds:

| Benchmark           | Base (`XCHG`) | Stub (`MOVQ` call) | Plain (inline) | Stub delta | Plain delta |
|---------------------|---------------|--------------------|----------------|------------|-------------|
| Sequencer/Commit    | 3.49n ±0%     | 0.823n ±0%         | 0.242n ±34%    | -76.4%     | -93.1%      |
| SP SC               | 5.32n ±25%    | 3.66n ±2%          | 3.17n ±3%      | -31.2%     | -40.4%      |
| SP4SC               | 0.335n ±39%   | 0.232n ±4%         | 0.201n ±6%     | -30.7%     | -40.0%      |
| SP MC               | 5.39n ±6%     | 3.70n ±1%          | 3.17n ±2%      | -31.4%     | -41.2%      |
| SP4MC               | 0.338n ±2%    | 0.240n ±2%         | 0.203n ±2%     | -28.8%     | -39.8%      |
| Shared SP SC/R1     | 10.49n ±2%    | 7.56n ±1%          | 6.99n ±7%      | -28.0%     | -33.4%      |
| RingBuffer SP SC/R1 | 8.27n ±1%     | 4.07n ±1%          | 3.52n ±2%      | -50.8%     | -57.5%      |
| RingBuffer SP SC/R4 | 1.52n ±2%     | 1.46n ±1%          | 1.47n ±2%      | -3.8%      | -3.1%       |
| Multi-producer rows | —             | —                  | —              | ~ to -4%   | ~ to -5%    |

The stub keeps about three quarters of the upper bound, and single-producer variance collapses (±6-39% to ±1-4%).

Confirmed on reserved P-cores with turbo off (`bench/compare`, base `c104c6e`, 10 shuffled rounds, 2026-09-28):

| Benchmark               | Base (`XCHG`) | Stub        | Delta          |
|-------------------------|---------------|-------------|----------------|
| Sequencer/Commit        | 4.741n ±0%    | 1.115n ±0%  | -76.5%         |
| SP SC                   | 8.929n ±35%   | 5.000n ±0%  | -44.0%         |
| SP4SC                   | 0.5295n ±17%  | 0.3157n ±1% | -40.4%         |
| SP MC                   | 8.064n ±10%   | 5.042n ±0%  | -37.5%         |
| SP4MC                   | 0.5308n ±10%  | 0.4088n ±9% | -23.0%         |
| Shared SP SC/R1         | 14.205n ±1%   | 9.694n ±0%  | -31.8%         |
| RingBuffer SP SC/R1     | 10.455n ±0%   | 5.861n ±2%  | -44.0%         |
| RingBuffer SP SC/R4     | 2.059n ±2%    | 1.972n ±0%  | -4.3%          |
| Multi-producer (6 rows) | —             | —           | -2.3% to -4.6% |
| Geomean                 | 5.747n        | 4.168n      | -27.5%         |

Reserved and turbo off, the gains are larger than in the pilot, and even the multi-producer rows improve slightly (all
p <= 0.005). Still to do: Zen 2.

## 6a. Phased-backoff default wait strategy: not adopted

Tried: `Idle` and `Reserve` spin (return immediately) for the first N calls of a wait, then `Gosched` for 16 calls,
then sleep as before. New benchmarks measure it directly: `BenchmarkWakeLatency` (one event published after the ring
has been empty for a gap; `latency-ns/op` is publish to handled) and `BenchmarkSequencerCapacity` (single writer, 1K to
64K slots). Pilot, pinned, turbo on:

| Measure                              | Original (sleep first) | Spin 256, yield 16 |
|--------------------------------------|------------------------|--------------------|
| SP SC/R16 at 1K slots                | 7.05n                  | 0.57n              |
| Wake latency, no gap (mean)          | 5.8us                  | 0.15us             |
| Wake latency, 2us gap (p50 / p99)    | 1.6us / 69us           | 0.25us / 950us     |
| Wake latency, 50us gap (p50 / p90)   | 1.1us / 559us          | 648us / 1.04ms     |
| Wake latency, 50us gap (mean)        | ~100us                 | ~550us             |

- Dense traffic wins big: the consumer is still spinning when the next event arrives, and small rings stop stalling.
- Sparse traffic loses badly. All three variants measured after a gap (spin 256 with 16 yields, spin 256 with no
  yields, and 16 yields with no spins) made mean latency 2-8x worse at a 2us gap and ~5x worse at 50us.
- The mechanism is the Go runtime, not the Disruptor. A `time.Sleep` shorter than 1ms is fast only while some thread
  is still spinning in the scheduler; once the thread parks, `netpoll_epoll.go` rounds the epoll timeout up to 1ms
  (`delay < 1e6` gives `waitms = 1`). Both strategies show the 1ms tail; after a spin phase, far more wakes land in it,
  plausibly because the runtime's other threads have parked by the time the consumer finally sleeps (not verified).
- Consequence: no sleep-based `Idle` phase can promise sub-millisecond wake latency in Go. Low latency after idle needs
  a consumer that never sleeps (an opt-in spinning strategy on a dedicated core, pathway 3) or a wake-up the producer
  triggers (e.g. a futex or channel signal on the transition from empty, which puts a cost on `Commit`).
- The benchmarks were kept; the strategy was not. The original default is unchanged.

## 6b. Opt-in busy-spin wait strategy and the empty-poll `Load`: adopted

`BusySpinWaitStrategy` makes every wait one `spinHint()` (eight `PAUSE`s on amd64). Pilots, pinned, unreserved, turbo
on, 6 shuffled rounds each:

| Variant (busy-spin rows)             | SP SC/R1 | SP MC/R1 | SP SC/R16 | Shared SP SC/R1 | Wake p50  |
|--------------------------------------|----------|----------|-----------|-----------------|-----------|
| 1 `PAUSE`, `Load` reads upper first  | 5.20n    | 8.75n    | 0.342n    | 41.6n           | 150-176ns |
| 1 `PAUSE`, empty-poll `Load`         | 5.27n    | 10.1n    | 0.337n    | 27.8n           | 161-220ns |
| 8 `PAUSE`s, `Load` reads upper first | 3.85n    | 4.26n    | 0.242n    | 25.5n           | 190-404ns |
| 8 `PAUSE`s, empty-poll `Load`        | 3.88n    | 4.27n    | 0.243n    | 11.5n           | 210-398ns |
| No spin hint (vs 1 `PAUSE`)          | +78%     | ~        | +46%      | +31%            | ~         |

- The spinning consumer's polls take the producer's cache lines away. More `PAUSE`s per poll cut that traffic, and
  reading `reservedSequence` only when the first marker is current removes the shared sequencer's worst case (every
  `Reserve` is a locked add on that line). The eight `PAUSE`s cost ~100-250ns of wake-up latency.

Final design against the default strategy, same binary, P-cores reserved, turbo off, 10 shuffled rounds
(`bench/suite baseline`, 2026-09-28):

| Benchmark                   | Default     | Busy-spin | Delta   |
|-----------------------------|-------------|-----------|---------|
| Wake p50 (6 rows)           | 1.52-2.20us | 261-413ns | 4-8x    |
| Wake p99, no gap or 2us gap | 9.0-22.5us  | 296-484ns | 19-76x  |
| Wake p99, 50us gap          | 1.04ms      | 549-578ns | ~1,850x |
| SP SC/R16 at 1K slots       | 8.95n       | 0.682n    | -92%    |
| SP SC/R1                    | 4.73n       | 5.00n     | +6%     |
| SP MC/R1                    | 4.79n       | 5.57n     | +16%    |
| SP SC/R16 (64K)             | 0.297n      | 0.313n    | +5%     |
| Shared SP SC/R1             | 9.86n       | 15.44n    | +57%    |
| MP SC/R1                    | 35.9n       | 41.5n     | +16%    |
| MP SC/R16                   | 2.40n       | 2.40n     | ~       |

The empty-poll `Load` against the previous `Load`, default strategy, 8 rounds:

| Benchmark            | Before | After  | Delta            |
|----------------------|--------|--------|------------------|
| Shared SP SC/R1      | 7.57n  | 7.93n  | +4.7% (p=0.010)  |
| Shared SP SC/R1, 1K  | 15.88n | 11.43n | -28.0% (p=0.000) |
| Shared SP SC/R1, 64K | 7.67n  | 7.82n  | +1.9% (p=0.002)  |
| MP MC/R4             | 2.10n  | 1.90n  | -9.4% (p=0.003)  |
| Other multi-producer | —      | —      | ~                |
| Geomean              | 8.65n  | 8.27n  | -4.3%            |

Reserved and turbo off (`bench/compare`, base `d9cd73b`, 10 rounds): shared SP SC/R1 +1.6% (9.70 to 9.86ns,
p=0.000), MP MC/R1 -4.8%, MP MC/R4 -7.3%, other multi-producer rows unchanged, geomean -2.2%. The single-writer rows
also got 5-9% faster (and `Sequencer/Commit` 2.3% slower) although their code did not change, so treat single-writer
deltas of that size between two binaries as code-layout effects.

CPU cost (`BenchmarkIdleCPU`: process CPU time over wall time while the ring is empty, or while one event arrives every
100us; reserved, turbo off, medians of 10 rounds):

| Consumers | Default, idle | Default, sparse | Busy-spin, idle | Busy-spin, sparse |
|-----------|---------------|-----------------|-----------------|-------------------|
| 1         | 0.55 cores    | 0.23 cores      | 1.00 cores      | 1.02 cores        |
| 2         | 0.84 cores    | 0.78 cores      | 2.00 cores      | 2.01 cores        |

- Busy-spin costs exactly one core per waiting goroutine, as designed.
- **The default is not cheap while idle either.** A plain Go loop of `time.Sleep(500ns)` burns 0.57 cores (0.78 for two
  goroutines), while `time.Sleep(50us)` or `time.Sleep(1ms)` costs 0.02: a sub-microsecond timer keeps the runtime's
  scheduler threads spinning. So an idle default consumer spends more than half a core to buy a 1-2us median wake-up,
  and its p99 after an idle gap is already ~1ms (section 6a). An idle phase that escalates to a longer sleep (tens of
  microseconds) would cut idle CPU ~30x while barely moving that p99; not yet tried.

Still to do: arm64 (`YIELD` may be far shorter than `PAUSE`), and Zen 2.

## 6c. `ParkingWaitStrategy`: idle CPU met, wake latency after a gap not met (pilot)

`ParkingWaitStrategy` spins, then yields, then parks an idle Listener on a channel that every Commit, every handled
batch, and Close wake (`wait_strategy_park.go`). Success criteria came from `TODO.md`.

**Machine and method.** Every number in this section is: pilot: macOS, Apple M4 Pro, arm64, unreserved, turbo not
controlled. `bench/reserve` and `bench/suite` need Linux and root, so neither ran. Instead, `go test -c` built master
(`ef0337c`, from a temporary worktree) and the candidate (`0a65656` plus the Task 2 tests) as separate binaries. Ten
rounds ran them in alternating order, base first in odd rounds, without CPU pinning, with
`-benchmem -test.count 1`. `benchstat` compared them. The `BusySpin` and `Parking` throughput families ran in a
second pass, 10 candidate-only runs, because the first pass pattern `^BusySpin$` matched nothing. Raw results are in
`/tmp/go-disruptor-bench/parking-pilot/`. The i7-12700K reserved run is still open (`TODO.md`). Amd64's `storeRelease`
(a plain `MOVQ`) path does not run on arm64, so the fence cost on amd64 is unmeasured here.

**Criterion 1, idle CPU at least 10x lower than the default: met.** `BenchmarkIdleCPU`, median cores of 10 rounds:

| Consumers | Default idle | Parking idle | Default sparse | Parking sparse | BusySpin idle / sparse |
|-----------|--------------|--------------|----------------|----------------|------------------------|
| 1         | 1.337        | 0.023 (57x)  | 1.361          | 0.085 (16x)    | 0.998 / 1.062          |
| 2         | 1.618        | 0.035 (46x)  | 1.615          | 0.123 (13x)    | 1.998 / 2.063          |

The default strategy costs more than a core on this machine (0.55 and 0.84 on the reserved i7, section 6b). The
sparse rows (one event per 100us) are the weakest: 13x for two consumers.

**Criterion 2, wake p50 and p99 after long gaps no worse than the default: not met.** `BenchmarkWakeLatency`,
medians of 10 rounds (p99 has ±15-86% spread on the Parking rows):

| Gap  | Sequencer    | Default p50 | Parking p50 | BusySpin p50 | Default p99 | Parking p99 | BusySpin p99 |
|------|--------------|-------------|-------------|--------------|-------------|-------------|--------------|
| 0    | SP SC        | 3.42us      | 84ns        | 84ns         | 14.04us     | 292ns       | 271ns        |
| 0    | Shared SP SC | 3.46us      | 125ns       | 125ns        | 14.04us     | 416ns       | 417ns        |
| 2us  | SP SC        | 2.25us      | 8.63us      | 84ns         | 13.42us     | 19.7us      | 333ns        |
| 2us  | Shared SP SC | 2.23us      | 8.63us      | 125ns        | 13.58us     | 19.9us      | 500ns        |
| 50us | SP SC        | 2.15us      | 14.1us      | 84ns         | 13.40us     | 24.5us      | 417ns        |
| 50us | Shared SP SC | 2.21us      | 14.1us      | 125ns        | 13.58us     | 23.2us      | 563ns        |

`SP SC` is the single-writer sequencer; `Shared SP SC` is one writer on the shared sequencer (`WriterCount(2)`).

- With no gap, Parking ties BusySpin on p50 (84ns and 125ns, 97% lower than the default) and equals it at p99,
  because the Listener is still in its spin phase. BusySpin keeps that p50 at every gap.
- After a 2us or 50us gap, the Listener has parked. A park-to-wake round trip through the Go scheduler costs 8-14us
  at p50 and 20-25us at p99. That is 4-7x the default's p50 and 1.5-1.8x its p99. These rows fail the criterion.
- The default strategy's p99 here is 13-14us, not the 1ms seen on the i7 (section 6a). A likely explanation, not
  verified: darwin's netpoll uses `kqueue`, which takes a nanosecond `timespec`, so the 1ms `epoll` timeout floor may
  not apply on macOS. If so, the default looks better here than on Linux. The Linux comparison could reverse the p99
  verdict. It cannot fix the p50 verdict unless the Linux wake is 4x faster, which no run has shown.
- The strategy is not tuned. A longer spin phase would move the gap at which parking starts, not the cost of a park.
- The parked path allocates. The 2us rows are 109 B/op with 0 allocs/op; only the 50us rows reach 1 alloc/op
  (112 B/op). The cause is one new channel per notify that finds a waiter. The sparse idle rows show 8 (one consumer)
  and 17 (two consumers) allocs/op, one op being a millisecond, for the same reason.

**Criterion 3, the cost on `Commit`: measured.** As first built, `notify` ran `waiters.Add(0)`, a `LOCK XADD` (arm64:
a seq-cst atomic read-modify-write), after every Commit and every handled batch. A notify that finds a waiter takes a
mutex, closes a channel, makes a new one, and wakes every parked Listener. In the throughput families, run with the
same binary, BusySpin against Parking (benchstat, 10 runs):

| Row             | BusySpin | Parking | Delta |
|-----------------|----------|---------|-------|
| SP SC/R1        | 15.98n   | 59.03n  | +270% |
| SP SC/R16       | 1.003n   | 3.704n  | +269% |
| SP SC/R16/1K    | 1.013n   | 3.832n  | +278% |
| SP MC/R1        | 16.35n   | 91.11n  | +457% |
| Shared SP SC/R1 | 22.19n   | 58.77n  | +165% |
| MP SC/R1        | 42.48n   | 63.12n  | +49%  |
| MP SC/R16       | 2.241n   | 3.870n  | +73%  |

The cost was not park and wake churn. Every `BenchmarkParking` row is 0 B/op, so no notify found a waiter (a wake
allocates a channel). The extra time is near-constant per Commit: about 43ns at R1 (59.0 - 16.0), and the same at
R16, where 2.7ns per slot times 16 slots is 43ns. That is the read-modify-write on the `waiters` line, which every
Commit and every handled batch wrote, so the line moved between the producer's and the Listener's cores each time.

**The fix: a load, and a private-line fence only where it is needed.** `notify` now runs `waiters.Load()`. Dekker's
pattern still holds, because each side orders its store before its load:

- The parking side adds itself to `waiters` with a seq-cst add, then re-reads the barrier.
- A Listener calls `notify` after `handledSequence.Store` or (in Close) `running.Store`. Both are seq-cst `sync/atomic`
  stores, so the load cannot move ahead of them.
- `Commit` calls `notifyAfterRelease`, which runs `fullFence` and then `notify`. Where `storeRelease` is
  `atomic.Int64.Store` (every build except amd64 without `-race`), `fullFence` is a no-op. On amd64 without `-race`,
  `storeRelease` is a plain `MOVQ`, which x86 may reorder after a later load, so `fullFence` is `LOCK ORQ $0, 0(SP)`:
  a locked instruction on the goroutine's own stack line (the Linux `smp_mb` idiom), not on a shared line.

Base (`457d20d`, `waiters.Add(0)`) against the fix, same pilot machine, 10 interleaved rounds in shuffled order, only
the `BenchmarkParking` family and the Parking rows of `BenchmarkWakeLatency` (raw results and `benchstat.txt` in
`/tmp/go-disruptor-bench/parking-fence/`):

| Row             | Add(0)  | Load   | Delta                 |
|-----------------|---------|--------|-----------------------|
| SP SC/R1        | 66.08n  | 21.50n | -67.5% (p=0.000)      |
| SP SC/R16       | 3.997n  | 1.340n | -66.5% (p=0.000)      |
| SP SC/R16/1K    | 3.905n  | 1.425n | -63.5% (p=0.000)      |
| SP MC/R1        | 102.80n | 22.40n | -78.2% (p=0.000)      |
| Shared SP SC/R1 | 72.80n  | 26.21n | -64.0% (p=0.000)      |
| MP SC/R1        | 71.79n  | 58.28n | -18.8% (p=0.005)      |
| MP SC/R16       | 4.177n  | 2.635n | -36.9% (p=0.007)      |

- The single-writer rows save 44.6ns per Commit at R1 and 42.7ns per Commit at R16 (2.66ns per slot): the ~43ns
  RMW cost, removed. `SP MC/R1` saves 80ns, because two Listeners each wrote the line per batch.
- `SP SC/R1` is now 21.5ns, against BusySpin's 16.0ns in the earlier pass (a different run, so only a rough gap).
- The four-writer rows gain less: their `Reserve` already contends on a shared counter.
- The wake-latency rows did not move (all `~` except `Shared SP SC/50us` mean and p50, -10.6% and -2.3% at
  p=0.043 and p=0.008), and their allocation is unchanged (109-112 B/op, 0 or 1 alloc/op).
- The amd64 `fullFence` is compiled (`GOARCH=amd64 go vet` and `go test -c` pass, and `objdump` shows
  `LOCK ORQ $0x0, 0(SP)`) but unmeasured: arm64 runs the no-op. The reserved i7 run must measure it.

Parking is still a strategy for idle and sparse traffic, not for the lowest latency.

**Criterion 4, no default-strategy row regresses: met.** Base (master) against candidate, 10 interleaved rounds, all
default-strategy rows: 34 throughput rows (`Sequencer`, `SharedSequencer`, `SequencerCapacity`,
`SharedSequencerCapacity`, `RingBuffer`) and the `WakeLatency/Default` and `IdleCPU/Default` rows. Every row is `~`
by benchstat (p>0.05) except `WakeLatency/SP_SC/2us` p50, -0.9% (p=0.045, an improvement). The throughput geomean
moved +0.26%, from 10.17n to 10.20n. `New` wraps the Sequencer only for the Parking strategy, so the default path does
not change.

Open: the reserved i7-12700K run (`TODO.md`). A reserved amd64 run on another CPU is section 6d.

## 6d. `ParkingWaitStrategy` reserved on amd64 (i9-10850K): p99 verdict reversed, fence cost measured

The reserved run that section 6c and `TODO.md` asked for, on Linux amd64. It is **not the i7-12700K**: it ran on
`appa`, an Intel i9-10850K, so the Golden Cove rows above and these rows do not compare one to one.

**Machine and method.**

| Item        | Value                                                                                                   |
|-------------|---------------------------------------------------------------------------------------------------------|
| CPU         | Intel i9-10850K (Comet Lake): 10 cores, 20 threads (SMT siblings are CPU i and i+10), one NUMA node     |
| Software    | Debian 13, kernel 6.12.115, Go 1.27.1, bare metal                                                       |
| Reservation | `bench/reserve --cpus 0-7,10-17 --housekeeping 8,9,18,19`: cores 0 to 7 (both threads) for benchmarks;   |
|             | every other process (systemd slices, the user manager, a Kafka container, `tailscaled`) on 8, 9, 18, 19 |
| Sets        | single-producer sets on `0,1,2,3`, multi-producer sets on `0,1,2,3,4,5,6,7` (one thread per core);     |
|             | PairChase on CPU 2                                                                                      |
| State       | turbo off during each run (`reserve`), governor `performance`; MSR `0x1a4` = `0x0` on all 20 CPUs       |
| Revision    | `2f73384` (section 6c's code plus the bench tooling change below)                                       |
| Date        | 2026-11-06                                                                                              |

Turbo, the CPU sets and the MSR values were back to their first state after every run (turbo on, `AllowedCPUs=`
empty on `system.slice`, `user.slice` and `init.scope`, MSR `0x0`).

Commands (10 shuffled rounds unless stated):

```bash
CPU="--cpus 0-7,10-17 --housekeeping 8,9,18,19 --single-producer-cpus 0,1,2,3 --multi-producer-cpus 0,1,2,3,4,5,6,7"
bench/suite validate $CPU --control-cpu 2 --prefetch-mask 0xf     # 1 round
bench/suite baseline $CPU --control-cpu 2
bench/compare --base origin/master $CPU                            # master = ef0337c
bench/suite prefetch $CPU --control-cpu 2 --prefetch-mask 0xf
```

The tooling change (commit `2f73384`): the suite's baseline step now selects the Parking rows (`BenchmarkParking` and
the Parking rows of `BenchmarkWakeLatency` and `BenchmarkIdleCPU`), `bench/compare` takes the suite's CPU options,
and `bench/suite --prefetch-mask` sets the mask for the validate and prefetch steps.

`environment.txt` of the baseline step:

```
date: 2026-11-06 23:25:22Z
host: appa
cpu: Intel(R) Core(TM) i9-10850K CPU @ 3.60GHz
kernel: 6.12.115+deb13.1-amd64
turbo: off
msr 0x1a4: cpu0 0x0 cpu1 0x0 cpu2 0x0 cpu3 0x0 cpu4 0x0 cpu5 0x0 cpu6 0x0 cpu7 0x0 cpu8 0x0 cpu9 0x0 cpu10 0x0 cpu11 0x0 cpu12 0x0 cpu13 0x0 cpu14 0x0 cpu15 0x0 cpu16 0x0 cpu17 0x0 cpu18 0x0 cpu19 0x0
rounds: 10
set: 0,1,2,3:4:(Sequencer|RingBuffer|WakeLatency|BusySpin|Parking|IdleCPU)/(SP|Default|BusySpin|Parking)
set: 0,1,2,3,4,5,6,7:8:(Sequencer|RingBuffer|BusySpin|Parking)/MP
variant: base=/tmp/go-disruptor-bench/appa-baseline/bin/base.test mask=original built with unknown
```

The compare, prefetch and fence runs recorded the same machine lines (dates 23:47:46Z, 23:56:11Z and 01:02:03Z, turbo
off, MSR `0x0` on every CPU). "built with unknown" means `go` was not on root's `PATH` under `reserve`; every binary
was built with Go 1.27.1 (the two fence binaries on macOS with `GOOS=linux GOARCH=amd64`). The raw results were in
`/tmp/go-disruptor-bench/appa-*` on appa (tmpfs); this section is the durable record.

**Single-producer rows are bimodal on this machine.** `Sequencer/SP SC` ran at about 6.0 ns in some rounds and 14 ns
in others (median 12.8 ns, ±53%), and so did `SP MC`, `SP4SC`, `SP4MC` and the `BusySpin` and `Parking` `SP SC` rows.
The shared-sequencer, ring-buffer and multi-producer rows are tight (±0-7%). The likely cause is where the Go
scheduler puts the producer and the consumer inside the 4-CPU set (the "Pin benchmark producers and consumers"
item); this run did not check it. Read the medians of those rows as rough.

**Criterion 1, idle CPU at least 10x lower than the default.** `BenchmarkIdleCPU`, median cores:

| Consumers | Default idle | Parking idle | Default sparse | Parking sparse | BusySpin idle / sparse |
|-----------|--------------|--------------|----------------|----------------|------------------------|
| 1         | 0.755        | 0.0171 (44x) | 0.421          | 0.0456 (9.2x)  | 0.998 / 1.016          |
| 2         | 1.026        | 0.0255 (40x) | 0.993          | 0.0829 (12x)   | 1.997 / 2.014          |

Met for idle (40-44x). The one-consumer sparse row is just under 10x (9.2x). The default costs 0.76 and 1.03 cores
idle here (0.55 and 0.84 on the i7, section 6b).

**Criterion 2, wake p50 and p99 after long gaps no worse than the default.** `BenchmarkWakeLatency`, medians, p50 /
p99, `SP SC` (the `Shared SP SC` rows are within 5% of these):

| Gap  | Default           | Parking         | BusySpin      | Parking, pilot (6c) |
|------|-------------------|-----------------|---------------|---------------------|
| 0    | 2.91 / 3.80us     | 331 / 341ns     | 332 / 339ns   | 84 / 292ns          |
| 2us  | 905ns / 1.87us    | 457 / 496ns     | 435 / 449ns   | 8.63 / 19.7us       |
| 50us | 1.81us / 1.02ms   | 59.7 / 60.9us   | 462 / 509ns   | 14.1 / 24.5us       |

- **p99 after a long gap: reversed, now met.** On Linux the default's p99 after 50us is 1.02ms, the `epoll` 1ms floor
  of section 6a, as section 6c expected. Parking's p99 is 60.9us, 17x better.
- **p50 after a long gap: confirmed, not met.** Parking's p50 after 50us is 59.7us against the default's 1.8us, 33x
  worse.
- **After 2us, Parking does not park on this CPU.** It wakes in 457ns at p50, better than the default (905ns) and
  close to BusySpin. A likely reason: `spinHint` is eight `PAUSE`s, a `PAUSE` takes about 140 cycles on Skylake and
  later cores, so 64 spins and 16 yields last tens of microseconds, far longer than on the M4 Pro. This run did not
  time the spin phase.
- **The park-to-wake trip costs about 60us here, against 8-14us on the M4 Pro**, with a spread under 1%. It is not the
  deep C-states: a diagnostic with C2 and C3 disabled on the benchmark CPUs (5 single rounds each way, one binary)
  gave 59.6 / 60.4us with them off and 59.6 / 61.0us with them on. The cause is open.
- The parked path allocates as in section 6c: 112-114 B/op and 1 alloc/op in the 50us rows.

**Criterion 3, the cost on `Commit`, and the amd64 fence.** Throughput, medians of the baseline step, and the
fence diagnostic below:

| Row             | BusySpin        | Parking         | Parking without `fullFence` (diagnostic) |
|-----------------|-----------------|-----------------|------------------------------------------|
| SP SC/R1        | 11.4n (bimodal) | 30.2n (bimodal) | 19.6n                                    |
| SP SC/R16       | 0.41n           | 2.04n           | 1.30n                                    |
| SP MC/R1        | 15.7n (bimodal) | 38.9n (bimodal) | 26.9n                                    |
| Shared SP SC/R1 | 19.4n           | 33.2n           | 24.5n                                    |
| MP SC/R1        | 30.4n           | 46.0n           | 31.2n                                    |
| MP SC/R16       | 1.78n           | 2.90n           | 1.92n                                    |

The diagnostic isolates `fullFence`: two binaries of the same commit, one as shipped and one with `fullFence` as a
bare `RET` (not correct; for measurement only, never committed). `objdump` showed `LOCK ORQ $0x0, 0(SP)` in the first
and only `RET` in the second. Reserved as above, 10 shuffled rounds, the Parking and BusySpin families and the Parking
wake rows:

| Row                     | With fence | Without fence | Delta              |
|-------------------------|------------|---------------|--------------------|
| Parking SP SC/R1        | 38.24n     | 19.61n        | -48.7% (p=0.001)   |
| Parking SP SC/R16       | 2.752n     | 1.301n        | -52.8% (p=0.002)   |
| Parking SP SC/R16/1K    | 2.411n     | 1.213n        | -49.7% (p=0.002)   |
| Parking SP MC/R1        | 41.62n     | 26.92n        | -35.3% (p=0.035)   |
| Parking Shared SP SC/R1 | 33.75n     | 24.52n        | -27.4% (p=0.000)   |
| Parking MP SC/R1        | 45.15n     | 31.20n        | -30.9% (p=0.000)   |
| Parking MP SC/R16       | 2.907n     | 1.920n        | -34.0% (p=0.000)   |
| BusySpin rows (7)       | —          | —             | ~                  |
| Parking wake rows (6)   | —          | —             | ~ (two p50/p99 rows -2% to -5%, p<0.05) |

- **The amd64 fence costs about 19-23ns per `Commit` with one writer** (18.6ns at R1; 1.45ns per slot at R16, which is
  23ns per 16-slot Commit), 9-14ns on the shared sequencer, and nothing measurable on wake latency. That is about two
  thirds of Parking's extra cost over BusySpin on amd64. The rest (about 8ns at R1: 19.6ns against BusySpin's 11.9ns
  in the same run) is the load of `waiters` and the notify call.
- The `LOCK ORQ` touches only the goroutine's own stack line, but a locked instruction drains the store buffer. The
  buffer holds `Commit`'s plain store to the cursor line, which the consumer's core usually owns, so the fence waits for
  that line: the same wait that the release store removed from `Commit` (section 6). This explanation fits the
  numbers; no `perf` counter checked it.
- On arm64 (section 6c) Parking `SP SC/R1` was 21.5ns against BusySpin's 16.0ns. On amd64 it is 30-38ns against about
  12ns. Parking stays a strategy for idle and sparse traffic; on amd64 a dense single writer pays about 2.5-3x
  BusySpin's time per `Commit`.

**Criterion 4, no default-strategy row regresses: met.** `bench/compare --base origin/master` (`ef0337c`), 10 rounds:
every row `~` except `SharedSequencer/MP_MC/R4`, -5.0% (p=0.001, an improvement). Geomean 5.709n to 5.520n (-3.3%,
from the bimodal single-producer rows, none significant). `Sequencer/Commit` is 1.832ns in both.

**Prefetchers on Comet Lake: Intel's documented bits work.** The validate step with mask `0xf` (bits 0-3: L2
streamer, L2 adjacent line, L1 next line, L1 IP stride) passed: PairChase 86.9ns on, 134.8ns off (1.55x). The CPU
took the mask on all 16 benchmark CPUs, and `0x0` came back after. So the prefetch step ran (PairChase 86.8ns and
134.7ns). Prefetchers off against on:

| Row                                            | On              | Off             | Delta            |
|------------------------------------------------|-----------------|-----------------|------------------|
| SharedSequencer/SP_SC/R1                       | 13.79n          | 16.60n          | +20.4%           |
| SharedSequencerCapacity/SP_SC/R1 (1K to 256K)  | 12.80n - 18.20n | 15.04n - 24.59n | +17.5% to +35.1% |
| SharedSequencerCapacity/MP_SC/R1/1K            | 31.52n          | 42.34n          | +34.3%           |
| RingBuffer/SP_SC/R1                            | 10.72n          | 24.75n          | +130.8%          |
| RingBuffer/SP_SC/R4                            | 2.944n          | 4.407n          | +49.7%           |
| BusySpin/Shared_SP_SC/R1                       | 19.93n          | 22.50n          | +12.9%           |
| BusySpin/MP_SC/R16                             | 1.777n          | 1.955n          | +10.0%           |
| SharedSequencer/MP_SC/R4                       | 2.262n          | 1.981n          | -12.4%           |
| WakeLatency/Default, 0 and 2us (ns/op)         | —               | —               | -3.5% to -6.7%   |
| Sequencer, Parking, IdleCPU, other wake rows   | —               | —               | ~                |

- The shared sequencer depends on prefetching here too (+18-35%; the i7: +19-25%, section 4), and the single-writer
  ring buffer depends on it most (+131%).
- `SharedSequencer/MP_SC/R4` runs 12% faster without the prefetchers, the same direction as the i7's `RingBuffer MP
  SC/R4` (section 5), but here `RingBuffer/MP_SC/R4` is `~`.

Open: the i7-12700K run itself (Golden Cove numbers), the cause of the 60us park-to-wake trip on Linux, and the bimodal
single-producer rows.

## 7. Tooling lessons

- `go test -bench` splits its pattern on `/` per sub-benchmark level, except inside parentheses:
  `(SharedSequencerCapacity|RingBuffer)/SP`, not `SharedSequencerCapacity/SP|RingBuffer/SP`.
- `runuser` and `su` open a PAM session; on Arch, `pam_systemd` then moves the process into a session scope under
  `user.slice`, off the reserved CPUs (`taskset` fails with `EINVAL`). `bench/interleave` uses `setpriv` instead.
- A package-level array goes wherever the linker puts it (one build: 32B off a line boundary), which can differ between
  the variant binaries being compared. Benchmark buffers are now allocated aligned to `CacheLineBytes`.
- Undoing core reservation has two traps: clearing `AllowedCPUs=` leaves running processes narrowed (widen first, then
  clear), and per-user managers (`user@UID.service`) have no cpuset delegated (reset their threads with `taskset`).
- Setup dominates short `perf stat` runs (100ms start-up sleep, scheduler warm-up, allocation): subtract a
  `-test.benchtime 1x` run.

## Pathways forward, ranked

| # | Pathway                             | Evidence                             | Cost     | Status        |
|---|-------------------------------------|--------------------------------------|----------|---------------|
| 1 | Release-store `Commit` (amd64 stub) | Single writer -37-44%, ring -44%     | Small    | Done          |
| 2 | Capacity / full-buffer slow path    | 1K 23x slower (R16); now required    | Low      | Done          |
| 3 | Spinning `WaitStrategy` (opt-in)    | p99 wake ~0.3-0.5us vs 10us-1ms      | Moderate | Done          |
| 4 | Writer batch-boundary false sharing | MP SC/R4 15-18% faster, prefetch off | Low      | Counters next |
| 5 | Multi-producer reserve-1            | `reservedSequence.Add` contention    | High     | Document only |
| 6 | arm64 `LDAPR` barrier loads         | Store is already `STLR`; 3-5% old M5 | Small    | Measure first |

1. **Release-store `Commit`.** Done (section 6), confirmed on reserved cores: single writer 37-44% faster. Zen 2 is
   untested.
2. **Capacity and the slow path.** Done: the cost was the consumer's `Idle` sleep, not the producer's (section 5a), and
   `BufferCapacity` is now required, 64K recommended. The same data motivates pathway 3: a shorter consumer wake shrinks
   the capacity needed.
3. **Spinning wait strategy.** Done (section 6b): opt-in `BusySpinWaitStrategy`, plus the empty-poll `Load`. Open: a
   producer-triggered wake-up on the empty-to-non-empty transition, for low latency without a dedicated core, and an
   escalating idle sleep for the default, whose 500ns sleeps cost 0.58 cores per idle consumer.
4. **Batch-boundary false sharing.** Count store misses in RingBuffer MP SC/R4 with prefetchers on vs off, attributed to
   the writer's entry store; vary batch size and alignment. Likely user guidance rather than a library change.
5. **Multi-producer reserve-1.** Per-writer chunked claims would let an unused claimed slot block every consumer. The
   practical lever is batching (R4 is ~10x faster); document it.
6. **arm64.** No release-store gain is available: Go already compiles `atomic.Int64.Store` to `STLR` and `Load` to
   `LDAR`. The remaining idea is `LDAPR` (RCpc) for barrier loads, which can complete ahead of the same core's earlier
   `STLR`; measure on the macOS laptop before building it.

Ruled out with evidence: 128B padding, concrete dispatch, the unbounded `Load` scan, software `PREFETCHW` in `Commit`
(the release store addresses the same stall directly), and `go:linkname`.

## Design constraints learned

- Keep `committedSlots` dense and sequential. Per-line markers or index permutations (common in other multi-producer
  queues) would defeat the prefetchers the shared sequencer depends on; benchmark any such change with prefetchers on
  and off.
- Keep 64B padding on amd64; `CacheLineBytes` should keep meaning the cache line size.
- Record prefetcher state with every published result (`environment.txt` from `bench/interleave` does).

## Open questions

- Why is `SP4MC` slower with 128B padding (+3.0%, +7.8%)?
- Which cache level serves `committedSlots` lines to the consumer when the prefetchers are off?
- What is the mechanism behind MP MC/R4's +9% without prefetch (no counters collected)?
- Is bit 5 really Intel's "AMP" prefetcher, and what does bit 4 control?
- How much single-producer noise does the release store remove on reserved cores?
