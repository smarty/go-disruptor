//go:build !amd64 && !arm64

package disruptor

// spinHint does nothing on architectures without an assembly spin-wait hint.
func spinHint() {}
