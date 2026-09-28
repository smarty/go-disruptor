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

## 6. Release stores in `Commit`: the largest measured opportunity

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

How to ship it (full analysis in `TODO.md`, "Release stores for `Commit` via an amd64 assembly stub"):

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
| 1 | Release-store `Commit` (amd64 stub) | Upper bound -44-49% single writer    | Small    | Next          |
| 2 | Capacity / full-buffer slow path    | 1K 23x slower (R16); now required    | Low      | Done          |
| 3 | Spinning `WaitStrategy` (opt-in)    | Nothing measured yet                 | Moderate | After 2       |
| 4 | Writer batch-boundary false sharing | MP SC/R4 15-18% faster, prefetch off | Low      | Counters next |
| 5 | Multi-producer reserve-1            | `reservedSequence.Add` contention    | High     | Document only |
| 6 | arm64 release store (`STLR` stub)   | Prototype: 3-5% on Apple M5          | Small    | After 1       |

1. **Release-store `Commit`.** Implement the stub, then interleave base vs stub vs the unsafe plain store to see how
   much of the upper bound survives the call.
2. **Capacity and the slow path.** Done: the cost was the consumer's `Idle` sleep, not the producer's (section 5a), and
   `BufferCapacity` is now required, 64K recommended. The same data motivates pathway 3: a shorter consumer wake shrinks
   the capacity needed.
3. **Spinning wait strategy.** Busy-spin with `PAUSE` on dedicated cores, like Java's `BusySpinWaitStrategy`; measure
   throughput and tail latency (the benchmarks measure only throughput today).
4. **Batch-boundary false sharing.** Count store misses in RingBuffer MP SC/R4 with prefetchers on vs off, attributed to
   the writer's entry store; vary batch size and alignment. Likely user guidance rather than a library change.
5. **Multi-producer reserve-1.** Per-writer chunked claims would let an unused claimed slot block every consumer. The
   practical lever is batching (R4 is ~10x faster); document it.
6. **arm64.** Same stub pattern with `STLR`; testable on the macOS laptop.

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
