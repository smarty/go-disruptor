package prefetch

import (
	"math/rand/v2"
	"testing"
	"unsafe"
)

// BenchmarkPairChase walks 128B-aligned pairs of cache lines in random order across 256MB, far beyond any L3. Each step
// loads the first line of a pair (always a DRAM miss) and then, dependent on its value, the second line. When a
// prefetcher fetched the second line alongside the first, a step costs about one DRAM round trip; when none did, about
// two. OneLoad is the one-trip baseline for the machine at hand. Measured on an i7-12700K: PairChase ~97ns with the
// prefetchers on and ~157ns with MSR 0x1a4 = 0x23 (turbo off, reserved cores); OneLoad ~83ns (turbo on).
func BenchmarkPairChase(b *testing.B) {
	b.Run("OneLoad", func(b *testing.B) { benchmarkChase(b, false) })
	b.Run("PairChase", func(b *testing.B) { benchmarkChase(b, true) })
}
func benchmarkChase(b *testing.B, secondLoad bool) {
	const pairCount = 1 << 21 // 2M pairs * 128B = 256MB
	const wordsPerPair = 16   // word 0 is in the first line, word 8 in the second
	backing := make([]uint64, pairCount*wordsPerPair+wordsPerPair)
	offset := (128 - uintptr(unsafe.Pointer(&backing[0]))%128) % 128 / 8
	pairs := backing[offset : offset+pairCount*wordsPerPair]

	order := rand.Perm(pairCount)
	for i, pair := range order {
		next := uint64(order[(i+1)%pairCount])
		if secondLoad {
			pairs[pair*wordsPerPair] = 8 // the offset of the second load
			pairs[pair*wordsPerPair+8] = next
		} else {
			pairs[pair*wordsPerPair] = next
		}
	}

	b.ResetTimer()
	current := uint64(order[0])
	for i := 0; i < b.N; i++ {
		base := current * wordsPerPair
		if secondLoad {
			current = pairs[base+pairs[base]] // the second line's address depends on the first line's value
		} else {
			current = pairs[base]
		}
	}
	if current == 1<<63 {
		b.Log(current) // keeps the walk from being optimized away
	}
}
