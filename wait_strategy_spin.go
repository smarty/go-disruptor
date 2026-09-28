package disruptor

// BusySpinWaitStrategy never sleeps or yields: every wait returns after a single CPU spin hint (PAUSE on amd64, YIELD
// on arm64), so the caller re-reads its barrier immediately. It gives the lowest and most consistent wake-up latency
// (sub-microsecond after any idle period, where the default strategy's sub-millisecond sleeps can take a full
// millisecond once the runtime parks its threads), at the cost of each waiting goroutine burning a whole CPU for as
// long as it waits, including while the application is idle.
//
// Use it only when every goroutine that can wait (each Listener, plus producers that may find the ring full) has a
// CPU to itself: GOMAXPROCS must exceed the number of spinning goroutines by enough to run everything else, and the
// OS threads should be pinned to dedicated cores. When a spinning goroutine shares a CPU with the goroutine it waits
// for, progress depends on the runtime preempting the spinner (roughly every 10ms), which is catastrophic for latency.
// This is the equivalent of Java LMAX's BusySpinWaitStrategy.
type BusySpinWaitStrategy struct{}

func (this BusySpinWaitStrategy) Gate(int64)    { spinHint() }
func (this BusySpinWaitStrategy) Idle(int64)    { spinHint() }
func (this BusySpinWaitStrategy) Reserve(int64) { spinHint() }
