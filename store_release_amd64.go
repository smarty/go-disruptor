//go:build amd64 && !race

package disruptor

import "sync/atomic"

// storeRelease stores value with release semantics, which is all that publishing committed slots requires: the
// caller's earlier ring-buffer writes become visible before the stored sequence does. It is implemented in assembly
// (store_release_amd64.s). Race builds use sync/atomic instead (store_release_atomic.go), because the race detector
// models synchronization only through instrumented sync/atomic calls and would not see this one.
//
//go:noescape
func storeRelease(address *atomic.Int64, value int64)
