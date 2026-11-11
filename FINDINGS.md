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
  gave 59.6 / 60.4us with them off and 59.6 / 61.0us with them on. The record does not say whether the C-states were
  restored after this diagnostic. The box was later found with no CPU sets and turbo on (2026-11-07), and with every
  idle state enabled on CPUs 0-3 (2026-11-09). The cause is open.
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

The `Parking` column above is the baseline run. The diagnostic column (19.6n) comes from the fence run below, where
the same row measured 38.24n. Compare the fence cost inside the fence run (38.24n against 19.61n), not across the two
runs.

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
  23ns per 16-slot Commit), 9-14ns on the shared sequencer, and nothing measurable on wake latency. Within the fence run at R1,
  Parking costs 38.24ns and BusySpin 11.9ns, so Parking's extra cost is 26.3ns. The fence is 18.6ns of it, about 70%.
  The rest (about 7.7ns: 19.61ns against BusySpin's 11.9ns) is the load of `waiters` and the notify call.
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

## 8. A Go buffered channel against go-disruptor, the hand-off alone (`bench/chanvsring`, appa)

The question: how many messages per second does go-disruptor hand from producers to one consumer, flat out and at a
set rate, against a Go buffered channel? This section measures the hand-off alone: no Auditor, no I/O.

**Tool.** `bench/chanvsring` (commit `0d67a71`). P producer goroutines write one message each into a channel or a
ring slot; one consumer reads every field of it and checks each producer's sequence (a lost, repeated or reordered
message counts as an error, and a checksum covers every field). Both sides do the same work per message, and no other
work.

- Messages: `int64`, and `raw`, a 224-byte struct with the field types and order of vpc-auditor's `contracts.Raw`
  (ring B's slot; `unsafe.Sizeof` is 224 at vpc-auditor `0335a63`, not the 232 of the brief).
- Contestants, all with 16,384 slots: `chan T` of capacity 16,384; go-disruptor with `ParkingWaitStrategy{}` (the
  defaults, as ring B uses it); go-disruptor with `BusySpinWaitStrategy`. One producer uses the single-writer
  sequencer (`WriterCount(1)`, as ring A); 2 to 6 producers use the shared sequencer (`WriterCount(P)`, as ring B).
  Reserve takes 1 slot; a second go-disruptor row reserves 16, flat out only.
- Flat out: the producers send as fast as they can. Paced: 1M, 5M, 10M, 20M and 40M msg/s in total, split evenly,
  on an absolute schedule (cmd/ringload's `due = elapsed x rate`; a late producer sends what is due at once, and never
  skips). A producer that is ahead of its schedule spins on the clock, so each message leaves at its scheduled time.
- Latency: one message in 64 per producer carries its send time (flat out: the clock; paced: the scheduled time, so a
  stall cannot hide its delay). The consumer reads the clock for those messages only. Log-linear histogram, at most
  0.8% high.
- "Kept up": the received rate in the window is at least 99% of the offered rate, and at the end of the window at
  most 16,384 messages (one ring) are due but not received.
- CPU: the whole process from `getrusage` over the window ("cores"), and a CPU profile with goroutine labels split
  into producers, consumer, and "runtime" (samples without a label: the scheduler, GC, idle threads). On Linux the
  three add up to the `getrusage` figure within 0.1 core.
- One process per run: 1 s warm-up, then a 3 s window. Each run prints one benchstat line.
- Tests: the schedule (fake clock), the histogram's percentiles, exactly-once delivery for every contestant, message
  and shape (counts, per-producer order and the checksum, flat out and paced), no goroutine left after a run, and the
  CPU-profile split. `go test -race` passes; the run tests skip under `-short` (each run takes about 200 ms).

**Machine and method.** appa, as section 6d: Intel i9-10850K, Debian 13, kernel 6.12.115, Go 1.27.1, governor
`performance`, MSR `0x1a4` = `0x0` on all 20 CPUs.

| Item        | Value                                                                                                  |
|-------------|--------------------------------------------------------------------------------------------------------|
| Reservation | `bench/reserve --cpus 0-7,10-17 --housekeeping 8,9,18,19`, turbo off                                    |
| Pinning     | `taskset` to the first P+2 of CPUs 0 to 7 (one thread per physical core): P producers, the consumer,     |
|             | and one CPU for the runtime; GOMAXPROCS is Go's default (P+2)                                          |
| Shapes      | P = 1, 2, 4 and 6. P = 8 needs 10 cores with this rule; the reservation has 8, so P = 6 is the largest  |
| Rounds      | `bench/chanvsring/rounds`: 160 rows per round, in a new shuffled order each round, 5 rounds (800 runs) |
| Date        | 2026-11-11, 17:32 to 18:33 UTC; binary sha256 `3ee5bdbe...`                                             |

```bash
sudo bench/reserve --cpus 0-7,10-17 --housekeeping 8,9,18,19 -- \
	bench/chanvsring/rounds --binary ./chanvsring --output /tmp/cvr-reserved --rounds 5 --cpu-order 0,1,2,3,4,5,6,7
```

`bench/interleave` runs test binaries; this tool is a command, so `rounds` does the same job for it. `benchstat`
needs 6 samples for a confidence interval, so these 5-round medians have none. In `benchstat`, every flat-out
difference below is p=0.008 (n=5), except channel against Parking at P = 1 (p=0.222) and P = 2 (p=0.730), which are
`~`. The per-run spread of the flat-out rows is small (for example channel `int64` P=1: 13.1 to 13.8M; Parking: 13.1 to
19.2M, the widest).

The lab's processes ran during the runs (us-street idle, Kafka, two Alloy agents). Before the runs, no process used
more than 0.01 core (`top`, 10 s). `bench/reserve` moved `system.slice`, `user.slice` and `init.scope` to the
housekeeping CPUs. It does not move `nomad.slice`: the Nomad allocations' own processes (us-street) kept CPUs 0 to 19.
They were idle. After the run, turbo, `AllowedCPUs=` (empty on all three units), the user manager's affinity (0-19)
and MSR `0x1a4` (`0x0`) were back to their first state, with no process left in `bench.slice`.

**Headline per shape (reserved, medians of 5 runs).** Flat out: received msg/s with Reserve(1). Paced: the highest
offered rate kept up in at least 3 of 5 runs, with every lower rate kept up too. Latency at 5M msg/s: p50 / p99 from
the scheduled time.

| Message | P | Flat out: channel / Parking / BusySpin | Disruptor vs channel | Kept up: channel / Parking / BusySpin | p50 / p99 at 5M: channel | Parking | BusySpin |
|---------|---|----------------------------------------|----------------------|---------------------------------------|---------------------------|---------|----------|
| int64   | 1 | 13.7M / 14.8M / 24.1M | 1.08x / 1.76x | 10M / 10M / 20M | 35.8 / 70.7 us   | 341 / 587 ns | 313 / 563 ns |
| int64   | 2 | 13.0M / 12.9M / 15.2M | 1.00x / 1.17x | 10M / 10M / 10M | 40.7 / 80.9 us   | 445 / 667 ns | 441 / 659 ns |
| int64   | 4 | 12.4M / 18.2M / 25.3M | 1.47x / 2.05x | 10M / 10M / 20M | 56.3 / 137.2 us  | 437 / 687 ns | 441 / 683 ns |
| int64   | 6 | 11.8M / 19.7M / 29.6M | 1.67x / 2.51x | 5M / 20M / 20M  | 78.3 us / 1.24 ms | 441 / 691 ns | 447 / 695 ns |
| raw     | 1 | 7.5M / 12.6M / 20.1M  | 1.68x / 2.68x | 5M / 10M / 20M  | 39.7 / 77.3 us   | 407 / 651 ns | 415 / 643 ns |
| raw     | 2 | 7.7M / 11.6M / 14.2M  | 1.51x / 1.85x | 5M / 10M / 10M  | 81.9 us / 2.49 ms | 429 / 707 ns | 435 / 735 ns |
| raw     | 4 | 7.0M / 17.8M / 21.7M  | 2.53x / 3.09x | 1M / 10M / 10M  | 3.24 / 3.42 ms (kept 2/5) | 435 / 711 ns | 437 / 691 ns |
| raw     | 6 | 6.9M / 18.6M / 25.6M  | 2.70x / 3.73x | 5M / 10M / 20M  | 2.69 / 3.52 ms   | 457 / 763 ns | 451 / 739 ns |

- **Throughput.** With one producer and `int64`, Parking ties the channel (1.08x, not significant); BusySpin is 1.76x.
  With the 224-byte message, or with 4 or 6 producers, go-disruptor is 1.47x to 2.70x the channel with Parking and
  1.85x to 3.73x with BusySpin. The channel gets slower as producers are added (13.7M to 11.8M for `int64`); go-disruptor
  gets faster from 2 to 6 producers. With two producers on the shared sequencer, go-disruptor is at its slowest (12.9M
  and 15.2M), below one producer; this replicated in all 5 rounds and is not explained here.
- **Batching.** Reserve(16) gives 86M to 153M `int64` msg/s and 30M to 68M `raw` msg/s: 1.6x to 7.9x Reserve(1). A
  channel has no equivalent.
- **At a set rate.** go-disruptor keeps up with 10M msg/s in every shape, and BusySpin with 20M in 5 of 8. The channel
  keeps up with 5M to 10M (`int64`) and 1M to 5M (`raw`).
- **Latency.** At 1M and 5M msg/s, go-disruptor's p50 is 0.3 to 0.5 us and its p99 0.6 to 0.8 us. The channel's p50
  is 30 to 34 us at 1M and 36 us to 3.2 ms at 5M; its p99 is 61 to 586 us at 1M and 71 us to 3.5 ms at 5M. Parking and BusySpin do not differ at these rates (see the
  CPU note below).
- **CPU.** A go-disruptor consumer uses one whole core at every paced rate here, with either wait strategy. The channel
  consumer blocks between messages: consumer plus runtime use 0.11 to 0.24 cores at 1M msg/s and 0.30 to 0.76 at 5M.

**Why Parking costs as much as BusySpin here.** Parking spins 64 times (8 `PAUSE`s each) and yields 16 times before it
parks; section 6d estimates that phase at tens of microseconds on this CPU. At 1M msg/s a message arrives every
microsecond, so the consumer never reaches the park. Parking saves CPU only when the gaps are longer than the spin
phase: idle or sparse traffic (section 6d: 0.017 cores idle). These runs do not include such a rate.

**Why the paced channel's p50 is about 30 us even at 1M msg/s.** The consumer blocks on an empty channel. A send
makes it runnable on the producer's P, but the producer keeps running (it spins on the clock), so an idle P must steal
the consumer (the Go scheduler waits about 3 us before it steals a P's next goroutine while that P runs) and wake a
thread. A producer that blocked or slept between sends would let the consumer run sooner. The spin pacing is part of
this result, and so is the rule that a woken channel receiver waits for a scheduler thread. go-disruptor's consumer
never blocks, so it does not pay this.

**Producer cores are the pacing spin.** A paced producer spins on the clock until its next message is due, so each
producer shows about 1 core at every rate. Channel producers show less when they block on a full channel. Compare the
consumer and runtime columns, not "cores", for the cost of the hand-off.

**Flat-out latency is queue length.** Flat out, the channel is mostly full: its p50 is up to 16,384 / rate (0.14 to
2.4 ms). go-disruptor at Reserve(1) stays near empty (sub-microsecond latency), because its consumer is faster than its
producers. At Reserve(16) with 4 or 6 producers, the ring fills, and the latency is 60 to 190 us.

**What this does not measure.** The whole Auditor (parsing, the assembler, the replay log, Kafka), any I/O, more than
one consumer or handler group, P = 8, other capacities, and rates below 1M msg/s, where Parking parks. The `raw`
message carries pointers (a slice, strings, an interface) set to shared values; nothing is allocated per message, so
the GC hardly runs.

**Every table (reserved).** "runtime" is CPU without a goroutine label. "most behind" is the most messages one producer
was behind its schedule inside the window.

**Flat out, msg=int64: received msg/s (median).**

| P | channel | parking | busyspin | parking/channel | busyspin/channel | parking R16 | busyspin R16 |
|---|---|---|---|---|---|---|---|
| 1 | 13.7M | 14.8M | 24.1M | 1.08x | 1.76x | 99.9M | 116.9M |
| 2 | 13.0M | 12.9M | 15.2M | 1.00x | 1.17x | 86.1M | 114.6M |
| 4 | 12.4M | 18.2M | 25.3M | 1.47x | 2.05x | 144.0M | 152.7M |
| 6 | 11.8M | 19.7M | 29.6M | 1.67x | 2.51x | 150.9M | 153.2M |

**Flat out, msg=int64: CPU cores (total / producers / consumer / runtime) and latency p50 / p99.**

| P | contestant | cores | producers | consumer | runtime | p50 | p99 |
|---|---|---|---|---|---|---|---|
| 1 | channel | 1.95 | 0.96 | 0.98 | 0.00 | 548.9 us | 1.16 ms |
| 1 | parking | 2.00 | 0.99 | 0.99 | 0.00 | 202 ns | 555 ns |
| 1 | busyspin | 2.00 | 0.99 | 0.99 | 0.00 | 265 ns | 495 ns |
| 1 | parking R16 | 2.00 | 0.99 | 0.99 | 0.00 | 333 ns | 683 ns |
| 1 | busyspin R16 | 2.00 | 0.99 | 0.99 | 0.00 | 385 ns | 759 ns |
| 2 | channel | 2.02 | 1.11 | 0.87 | 0.04 | 1.20 ms | 1.28 ms |
| 2 | parking | 3.00 | 1.99 | 1.00 | 0.00 | 313 ns | 695 ns |
| 2 | busyspin | 3.00 | 1.99 | 0.99 | 0.00 | 353 ns | 763 ns |
| 2 | parking R16 | 3.00 | 2.00 | 0.99 | 0.00 | 619 ns | 1.0 us |
| 2 | busyspin R16 | 3.00 | 1.99 | 0.99 | 0.00 | 891 ns | 1.4 us |
| 4 | channel | 2.03 | 1.09 | 0.78 | 0.08 | 1.28 ms | 1.35 ms |
| 4 | parking | 5.00 | 3.98 | 1.00 | 0.00 | 415 ns | 771 ns |
| 4 | busyspin | 5.00 | 3.99 | 0.99 | 0.00 | 501 ns | 787 ns |
| 4 | parking R16 | 4.99 | 3.97 | 0.99 | 0.00 | 63.7 us | 75.8 us |
| 4 | busyspin R16 | 5.00 | 3.99 | 1.00 | 0.00 | 62.2 us | 74.2 us |
| 6 | channel | 2.06 | 1.11 | 0.72 | 0.12 | 1.36 ms | 1.43 ms |
| 6 | parking | 7.00 | 5.97 | 1.00 | 0.00 | 507 ns | 1.0 us |
| 6 | busyspin | 6.98 | 5.94 | 0.99 | 0.00 | 607 ns | 1.0 us |
| 6 | parking R16 | 6.98 | 5.95 | 0.99 | 0.00 | 60.9 us | 71.7 us |
| 6 | busyspin R16 | 7.00 | 5.97 | 0.99 | 0.00 | 63.0 us | 73.7 us |

**Flat out, msg=raw: received msg/s (median).**

| P | channel | parking | busyspin | parking/channel | busyspin/channel | parking R16 | busyspin R16 |
|---|---|---|---|---|---|---|---|
| 1 | 7.5M | 12.6M | 20.1M | 1.68x | 2.68x | 30.0M | 32.2M |
| 2 | 7.7M | 11.6M | 14.2M | 1.51x | 1.85x | 30.5M | 30.7M |
| 4 | 7.0M | 17.8M | 21.7M | 2.53x | 3.09x | 68.2M | 63.0M |
| 6 | 6.9M | 18.6M | 25.6M | 2.70x | 3.73x | 65.0M | 64.8M |

**Flat out, msg=raw: CPU cores (total / producers / consumer / runtime) and latency p50 / p99.**

| P | contestant | cores | producers | consumer | runtime | p50 | p99 |
|---|---|---|---|---|---|---|---|
| 1 | channel | 1.98 | 0.99 | 0.98 | 0.00 | 136.2 us | 892.9 us |
| 1 | parking | 2.00 | 0.99 | 1.00 | 0.00 | 337 ns | 643 ns |
| 1 | busyspin | 2.00 | 0.99 | 1.00 | 0.00 | 375 ns | 691 ns |
| 1 | parking R16 | 2.00 | 0.99 | 1.00 | 0.00 | 831 ns | 1.2 us |
| 1 | busyspin R16 | 2.00 | 0.99 | 1.00 | 0.00 | 803 ns | 1.1 us |
| 2 | channel | 2.03 | 1.11 | 0.83 | 0.03 | 2.06 ms | 2.18 ms |
| 2 | parking | 3.00 | 1.99 | 1.00 | 0.00 | 375 ns | 787 ns |
| 2 | busyspin | 3.00 | 1.99 | 0.99 | 0.00 | 411 ns | 863 ns |
| 2 | parking R16 | 3.00 | 1.99 | 0.99 | 0.00 | 1.5 us | 2.2 us |
| 2 | busyspin R16 | 3.00 | 1.99 | 1.00 | 0.00 | 1.7 us | 2.3 us |
| 4 | channel | 2.13 | 1.28 | 0.71 | 0.06 | 2.28 ms | 2.38 ms |
| 4 | parking | 5.00 | 3.98 | 1.00 | 0.00 | 417 ns | 855 ns |
| 4 | busyspin | 5.00 | 3.98 | 1.00 | 0.00 | 527 ns | 931 ns |
| 4 | parking R16 | 4.99 | 3.97 | 1.00 | 0.00 | 89.6 us | 133.1 us |
| 4 | busyspin R16 | 5.00 | 3.98 | 1.00 | 0.00 | 60.7 us | 123.4 us |
| 6 | channel | 2.09 | 1.16 | 0.71 | 0.11 | 2.34 ms | 2.44 ms |
| 6 | parking | 7.00 | 5.97 | 1.00 | 0.00 | 543 ns | 1.1 us |
| 6 | busyspin | 7.00 | 5.98 | 1.00 | 0.00 | 743 ns | 1.2 us |
| 6 | parking R16 | 6.46 | 5.26 | 0.97 | 0.20 | 129.0 us | 189.4 us |
| 6 | busyspin R16 | 7.00 | 5.97 | 1.00 | 0.00 | 130.0 us | 156.7 us |

**At a shared rate.** Cores are consumer plus runtime / the whole process (the rest is the producers' pacing spin).

**At 1M msg/s, msg=int64: consumer+runtime cores / all cores, and p50 / p99 / p999 from the scheduled time.**

| P | channel cores | parking cores | busyspin cores | channel p50 / p99 / p999 | parking p50 / p99 / p999 | busyspin p50 / p99 / p999 |
|---|---|---|---|---|---|---|
| 1 | 0.11 / 1.14 | 0.99 / 2.00 | 0.99 / 2.00 | 31.7 us / 585.7 us / 3.18 ms | 373 ns / 579 ns / 67.1 us | 349 ns / 587 ns / 19.1 us |
| 2 | 0.14 / 2.14 | 1.00 / 3.00 | 0.99 / 3.00 | 31.0 us / 62.7 us / 2.42 ms | 409 ns / 615 ns / 855 ns | 411 ns / 623 ns / 5.1 us |
| 4 | 0.13 / 4.13 | 1.00 / 5.00 | 0.99 / 5.00 | 29.9 us / 60.9 us / 1.95 ms | 421 ns / 615 ns / 711 ns | 419 ns / 623 ns / 843 ns |
| 6 | 0.12 / 6.13 | 1.00 / 7.00 | 0.99 / 7.00 | 29.9 us / 60.7 us / 782.3 us | 421 ns / 623 ns / 5.9 us | 423 ns / 611 ns / 2.5 us |

**At 1M msg/s, msg=raw: consumer+runtime cores / all cores, and p50 / p99 / p999 from the scheduled time.**

| P | channel cores | parking cores | busyspin cores | channel p50 / p99 / p999 | parking p50 / p99 / p999 | busyspin p50 / p99 / p999 |
|---|---|---|---|---|---|---|
| 1 | 0.16 / 1.19 | 0.99 / 2.00 | 0.99 / 2.00 | 34.0 us / 65.0 us / 2.62 ms | 403 ns / 631 ns / 32.0 us | 401 ns / 607 ns / 667 ns |
| 2 | 0.18 / 2.18 | 0.99 / 3.00 | 1.00 / 3.00 | 32.3 us / 63.7 us / 1.29 ms | 443 ns / 635 ns / 1.8 us | 421 ns / 635 ns / 2.0 us |
| 4 | 0.20 / 4.20 | 1.00 / 5.00 | 1.00 / 5.00 | 30.9 us / 63.2 us / 1.24 ms | 423 ns / 623 ns / 3.6 us | 427 ns / 635 ns / 2.2 us |
| 6 | 0.24 / 6.05 | 1.00 / 7.00 | 0.99 / 7.00 | 33.3 us / 68.1 us / 1.52 ms | 429 ns / 639 ns / 6.9 us | 435 ns / 631 ns / 7.6 us |

**At 5M msg/s, msg=int64: consumer+runtime cores / all cores, and p50 / p99 / p999 from the scheduled time.**

| P | channel cores | parking cores | busyspin cores | channel p50 / p99 / p999 | parking p50 / p99 / p999 | busyspin p50 / p99 / p999 |
|---|---|---|---|---|---|---|
| 1 | 0.30 / 1.34 | 0.99 / 2.00 | 1.00 / 2.00 | 35.8 us / 70.7 us / 757.8 us | 341 ns / 587 ns / 40.2 us | 313 ns / 563 ns / 24.1 us |
| 2 | 0.36 / 2.23 | 1.00 / 3.00 | 1.00 / 3.00 | 40.7 us / 80.9 us / 1.24 ms | 445 ns / 667 ns / 6.7 us | 441 ns / 659 ns / 4.5 us |
| 4 | 0.48 / 3.26 | 1.00 / 5.00 | 1.00 / 5.00 | 56.3 us / 137.2 us / 966.7 us | 437 ns / 687 ns / 6.5 us | 441 ns / 683 ns / 5.5 us |
| 6 | 0.46 / 3.67 | 0.99 / 6.99 | 1.00 / 7.00 | 78.3 us / 1.24 ms / 5.14 ms | 441 ns / 691 ns / 8.0 us | 447 ns / 695 ns / 8.2 us |

**At 5M msg/s, msg=raw: consumer+runtime cores / all cores, and p50 / p99 / p999 from the scheduled time.**

| P | channel cores | parking cores | busyspin cores | channel p50 / p99 / p999 | parking p50 / p99 / p999 | busyspin p50 / p99 / p999 |
|---|---|---|---|---|---|---|
| 1 | 0.60 / 1.61 | 1.00 / 2.00 | 0.99 / 2.00 | 39.7 us / 77.3 us / 159.7 us | 407 ns / 651 ns / 7.4 us | 415 ns / 643 ns / 7.1 us |
| 2 | 0.76 / 2.38 | 1.00 / 3.00 | 0.99 / 3.00 | 81.9 us / 2.49 ms / 3.34 ms | 429 ns / 707 ns / 4.6 us | 435 ns / 735 ns / 9.2 us |
| 4 | 0.73 / 2.68 | 1.00 / 5.00 | 1.00 / 5.00 | 3.24 ms / 3.42 ms / 3.52 ms (kept 2/5) | 435 ns / 711 ns / 6.4 us | 437 ns / 691 ns / 4.8 us |
| 6 | 0.66 / 2.59 | 1.00 / 7.00 | 0.99 / 6.99 | 2.69 ms / 3.52 ms / 4.36 ms | 457 ns / 763 ns / 8.9 us | 451 ns / 739 ns / 8.2 us |


**Paced, msg=int64: highest offered rate kept up (kept in at least 3 of 5 runs).**

| P | channel | parking | busyspin |
|---|---|---|---|
| 1 | 10M | 10M | 20M |
| 2 | 10M | 10M | 10M |
| 4 | 10M | 10M | 20M |
| 6 | 5M | 20M | 20M |

**Paced, msg=int64: every row (median of the runs; latency from the scheduled time).**

| P | rate | contestant | achieved | kept | cores | consumer | runtime | cores per 1M/s | p50 | p99 | p999 | most behind |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 1M | channel | 1.0M | 5/5 | 1.14 | 0.04 | 0.07 | 1.144 | 31.7 us | 585.7 us | 3.18 ms | 300 |
| 1 | 1M | parking | 1.0M | 5/5 | 2.00 | 0.99 | 0.00 | 2.001 | 373 ns | 579 ns | 67.1 us | 236 |
| 1 | 1M | busyspin | 1.0M | 5/5 | 2.00 | 0.99 | 0.00 | 1.999 | 349 ns | 587 ns | 19.1 us | 302 |
| 1 | 5M | channel | 5.0M | 5/5 | 1.34 | 0.24 | 0.06 | 0.268 | 35.8 us | 70.7 us | 757.8 us | 1426 |
| 1 | 5M | parking | 5.0M | 5/5 | 2.00 | 0.99 | 0.00 | 0.400 | 341 ns | 587 ns | 40.2 us | 1517 |
| 1 | 5M | busyspin | 5.0M | 5/5 | 2.00 | 1.00 | 0.00 | 0.400 | 313 ns | 563 ns | 24.1 us | 1390 |
| 1 | 10M | channel | 10.0M | 5/5 | 1.68 | 0.64 | 0.02 | 0.168 | 41.0 us | 79.9 us | 223.2 us | 3050 |
| 1 | 10M | parking | 10.0M | 5/5 | 2.00 | 1.00 | 0.00 | 0.200 | 285 ns | 663 ns | 25.2 us | 2123 |
| 1 | 10M | busyspin | 10.0M | 5/5 | 2.00 | 0.99 | 0.00 | 0.200 | 259 ns | 639 ns | 65.3 us | 3009 |
| 1 | 20M | channel | 13.6M | 0/5 | 1.96 | 0.98 | 0.01 | 0.142 | 796.90 ms | 1.27 s | 1.27 s | 25410000 |
| 1 | 20M | parking | 14.3M | 0/5 | 2.00 | 1.00 | 0.00 | 0.140 | 362.80 ms | 885.00 ms | 893.40 ms | 17880000 |
| 1 | 20M | busyspin | 20.0M | 5/5 | 2.00 | 0.99 | 0.00 | 0.100 | 551 ns | 1.8 us | 132.1 us | 6168 |
| 1 | 40M | channel | 13.7M | 0/5 | 1.96 | 0.98 | 0.00 | 0.143 | 1.64 s | 2.62 s | 2.63 s | 105300000 |
| 1 | 40M | parking | 15.2M | 0/5 | 2.00 | 0.99 | 0.00 | 0.132 | 1.51 s | 2.47 s | 2.48 s | 99090000 |
| 1 | 40M | busyspin | 38.9M | 2/5 | 2.00 | 0.99 | 0.00 | 0.051 | 62.91 ms | 112.20 ms | 113.80 ms | 4551000 |
| 2 | 1M | channel | 1.0M | 5/5 | 2.14 | 0.06 | 0.08 | 2.142 | 31.0 us | 62.7 us | 2.42 ms | 147 |
| 2 | 1M | parking | 1.0M | 5/5 | 3.00 | 1.00 | 0.00 | 3.003 | 409 ns | 615 ns | 855 ns | 149 |
| 2 | 1M | busyspin | 1.0M | 5/5 | 3.00 | 0.99 | 0.00 | 3.001 | 411 ns | 623 ns | 5.1 us | 125 |
| 2 | 5M | channel | 5.0M | 5/5 | 2.23 | 0.30 | 0.05 | 0.445 | 40.7 us | 80.9 us | 1.24 ms | 577 |
| 2 | 5M | parking | 5.0M | 5/5 | 3.00 | 1.00 | 0.00 | 0.600 | 445 ns | 667 ns | 6.7 us | 736 |
| 2 | 5M | busyspin | 5.0M | 5/5 | 3.00 | 1.00 | 0.00 | 0.601 | 441 ns | 659 ns | 4.5 us | 537 |
| 2 | 10M | channel | 10.0M | 5/5 | 2.32 | 0.75 | 0.02 | 0.232 | 74.2 us | 185.3 us | 1.03 ms | 8762 |
| 2 | 10M | parking | 10.0M | 5/5 | 3.00 | 1.00 | 0.00 | 0.300 | 471 ns | 879 ns | 24.3 us | 1448 |
| 2 | 10M | busyspin | 10.0M | 5/5 | 3.00 | 0.99 | 0.00 | 0.300 | 403 ns | 775 ns | 6.7 us | 1457 |
| 2 | 20M | channel | 13.2M | 0/5 | 2.03 | 0.87 | 0.02 | 0.153 | 843.10 ms | 1.34 s | 1.36 s | 13600000 |
| 2 | 20M | parking | 13.2M | 0/5 | 3.00 | 1.00 | 0.00 | 0.227 | 864.00 ms | 1.36 s | 1.38 s | 13720000 |
| 2 | 20M | busyspin | 14.9M | 0/5 | 3.00 | 1.00 | 0.00 | 0.201 | 541.10 ms | 1.23 s | 1.24 s | 12430000 |
| 2 | 40M | channel | 13.6M | 0/5 | 2.05 | 0.88 | 0.03 | 0.151 | 1.65 s | 2.62 s | 2.65 s | 52930000 |
| 2 | 40M | parking | 13.0M | 0/5 | 3.00 | 0.99 | 0.00 | 0.231 | 1.70 s | 2.70 s | 2.73 s | 54610000 |
| 2 | 40M | busyspin | 15.2M | 0/5 | 3.00 | 0.99 | 0.00 | 0.197 | 1.55 s | 2.55 s | 2.60 s | 51820000 |
| 4 | 1M | channel | 1.0M | 5/5 | 4.13 | 0.05 | 0.08 | 4.133 | 29.9 us | 60.9 us | 1.95 ms | 70 |
| 4 | 1M | parking | 1.0M | 5/5 | 5.00 | 1.00 | 0.00 | 5.001 | 421 ns | 615 ns | 711 ns | 54 |
| 4 | 1M | busyspin | 1.0M | 5/5 | 5.00 | 0.99 | 0.00 | 5.002 | 419 ns | 623 ns | 843 ns | 71 |
| 4 | 5M | channel | 5.0M | 5/5 | 3.26 | 0.45 | 0.03 | 0.652 | 56.3 us | 137.2 us | 966.7 us | 2361 |
| 4 | 5M | parking | 5.0M | 5/5 | 5.00 | 1.00 | 0.00 | 1.000 | 437 ns | 687 ns | 6.5 us | 1900 |
| 4 | 5M | busyspin | 5.0M | 5/5 | 5.00 | 1.00 | 0.00 | 1.000 | 441 ns | 683 ns | 5.5 us | 353 |
| 4 | 10M | channel | 10.0M | 4/5 | 2.37 | 0.71 | 0.03 | 0.237 | 1.59 ms | 2.05 ms | 2.85 ms | 7822 |
| 4 | 10M | parking | 10.0M | 5/5 | 5.00 | 0.99 | 0.00 | 0.500 | 439 ns | 747 ns | 7.5 us | 3903 |
| 4 | 10M | busyspin | 10.0M | 5/5 | 5.00 | 1.00 | 0.00 | 0.500 | 469 ns | 807 ns | 6.6 us | 709 |
| 4 | 20M | channel | 12.9M | 0/5 | 2.04 | 0.76 | 0.08 | 0.159 | 885.00 ms | 1.42 s | 1.43 s | 7159000 |
| 4 | 20M | parking | 19.5M | 2/5 | 5.00 | 1.00 | 0.00 | 0.256 | 53.74 ms | 176.20 ms | 180.40 ms | 901800 |
| 4 | 20M | busyspin | 20.0M | 5/5 | 5.00 | 1.00 | 0.00 | 0.250 | 611 ns | 5.4 us | 181.2 us | 1435 |
| 4 | 40M | channel | 12.7M | 0/5 | 2.05 | 0.77 | 0.09 | 0.162 | 1.71 s | 2.72 s | 2.75 s | 27410000 |
| 4 | 40M | parking | 19.7M | 0/5 | 5.00 | 1.00 | 0.00 | 0.253 | 1.27 s | 2.04 s | 2.08 s | 20810000 |
| 4 | 40M | busyspin | 24.2M | 0/5 | 5.00 | 0.99 | 0.00 | 0.207 | 968.90 ms | 1.68 s | 1.71 s | 17120000 |
| 6 | 1M | channel | 1.0M | 5/5 | 6.13 | 0.06 | 0.06 | 6.133 | 29.9 us | 60.7 us | 782.3 us | 45 |
| 6 | 1M | parking | 1.0M | 5/5 | 7.00 | 1.00 | 0.00 | 6.998 | 421 ns | 623 ns | 5.9 us | 46 |
| 6 | 1M | busyspin | 1.0M | 5/5 | 7.00 | 0.99 | 0.00 | 7.001 | 423 ns | 611 ns | 2.5 us | 46 |
| 6 | 5M | channel | 5.0M | 5/5 | 3.67 | 0.44 | 0.02 | 0.733 | 78.3 us | 1.24 ms | 5.14 ms | 4693 |
| 6 | 5M | parking | 5.0M | 5/5 | 6.99 | 0.99 | 0.00 | 1.399 | 441 ns | 691 ns | 8.0 us | 1641 |
| 6 | 5M | busyspin | 5.0M | 5/5 | 7.00 | 1.00 | 0.00 | 1.400 | 447 ns | 695 ns | 8.2 us | 229 |
| 6 | 10M | channel | 10.0M | 1/5 | 2.33 | 0.65 | 0.08 | 0.233 | 1.67 ms | 2.09 ms | 2.31 ms | 1894 |
| 6 | 10M | parking | 10.0M | 5/5 | 7.00 | 1.00 | 0.00 | 0.700 | 485 ns | 827 ns | 7.5 us | 588 |
| 6 | 10M | busyspin | 10.0M | 5/5 | 7.00 | 0.99 | 0.00 | 0.700 | 465 ns | 831 ns | 10.0 us | 473 |
| 6 | 20M | channel | 12.2M | 0/5 | 2.06 | 0.73 | 0.10 | 0.169 | 968.90 ms | 1.55 s | 1.59 s | 5318000 |
| 6 | 20M | parking | 20.0M | 5/5 | 7.00 | 1.00 | 0.00 | 0.350 | 707 ns | 3.0 us | 184.3 us | 1617 |
| 6 | 20M | busyspin | 20.0M | 5/5 | 7.00 | 1.00 | 0.00 | 0.350 | 691 ns | 1.2 us | 10.9 us | 991 |
| 6 | 40M | channel | 12.2M | 0/5 | 2.07 | 0.73 | 0.11 | 0.170 | 1.74 s | 2.77 s | 2.79 s | 18610000 |
| 6 | 40M | parking | 20.4M | 0/5 | 7.00 | 0.99 | 0.00 | 0.343 | 1.23 s | 2.00 s | 2.04 s | 13610000 |
| 6 | 40M | busyspin | 31.4M | 0/5 | 7.00 | 0.99 | 0.00 | 0.223 | 518.00 ms | 1.04 s | 1.08 s | 7223000 |

**Paced, msg=raw: highest offered rate kept up (kept in at least 3 of 5 runs).**

| P | channel | parking | busyspin |
|---|---|---|---|
| 1 | 5M | 10M | 20M |
| 2 | 5M | 10M | 10M |
| 4 | 1M | 10M | 10M |
| 6 | 5M | 10M | 20M |

**Paced, msg=raw: every row (median of the runs; latency from the scheduled time).**

| P | rate | contestant | achieved | kept | cores | consumer | runtime | cores per 1M/s | p50 | p99 | p999 | most behind |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 1M | channel | 1.0M | 5/5 | 1.19 | 0.10 | 0.06 | 1.194 | 34.0 us | 65.0 us | 2.62 ms | 240 |
| 1 | 1M | parking | 1.0M | 5/5 | 2.00 | 0.99 | 0.00 | 2.000 | 403 ns | 631 ns | 32.0 us | 306 |
| 1 | 1M | busyspin | 1.0M | 5/5 | 2.00 | 0.99 | 0.00 | 2.002 | 401 ns | 607 ns | 667 ns | 47 |
| 1 | 5M | channel | 5.0M | 5/5 | 1.61 | 0.57 | 0.03 | 0.323 | 39.7 us | 77.3 us | 159.7 us | 1541 |
| 1 | 5M | parking | 5.0M | 5/5 | 2.00 | 1.00 | 0.00 | 0.400 | 407 ns | 651 ns | 7.4 us | 1165 |
| 1 | 5M | busyspin | 5.0M | 5/5 | 2.00 | 0.99 | 0.00 | 0.400 | 415 ns | 643 ns | 7.1 us | 1504 |
| 1 | 10M | channel | 7.6M | 0/5 | 1.95 | 0.95 | 0.00 | 0.256 | 616.60 ms | 968.90 ms | 977.30 ms | 9753000 |
| 1 | 10M | parking | 10.0M | 5/5 | 2.00 | 1.00 | 0.00 | 0.200 | 491 ns | 939 ns | 64.0 us | 147 |
| 1 | 10M | busyspin | 10.0M | 5/5 | 2.00 | 1.00 | 0.00 | 0.200 | 375 ns | 767 ns | 5.3 us | 2148 |
| 1 | 20M | channel | 7.8M | 0/5 | 1.99 | 0.98 | 0.00 | 0.254 | 1.54 s | 2.45 s | 2.47 s | 49080000 |
| 1 | 20M | parking | 13.9M | 0/5 | 2.00 | 0.99 | 0.00 | 0.143 | 775.90 ms | 1.23 s | 1.23 s | 24580000 |
| 1 | 20M | busyspin | 20.0M | 5/5 | 2.00 | 0.99 | 0.00 | 0.100 | 3.0 us | 1.29 ms | 2.07 ms | 46580 |
| 1 | 40M | channel | 7.8M | 0/5 | 1.98 | 0.97 | 0.00 | 0.253 | 2.02 s | 3.20 s | 3.22 s | 128600000 |
| 1 | 40M | parking | 14.1M | 0/5 | 2.00 | 0.99 | 0.00 | 0.141 | 1.64 s | 2.58 s | 2.60 s | 104000000 |
| 1 | 40M | busyspin | 20.0M | 0/5 | 2.00 | 1.00 | 0.00 | 0.100 | 1.26 s | 1.99 s | 2.00 s | 79850000 |
| 2 | 1M | channel | 1.0M | 5/5 | 2.18 | 0.11 | 0.07 | 2.182 | 32.3 us | 63.7 us | 1.29 ms | 114 |
| 2 | 1M | parking | 1.0M | 5/5 | 3.00 | 0.99 | 0.00 | 3.001 | 443 ns | 635 ns | 1.8 us | 150 |
| 2 | 1M | busyspin | 1.0M | 5/5 | 3.00 | 1.00 | 0.00 | 3.002 | 421 ns | 635 ns | 2.0 us | 148 |
| 2 | 5M | channel | 5.0M | 5/5 | 2.38 | 0.76 | 0.01 | 0.475 | 81.9 us | 2.49 ms | 3.34 ms | 5516 |
| 2 | 5M | parking | 5.0M | 5/5 | 3.00 | 1.00 | 0.00 | 0.600 | 429 ns | 707 ns | 4.6 us | 739 |
| 2 | 5M | busyspin | 5.0M | 5/5 | 3.00 | 0.99 | 0.00 | 0.600 | 435 ns | 735 ns | 9.2 us | 749 |
| 2 | 10M | channel | 7.8M | 0/5 | 2.01 | 0.86 | 0.03 | 0.259 | 553.60 ms | 893.40 ms | 906.00 ms | 4528000 |
| 2 | 10M | parking | 10.0M | 5/5 | 3.00 | 1.00 | 0.00 | 0.300 | 559 ns | 3.33 ms | 5.37 ms | 28210 |
| 2 | 10M | busyspin | 10.0M | 5/5 | 3.00 | 0.99 | 0.00 | 0.300 | 447 ns | 947 ns | 91.1 us | 1484 |
| 2 | 20M | channel | 7.8M | 0/5 | 2.01 | 0.85 | 0.03 | 0.259 | 1.53 s | 2.47 s | 2.50 s | 24890000 |
| 2 | 20M | parking | 11.9M | 0/5 | 3.00 | 0.99 | 0.00 | 0.252 | 985.70 ms | 1.84 s | 1.88 s | 18750000 |
| 2 | 20M | busyspin | 13.8M | 0/5 | 3.00 | 1.00 | 0.00 | 0.217 | 771.80 ms | 1.43 s | 1.44 s | 14390000 |
| 2 | 40M | channel | 7.8M | 0/5 | 2.02 | 0.84 | 0.03 | 0.257 | 2.01 s | 3.20 s | 3.24 s | 64680000 |
| 2 | 40M | parking | 11.8M | 0/5 | 3.00 | 1.00 | 0.00 | 0.255 | 1.78 s | 2.87 s | 2.92 s | 58310000 |
| 2 | 40M | busyspin | 13.9M | 0/5 | 3.00 | 0.99 | 0.00 | 0.216 | 1.63 s | 2.70 s | 2.73 s | 54780000 |
| 4 | 1M | channel | 1.0M | 5/5 | 4.20 | 0.14 | 0.06 | 4.202 | 30.9 us | 63.2 us | 1.24 ms | 80 |
| 4 | 1M | parking | 1.0M | 5/5 | 5.00 | 1.00 | 0.00 | 5.001 | 423 ns | 623 ns | 3.6 us | 73 |
| 4 | 1M | busyspin | 1.0M | 5/5 | 5.00 | 1.00 | 0.00 | 5.001 | 427 ns | 635 ns | 2.2 us | 70 |
| 4 | 5M | channel | 5.0M | 2/5 | 2.68 | 0.68 | 0.05 | 0.536 | 3.24 ms | 3.42 ms | 3.52 ms | 707 |
| 4 | 5M | parking | 5.0M | 5/5 | 5.00 | 1.00 | 0.00 | 1.000 | 435 ns | 711 ns | 6.4 us | 365 |
| 4 | 5M | busyspin | 5.0M | 5/5 | 5.00 | 1.00 | 0.00 | 1.000 | 437 ns | 691 ns | 4.8 us | 363 |
| 4 | 10M | channel | 7.0M | 0/5 | 2.13 | 0.72 | 0.07 | 0.305 | 738.20 ms | 1.27 s | 1.30 s | 3259000 |
| 4 | 10M | parking | 10.0M | 5/5 | 5.00 | 0.99 | 0.00 | 0.500 | 473 ns | 871 ns | 13.3 us | 722 |
| 4 | 10M | busyspin | 10.0M | 5/5 | 5.00 | 0.99 | 0.00 | 0.500 | 443 ns | 847 ns | 10.1 us | 709 |
| 4 | 20M | channel | 6.8M | 0/5 | 2.14 | 0.72 | 0.08 | 0.312 | 1.65 s | 2.63 s | 2.68 s | 13440000 |
| 4 | 20M | parking | 17.2M | 0/5 | 5.00 | 1.00 | 0.00 | 0.291 | 295.70 ms | 725.60 ms | 746.60 ms | 3734000 |
| 4 | 20M | busyspin | 19.9M | 1/5 | 5.00 | 1.00 | 0.00 | 0.250 | 573.4 us | 91.23 ms | 91.75 ms | 459700 |
| 4 | 40M | channel | 6.9M | 0/5 | 2.20 | 0.72 | 0.07 | 0.320 | 2.08 s | 3.29 s | 3.32 s | 33200000 |
| 4 | 40M | parking | 17.6M | 0/5 | 5.00 | 0.99 | 0.00 | 0.284 | 1.39 s | 2.33 s | 2.37 s | 23610000 |
| 4 | 40M | busyspin | 22.3M | 0/5 | 5.00 | 1.00 | 0.00 | 0.224 | 1.07 s | 2.04 s | 2.11 s | 21090000 |
| 6 | 1M | channel | 1.0M | 5/5 | 6.05 | 0.19 | 0.05 | 6.054 | 33.3 us | 68.1 us | 1.52 ms | 171 |
| 6 | 1M | parking | 1.0M | 5/5 | 7.00 | 1.00 | 0.00 | 7.001 | 429 ns | 639 ns | 6.9 us | 46 |
| 6 | 1M | busyspin | 1.0M | 5/5 | 7.00 | 0.99 | 0.00 | 7.002 | 435 ns | 631 ns | 7.6 us | 46 |
| 6 | 5M | channel | 5.0M | 3/5 | 2.59 | 0.63 | 0.03 | 0.518 | 2.69 ms | 3.52 ms | 4.36 ms | 2116 |
| 6 | 5M | parking | 5.0M | 5/5 | 7.00 | 1.00 | 0.00 | 1.400 | 457 ns | 763 ns | 8.9 us | 404 |
| 6 | 5M | busyspin | 5.0M | 5/5 | 6.99 | 0.99 | 0.00 | 1.399 | 451 ns | 739 ns | 8.2 us | 239 |
| 6 | 10M | channel | 7.0M | 0/5 | 2.06 | 0.73 | 0.11 | 0.296 | 755.00 ms | 1.32 s | 1.39 s | 2328000 |
| 6 | 10M | parking | 10.0M | 5/5 | 7.00 | 0.99 | 0.00 | 0.700 | 501 ns | 947 ns | 9.3 us | 778 |
| 6 | 10M | busyspin | 10.0M | 5/5 | 7.00 | 1.00 | 0.00 | 0.700 | 473 ns | 895 ns | 8.2 us | 457 |
| 6 | 20M | channel | 7.0M | 0/5 | 2.08 | 0.72 | 0.12 | 0.298 | 1.64 s | 2.60 s | 2.63 s | 8778000 |
| 6 | 20M | parking | 19.7M | 1/5 | 7.00 | 1.00 | 0.00 | 0.355 | 29.62 ms | 114.80 ms | 116.90 ms | 390100 |
| 6 | 20M | busyspin | 20.0M | 5/5 | 7.00 | 0.99 | 0.00 | 0.350 | 791 ns | 2.1 us | 42.8 us | 908 |
| 6 | 40M | channel | 6.9M | 0/5 | 2.07 | 0.75 | 0.12 | 0.299 | 2.07 s | 3.29 s | 3.34 s | 22260000 |
| 6 | 40M | parking | 19.7M | 0/5 | 7.00 | 0.99 | 0.00 | 0.355 | 1.29 s | 2.06 s | 2.11 s | 14030000 |
| 6 | 40M | busyspin | 25.8M | 0/5 | 7.00 | 1.00 | 0.00 | 0.272 | 868.20 ms | 1.51 s | 1.58 s | 10540000 |

**Unreserved pass, turbo on (for comparison only).** The same binary and pinning, without `bench/reserve`, turbo on,
the flat-out rows and the 5M rows, 3 shuffled rounds, 2026-11-11 18:34 to 18:49 UTC. From 18:40 UTC appa's Nomad
client lost its server over the network (the Mac's ssh to appa failed then too); the last 9 minutes of this pass ran
during that outage. With turbo, the flat-out rows change by -4% to +77% (most by +25% to +45%). The disruptor-to-channel
ratios stay within -10% and +17% of the reserved ones, so the verdict does not change.
**Flat out, msg=int64: received msg/s (median).**

| P | channel | parking | busyspin | parking/channel | busyspin/channel | parking R16 | busyspin R16 |
|---|---|---|---|---|---|---|---|
| 1 | 18.1M | 22.9M | 30.7M | 1.27x | 1.70x | 126.2M | 167.0M |
| 2 | 17.7M | 19.5M | 22.4M | 1.10x | 1.27x | 152.4M | 109.8M |
| 4 | 16.9M | 27.3M | 33.3M | 1.62x | 1.98x | 194.4M | 204.0M |
| 6 | 16.2M | 26.6M | 38.7M | 1.64x | 2.38x | 203.4M | 205.7M |

**Flat out, msg=raw: received msg/s (median).**

| P | channel | parking | busyspin | parking/channel | busyspin/channel | parking R16 | busyspin R16 |
|---|---|---|---|---|---|---|---|
| 1 | 10.7M | 17.2M | 25.6M | 1.62x | 2.41x | 41.5M | 42.0M |
| 2 | 10.3M | 15.8M | 18.4M | 1.54x | 1.80x | 41.7M | 40.9M |
| 4 | 9.5M | 23.5M | 28.7M | 2.48x | 3.04x | 79.6M | 79.8M |
| 6 | 9.3M | 24.8M | 33.6M | 2.67x | 3.62x | 90.5M | 83.9M |


**Paced, msg=int64: every row (median of the runs; latency from the scheduled time).**

| P | rate | contestant | achieved | kept | cores | consumer | runtime | cores per 1M/s | p50 | p99 | p999 | most behind |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 5M | channel | 5.0M | 3/3 | 1.26 | 0.19 | 0.05 | 0.253 | 33.3 us | 65.3 us | 1.27 ms | 1680 |
| 1 | 5M | parking | 5.0M | 3/3 | 2.00 | 0.99 | 0.00 | 0.400 | 243 ns | 6.0 us | 247.8 us | 1379 |
| 1 | 5M | busyspin | 5.0M | 3/3 | 2.00 | 0.99 | 0.00 | 0.400 | 249 ns | 2.5 us | 45.6 us | 1298 |
| 2 | 5M | channel | 5.0M | 3/3 | 2.19 | 0.22 | 0.07 | 0.439 | 36.6 us | 72.2 us | 109.6 us | 671 |
| 2 | 5M | parking | 5.0M | 3/3 | 3.00 | 0.99 | 0.00 | 0.600 | 287 ns | 471 ns | 28.4 us | 815 |
| 2 | 5M | busyspin | 5.0M | 3/3 | 3.00 | 1.00 | 0.00 | 0.600 | 311 ns | 547 ns | 21.1 us | 793 |
| 4 | 5M | channel | 5.0M | 3/3 | 3.63 | 0.34 | 0.03 | 0.726 | 44.8 us | 143.4 us | 1.82 ms | 2438 |
| 4 | 5M | parking | 5.0M | 3/3 | 5.00 | 1.00 | 0.00 | 0.999 | 293 ns | 465 ns | 12.9 us | 372 |
| 4 | 5M | busyspin | 5.0M | 3/3 | 5.00 | 0.99 | 0.00 | 1.001 | 291 ns | 469 ns | 6.0 us | 342 |
| 6 | 5M | channel | 5.0M | 3/3 | 4.83 | 0.37 | 0.03 | 0.966 | 57.9 us | 3.26 ms | 3.29 ms | 2766 |
| 6 | 5M | parking | 5.0M | 3/3 | 7.00 | 0.99 | 0.00 | 1.400 | 299 ns | 473 ns | 6.7 us | 1327 |
| 6 | 5M | busyspin | 5.0M | 3/3 | 6.98 | 0.99 | 0.00 | 1.396 | 295 ns | 159.7 us | 7.21 ms | 6920 |

**Paced, msg=raw: every row (median of the runs; latency from the scheduled time).**

| P | rate | contestant | achieved | kept | cores | consumer | runtime | cores per 1M/s | p50 | p99 | p999 | most behind |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 5M | channel | 5.0M | 3/3 | 1.45 | 0.38 | 0.04 | 0.289 | 35.1 us | 69.6 us | 161.8 us | 1621 |
| 1 | 5M | parking | 5.0M | 3/3 | 2.00 | 0.99 | 0.00 | 0.400 | 271 ns | 6.8 us | 102.4 us | 1196 |
| 1 | 5M | busyspin | 5.0M | 3/3 | 2.00 | 0.99 | 0.00 | 0.400 | 271 ns | 5.7 us | 64.8 us | 1625 |
| 2 | 5M | channel | 5.0M | 3/3 | 2.28 | 0.47 | 0.03 | 0.456 | 41.5 us | 87.5 us | 261.1 us | 880 |
| 2 | 5M | parking | 5.0M | 3/3 | 3.00 | 0.99 | 0.00 | 0.600 | 337 ns | 1.9 us | 142.3 us | 744 |
| 2 | 5M | busyspin | 5.0M | 3/3 | 3.00 | 1.00 | 0.00 | 0.600 | 337 ns | 575 ns | 25.5 us | 729 |
| 4 | 5M | channel | 5.0M | 3/3 | 3.25 | 0.67 | 0.01 | 0.650 | 123.9 us | 3.29 ms | 6.26 ms | 8522 |
| 4 | 5M | parking | 5.0M | 3/3 | 5.00 | 0.99 | 0.00 | 1.000 | 317 ns | 527 ns | 3.2 us | 365 |
| 4 | 5M | busyspin | 5.0M | 3/3 | 5.00 | 1.00 | 0.00 | 1.000 | 311 ns | 551 ns | 5.8 us | 357 |
| 6 | 5M | channel | 5.0M | 3/3 | 3.10 | 0.54 | 0.01 | 0.621 | 119.3 us | 3.29 ms | 5.05 ms | 4699 |
| 6 | 5M | parking | 5.0M | 3/3 | 7.00 | 0.99 | 0.00 | 1.399 | 339 ns | 563 ns | 5.6 us | 233 |
| 6 | 5M | busyspin | 5.0M | 3/3 | 6.99 | 0.99 | 0.00 | 1.398 | 337 ns | 587 ns | 2.26 ms | 6391 |

Open: why two producers on the shared sequencer are slower than one, and than four; the channel's paced latency with a
producer that sleeps between sends instead of spinning; rates below 1M msg/s, where Parking parks.

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
