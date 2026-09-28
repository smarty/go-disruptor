//go:build unix

package disruptor

import (
	"syscall"
	"time"
)

// processCPUTime returns the user and system CPU time consumed by the whole process so far.
func processCPUTime() (time.Duration, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), true
}
