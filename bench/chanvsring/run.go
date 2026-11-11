package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/pprof"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/smarty/go-disruptor"
)

// capacity is the size of every contestant: the channel's buffer and the ring.
const capacity = 16_384

var (
	implementations = []string{"channel", "parking", "busyspin"}
	messages        = []string{"int64", "raw"}
)

// batchesFor lists the Reserve sizes measured: only go-disruptor batches, and only flat out.
func batchesFor(implementation string, rate int64) []int {
	if implementation == "channel" || rate > 0 {
		return []int{1}
	}
	return []int{1, 16}
}

type config struct {
	Implementation string // channel, parking (ParkingWaitStrategy) or busyspin (BusySpinWaitStrategy)
	Message        string // int64 or raw
	Producers      int
	Batch          int   // slots per Reserve; 1 except for go-disruptor flat out
	Rate           int64 // messages per second across all producers; 0 is flat out
	Warmup         time.Duration
	Duration       time.Duration
}

func (this config) validate() error {
	switch {
	case !slices.Contains(implementations, this.Implementation):
		return fmt.Errorf("unknown implementation %q (want one of %v)", this.Implementation, implementations)
	case !slices.Contains(messages, this.Message):
		return fmt.Errorf("unknown message %q (want one of %v)", this.Message, messages)
	case this.Producers < 1 || this.Producers > maxProducers:
		return fmt.Errorf("producers must be 1 to %d", maxProducers)
	case !slices.Contains(batchesFor(this.Implementation, this.Rate), this.Batch):
		return fmt.Errorf("batch %d is not measured for %s at rate %d", this.Batch, this.Implementation, this.Rate)
	case this.Rate < 0 || this.Warmup < 0 || this.Duration <= 0:
		return errors.New("the rate and warm-up must not be negative, and the duration must be above 0")
	}
	return nil
}

func (this config) name() string {
	mode, rate := "flat", "max"
	if this.Rate > 0 {
		mode, rate = "paced", rateLabel(this.Rate)
	}
	return fmt.Sprintf("mode=%s/rate=%s/msg=%s/P=%d/impl=%s/batch=%d", mode, rate, this.Message, this.Producers,
		this.Implementation, this.Batch)
}

func rateLabel(rate int64) string {
	switch {
	case rate%1_000_000 == 0:
		return fmt.Sprintf("%dM", rate/1_000_000)
	case rate%1_000 == 0:
		return fmt.Sprintf("%dk", rate/1_000)
	default:
		return fmt.Sprint(rate)
	}
}

// result times are nanoseconds on the run's clock.
type result struct {
	config
	Sent                  []int64 // per producer, the whole run
	Received, Errors      int64
	Checksum              uint64
	StartCount, StartTime int64 // the window, as the consumer saw it
	EndCount, EndTime     int64
	Latency               histogram // from the send time (flat out) or the scheduled time (paced)
	Behind                int64     // the most messages one producer was behind its schedule inside the window
	Backlog               int64     // messages due but not yet received at the end of the window
	Cores                 float64   // the process: user and system time over the window (getrusage)
	ProducerCores         float64   // the CPU profile's samples labelled role=producer
	ConsumerCores         float64   // ... role=consumer
	RuntimeCores          float64   // ... without a label: the scheduler, GC and other runtime threads
}

// rate is the received rate inside the window, in messages per second.
func (this result) rate() float64 {
	if this.EndTime <= this.StartTime {
		return 0
	}
	return float64(this.EndCount-this.StartCount) * nanosPerSecond / float64(this.EndTime-this.StartTime)
}

// keptUp holds for a paced run that received at least 99% of the offered rate and ended with no more than one ring of
// messages due but not received.
func (this result) keptUp() bool {
	return this.Rate > 0 && this.rate() >= 0.99*float64(this.Rate) && this.Backlog <= capacity
}

func (this result) expectedChecksum() uint64 {
	if this.Message == "raw" {
		return expectedChecksum[raw](this.Sent)
	}
	return expectedChecksum[int64](this.Sent)
}

// line is one run in Go's benchmark format, so that benchstat can read it.
func (this result) line() string {
	rate := this.rate()
	kept := 0
	if this.keptUp() {
		kept = 1
	}
	return fmt.Sprintf("BenchmarkChanVsRing/%s\t1\t%.1f ns/op\t%.0f msgs/s\t%.3f cores\t%.3f producer-cores\t"+
		"%.4f consumer-cores\t%.4f runtime-cores\t%.4f cores-per-Mmsg\t%d p50-ns\t%d p99-ns\t%d p999-ns\t"+
		"%d behind-msgs\t%d backlog-msgs\t%d kept", this.name(), nanosPerSecond/rate, rate, this.Cores,
		this.ProducerCores, this.ConsumerCores, this.RuntimeCores, this.Cores*1e6/rate, this.Latency.percentile(0.5),
		this.Latency.percentile(0.99), this.Latency.percentile(0.999), this.Behind, this.Backlog, kept)
}

func run(config config) (result, error) {
	if err := config.validate(); err != nil {
		return result{}, err
	}
	if config.Message == "raw" {
		return runWith[raw](config)
	}
	return runWith[int64](config)
}

// Timeline of a run, from the run's clock zero: the producers start together at origin, the window opens after the
// warm-up and closes after the duration, and the producers stop a tail later, so that the consumer sees a sampled
// message after the window closes.
const (
	origin = 5 * time.Millisecond
	tail   = 10 * time.Millisecond
)

func runWith[T any](config config) (result, error) {
	base := time.Now()
	clock := func() int64 { return int64(time.Since(base)) }
	windowStart := int64(origin + config.Warmup)
	windowEnd := windowStart + int64(config.Duration)
	consumer := newConsumer(config.Producers, window{start: windowStart, end: windowEnd}, clock)
	producers := make([]*producer, config.Producers)
	for index := range producers {
		producers[index] = &producer{id: index, base: base, rate: config.Rate, batch: config.Batch,
			schedule: schedule{rate: config.Rate, producers: int64(config.Producers), producer: int64(index)},
			window:   window{start: windowStart, end: windowEnd}, stopAt: windowEnd + int64(tail)}
	}

	var consumers, senders sync.WaitGroup
	var closeTransport func()
	switch config.Implementation {
	case "channel":
		channel := make(chan T, capacity)
		consumers.Go(func() {
			labelled("consumer", func() {
				for message := range channel {
					take(&message, consumer)
				}
			})
		})
		for _, each := range producers {
			senders.Go(func() { labelled("producer", func() { produceChannel(each, channel) }) })
		}
		closeTransport = func() { close(channel) }
	default:
		ring := make([]T, capacity)
		instance, err := disruptor.New(
			disruptor.Options.BufferCapacity(capacity),
			disruptor.Options.WriterCount(uint8(config.Producers)), // more than 1: the shared sequencer, as in ring B
			disruptor.Options.WaitStrategy(waitStrategy(config.Implementation)),
			disruptor.Options.NewHandlerGroup(&ringHandler[T]{ring: ring, consumer: consumer}))
		if err != nil {
			return result{}, err
		}
		consumers.Go(func() { labelled("consumer", instance.Listen) })
		for _, each := range producers {
			senders.Go(func() { labelled("producer", func() { produceRing(each, instance, ring) }) })
		}
		closeTransport = func() { _ = instance.Close() }
	}

	measured, err := measure(base, windowStart, windowEnd)
	senders.Wait()
	closeTransport()
	consumers.Wait()
	if err != nil {
		return result{}, err
	}

	outcome := result{config: config, Received: consumer.received, Errors: consumer.errors,
		Checksum: consumer.checksum, StartCount: consumer.startCount, StartTime: consumer.startTime,
		EndCount: consumer.endCount, EndTime: consumer.endTime, Latency: consumer.latency, Cores: measured.cores,
		ProducerCores: measured.roles["producer"], ConsumerCores: measured.roles["consumer"],
		RuntimeCores: measured.roles[""]}
	for _, each := range producers {
		outcome.Sent = append(outcome.Sent, each.sent)
		outcome.Behind = max(outcome.Behind, each.behind)
		if config.Rate > 0 {
			outcome.Backlog += each.schedule.due(consumer.endTime - int64(origin))
		}
	}
	if config.Rate > 0 {
		outcome.Backlog -= consumer.endCount
	}
	if !consumer.started || !consumer.ended {
		return outcome, errors.New("the consumer saw no sampled message after the window opened or after it closed")
	}
	return outcome, nil
}

func waitStrategy(implementation string) disruptor.WaitStrategy {
	if implementation == "busyspin" {
		return disruptor.BusySpinWaitStrategy{}
	}
	return disruptor.ParkingWaitStrategy{} // the defaults, as the Auditor's ring B uses it
}

func labelled(role string, work func()) {
	pprof.Do(context.Background(), pprof.Labels("role", role), func(context.Context) { work() })
}

type ringHandler[T any] struct {
	ring     []T
	consumer *consumer
}

func (this *ringHandler[T]) Handle(lower, upper int64) {
	for ; lower <= upper; lower++ {
		take(&this.ring[lower&(capacity-1)], this.consumer)
	}
}

// producer is one sending goroutine's state.
type producer struct {
	id       int
	base     time.Time
	rate     int64
	batch    int
	schedule schedule
	window   window
	stopAt   int64
	sent     int64
	behind   int64
}

func (this *producer) now() int64 { return int64(time.Since(this.base)) }

// due waits for origin, then for at least one due message (paced), and returns how many to send: 0 means stop. Flat
// out, it returns a batch at once; the caller checks the clock on sampled messages.
func (this *producer) due() int64 {
	if this.rate == 0 {
		return int64(this.batch)
	}
	for {
		now := this.now()
		if now >= this.stopAt {
			return 0
		}
		if now < int64(origin) {
			continue
		}
		due := this.schedule.due(now - int64(origin))
		if due <= this.sent {
			continue // ahead of the schedule: spin on the clock, so that sends leave at their scheduled time
		}
		if now >= this.window.start && now < this.window.end {
			this.behind = max(this.behind, due-this.sent)
		}
		return min(due-this.sent, 1024)
	}
}

// stamp is the send time of message `sequence` if it is sampled. Flat out it reads the clock, and reports whether the
// run is over; paced it is the scheduled time (no clock read, no division for unsampled messages).
func (this *producer) stamp(sequence int64) (sent int64, stop bool) {
	if sequence&sampleMask != 0 {
		return 0, false
	}
	if this.rate > 0 {
		return int64(origin) + this.schedule.at(sequence), false
	}
	sent = this.now()
	return sent, sent >= this.stopAt
}

func (this *producer) waitForOrigin() {
	for this.now() < int64(origin) {
	}
}

func produceChannel[T any](this *producer, channel chan<- T) {
	this.waitForOrigin()
	for {
		count := this.due()
		if count == 0 {
			return
		}
		for range count {
			sent, stop := this.stamp(this.sent)
			if stop {
				return
			}
			var message T
			put(&message, this.id, this.sent, sent)
			channel <- message
			this.sent++
		}
	}
}

func produceRing[T any](this *producer, sequencer disruptor.Sequencer, ring []T) {
	this.waitForOrigin()
	for {
		count := this.due()
		if count == 0 {
			return
		}
		if this.batch == 1 {
			for range count {
				sent, stop := this.stamp(this.sent)
				if stop {
					return
				}
				upper := sequencer.Reserve(1)
				put(&ring[upper&(capacity-1)], this.id, this.sent, sent)
				sequencer.Commit(upper, upper)
				this.sent++
			}
			continue
		}
		upper := sequencer.Reserve(uint32(count))
		lower := upper - count + 1
		stop := false
		for sequence := lower; sequence <= upper; sequence++ {
			sent, over := this.stamp(this.sent) // past the end, the reserved slots are written and committed all the same
			stop = stop || over
			put(&ring[sequence&(capacity-1)], this.id, this.sent, sent)
			this.sent++
		}
		sequencer.Commit(lower, upper)
		if stop {
			return
		}
	}
}

// measurement is the CPU used inside the window: the whole process from getrusage, and the CPU profile by role.
type measurement struct {
	cores float64
	roles map[string]float64
}

// measure sleeps until the window opens, starts a CPU profile, sleeps until it closes, and returns the CPU used.
func measure(base time.Time, windowStart, windowEnd int64) (measurement, error) {
	time.Sleep(time.Duration(windowStart) - time.Since(base))
	var profile bytes.Buffer
	if err := pprof.StartCPUProfile(&profile); err != nil {
		return measurement{}, err
	}
	startCPU, startWall := processCPU(), time.Now()
	time.Sleep(time.Duration(windowEnd) - time.Since(base))
	endCPU, wall := processCPU(), time.Since(startWall)
	pprof.StopCPUProfile()
	byLabel, err := cpuByLabel(profile.Bytes(), "role")
	if err != nil {
		return measurement{}, err
	}
	outcome := measurement{cores: float64(endCPU-startCPU) / float64(wall), roles: map[string]float64{}}
	for role, used := range byLabel {
		outcome.roles[role] = float64(used) / float64(wall)
	}
	return outcome, nil
}

// processCPU is the user and system time of the whole process so far.
func processCPU() time.Duration {
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
