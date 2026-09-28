//go:build !amd64 || race

package disruptor

import "sync/atomic"

// storeRelease stores value with (at least) release semantics. Outside amd64, and in every race build, it is
// sync/atomic's sequentially consistent Store, which the race detector instruments as a release.
func storeRelease(address *atomic.Int64, value int64) { address.Store(value) }
