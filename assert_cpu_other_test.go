//go:build !unix

package disruptor

import "time"

// processCPUTime is unavailable without getrusage; BenchmarkIdleCPU skips.
func processCPUTime() (time.Duration, bool) { return 0, false }
