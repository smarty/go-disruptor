// Package prefetch holds a positive control for experiments that switch hardware prefetchers off (see
// bench/msr-prefetch): a benchmark whose cost roughly doubles when the prefetcher that fetches neighbouring cache lines
// is disabled. Run it alongside the benchmarks under test, so a flat result there can be trusted to mean "no effect"
// rather than "the switch did not take".
package prefetch
