# Benchmark tooling

Tools for comparing benchmark variants reliably enough to trust deltas of a few percent. The method behind every
Performance Finding in `CLAUDE.md`: build each variant once, reserve the benchmark cores, turn turbo off, interleave the
variants in shuffled order across rounds, and compare with `benchstat`
(`go install golang.org/x/perf/cmd/benchstat@latest`).

| Tool                      | Runs as | Purpose                                                                        |
|---------------------------|---------|--------------------------------------------------------------------------------|
| `suite`                   | you     | Builds, then runs the validate, baseline, and prefetch steps via `reserve`     |
| `compare`                 | you     | A git revision (default `HEAD`) against the working tree, via `reserve`        |
| `reserve`                 | root    | Moves everything else off the benchmark CPUs, disables turbo, restores on exit |
| `interleave`              | either  | Shuffled rounds per variant, then `environment.txt` and `benchstat`            |
| `msr-prefetch`            | root    | Reads/writes Intel's prefetcher MSR (`0x1a4`), with the Golden Cove bit map    |
| `prefetch/` (`PairChase`) | either  | Positive control: proves a prefetcher switch actually took effect              |

## The standard suite

`bench/suite` (run as yourself; it calls `sudo` per step) builds the binaries and runs three steps, writing to
`/tmp/go-disruptor-bench/<UTC timestamp>/<step>/`: `validate` (one round, asserting that reservation works and that
switching the prefetchers off slows `PairChase` by at least 1.3x), `baseline` (10 rounds of the sequencer, capacity,
ring-buffer, wake-latency, busy-spin, and idle-CPU benchmarks, with both wait strategies side by side), and `prefetch`
(the same benchmarks, prefetchers on vs off, with the control). Run one step with `bench/suite baseline`. The CPU layout
defaults to the i7-12700K; any other CPU must pass every CPU option.

Note that `-test.bench` splits its pattern on `/` per sub-benchmark level, except inside parentheses: select two
benchmark families with `(SequencerCapacity|RingBuffer)/SP`, not `SequencerCapacity/SP|RingBuffer/SP`.

## Comparing two variants

`bench/compare` does all of the following for the common case: it builds `--base` (default `HEAD`) in a temporary
worktree and the working tree as the candidate, then runs the core sets for 10 reserved rounds.

```bash
go test -c -o /tmp/base.test .                                # build every variant first; nothing compiles mid-run
git worktree add ../candidate HEAD                            # ...then change the candidate...
(cd ../candidate && go test -c -o /tmp/candidate.test .)

sudo bench/reserve --cpus 0-15 --housekeeping 16-19 -- \
	bench/interleave --rounds 10 --output /tmp/results --benchstat "$(go env GOPATH)/bin/benchstat" \
	--set '0,2,4,6:4:Sequencer/(SP|Reserve|NextWrapPoint|Commit)' \
	--set '0,2,4,6,8,10,12,14:8:SharedSequencer/MP' \
	base=/tmp/base.test candidate=/tmp/candidate.test
```

`sudo` resets `PATH`, so pass `--benchstat` explicitly. Without `reserve`, `interleave` still works as a normal user,
which is fine for a first look; a pinned but unreserved pilot is noisier (±10-50% on single-producer rows).

**Choosing CPUs.** Never let a benchmark land on a hybrid CPU's E-cores. Read the layout from
`lscpu -e=CPU,CORE,MAXMHZ`: P-core threads report the higher `MAXMHZ`, and SMT siblings share a `CORE` value. Reserve
both threads of every benchmark core (`--cpus`), so siblings stay idle, but pin each `--set` to one thread per core. On
the i7-12700K that is `--cpus 0-15 --housekeeping 16-19`, with `0,2,4,6` for single-producer sets and all eight even
CPUs for multi-producer sets (they keep 5-6 goroutines busy).

**Reading results.** Only trust a multi-producer delta that survives a second, independent run. `environment.txt`
records the machine, turbo, prefetcher MSR values (as root on Intel), rounds, sets, and each binary's Go version; keep
it with any number that gets published.

## Prefetcher experiments

Hardware prefetch state changes results (the shared sequencer at reserve-1 is 19-25% slower on the i7-12700K without
it), so record it (`environment.txt` does) and change it only deliberately. A variant can carry a mask, which
`interleave` applies to `--mask-cpus` before each of its runs and restores on exit; this requires running under
`reserve`. Always include the `PairChase` control, built from `bench/prefetch`, as its own set:

```bash
go test -c -o /tmp/control.test ./bench/prefetch
sudo bench/reserve --cpus 0-15 --housekeeping 16-19 -- \
	bench/interleave --rounds 10 --output /tmp/prefetch --mask-cpus 0-15 --benchstat "$(go env GOPATH)/bin/benchstat" \
	--set '2:1:PairChase/PairChase:/tmp/control.test' \
	--set '0,2,4,6:4:SharedSequencer/SP' \
	on=/tmp/base.test off=/tmp/base.test@0x23
```

If `PairChase` does not roughly double with the prefetchers off (~97ns to ~157ns on the i7-12700K), the switch did not
take, and a flat result elsewhere means nothing. On Golden Cove P-cores, bit 1 (Intel's documented adjacent-line
control) has no measurable effect, and bit 5 carries the neighbouring-line fetch; see `msr-prefetch --help` for the
whole measured bit map.

## Attributing costs with `perf`

Count per-op events with `perf stat -x, -e ...` over a fixed `-test.benchtime Nx`, and subtract a `-test.benchtime 1x`
run of the same benchmark to remove setup (the 100ms start-up sleep, scheduler warm-up, and allocation). Useful Golden
Cove events (`cpu_core/<event>/u`): `mem_load_retired.{l1_hit,fb_hit,l2_hit,l3_hit,l3_miss}`,
`mem_load_l3_hit_retired.{xsnp_fwd,xsnp_no_fwd}` (lines taken from another core), `l2_rqsts.rfo_miss` (store misses),
and `l2_rqsts.all_hwpf` (hardware prefetch requests).

For per-function attribution, use precise sampling (`perf record -e cpu_core/<event>/upp`, then
`perf report --sort sym,srcline`). Without `pp`, samples skid onto nearby instructions: imprecise sampling put ~40% of
fill-buffer hits in `sharedSequencer.Reserve`, where precise sampling found 0.2%.
