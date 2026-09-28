//go:build amd64 || arm64

package disruptor

// spinHint executes the CPU's spin-wait hint (spin_hint_amd64.s, spin_hint_arm64.s). It touches no memory, so it is
// the same in race builds.
func spinHint()
