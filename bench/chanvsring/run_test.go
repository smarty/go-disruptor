package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRun_EveryMessageArrivesExactlyOnce(t *testing.T) {
	skipWhenShort(t)
	producers, rates := []int{1, 2, 4}, []int64{0, 400_000}
	for _, implementation := range implementations {
		for _, message := range messages {
			for _, count := range producers {
				for _, rate := range rates {
					for _, batch := range batchesFor(implementation, rate) {
						config := config{Implementation: implementation, Message: message, Producers: count, Batch: batch,
							Rate: rate, Warmup: 5 * time.Millisecond, Duration: 20 * time.Millisecond}
						t.Run(config.name(), func(t *testing.T) { assertExactlyOnce(t, config) })
					}
				}
			}
		}
	}
}

func assertExactlyOnce(t *testing.T, config config) {
	result, err := run(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sent) != config.Producers {
		t.Fatalf("%d producer counts, want %d", len(result.Sent), config.Producers)
	}
	var sent int64
	for _, each := range result.Sent {
		if each == 0 {
			t.Errorf("a producer sent nothing: %v", result.Sent)
		}
		sent += each
	}
	if result.Received != sent || result.Errors != 0 {
		t.Errorf("received %d of %d sent, %d sequence errors", result.Received, sent, result.Errors)
	}
	if want := result.expectedChecksum(); result.Checksum != want {
		t.Errorf("checksum = %d, want %d", result.Checksum, want)
	}
}

func TestRun_LeavesNoGoroutineBehind(t *testing.T) {
	skipWhenShort(t)
	before := runtime.NumGoroutine()
	for _, implementation := range implementations {
		config := config{Implementation: implementation, Message: "int64", Producers: 2, Batch: 1,
			Warmup: time.Millisecond, Duration: 5 * time.Millisecond}
		if result, err := run(config); err != nil || result.Received == 0 {
			t.Fatalf("%s: received %d, error %v", implementation, result.Received, err)
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for runtime.NumGoroutine() != before && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after != before {
			t.Errorf("%s: %d goroutines after the run, %d before", implementation, after, before)
		}
	}
}

func TestRun_PacedMeetsALowRateAndMeasuresTheWindow(t *testing.T) {
	skipWhenShort(t)
	config := config{Implementation: "channel", Message: "int64", Producers: 2, Batch: 1, Rate: 200_000,
		Warmup: 10 * time.Millisecond, Duration: 100 * time.Millisecond}

	result, err := run(config)
	if err != nil {
		t.Fatal(err)
	}

	if achieved := result.rate(); achieved < 180_000 || achieved > 220_000 {
		t.Errorf("achieved %.0f msg/s, want 200,000 within 10%%", achieved)
	}
	if !result.keptUp() {
		t.Errorf("not kept up at 200,000 msg/s: %+v", result)
	}
	if samples := result.Latency.count(); samples < 200 { // 20,000 messages in the window, one in 64 sampled
		t.Errorf("latency samples = %d, want at least 200", samples)
	}
	if window := time.Duration(result.EndTime - result.StartTime); window < 90*time.Millisecond || window > 110*time.Millisecond {
		t.Errorf("window = %s, want about 100ms", window)
	}
}

func TestRun_RejectsShapesItDoesNotMeasure(t *testing.T) {
	for _, config := range []config{
		{Implementation: "channel", Message: "int64", Producers: 1, Batch: 16},
		{Implementation: "parking", Message: "int64", Producers: 1, Batch: 16, Rate: 1_000},
		{Implementation: "queue", Message: "int64", Producers: 1, Batch: 1},
		{Implementation: "channel", Message: "string", Producers: 1, Batch: 1},
		{Implementation: "channel", Message: "int64", Producers: 0, Batch: 1},
		{Implementation: "channel", Message: "int64", Producers: maxProducers + 1, Batch: 1},
	} {
		if _, err := run(config); err == nil {
			t.Errorf("%s: no error", config.name())
		}
	}
}

func TestResult_KeptUpNeedsTheRateAndABoundedBacklog(t *testing.T) {
	base := result{config: config{Rate: 1_000_000}, StartTime: 0, EndTime: 1_000_000_000, StartCount: 0}
	for _, test := range []struct {
		received, backlog int64
		want              bool
	}{
		{1_000_000, 0, true},
		{990_000, capacity, true},
		{989_999, 0, false},
		{1_000_000, capacity + 1, false},
	} {
		result := base
		result.EndCount, result.Backlog = test.received, test.backlog
		if got := result.keptUp(); got != test.want {
			t.Errorf("received %d, backlog %d: kept up = %v, want %v", test.received, test.backlog, got, test.want)
		}
	}
	if flat := (result{EndCount: 5, EndTime: 1}); flat.keptUp() {
		t.Error("a flat-out run reports kept up")
	}
}

func TestResult_LineIsABenchstatLine(t *testing.T) {
	result := result{config: config{Implementation: "parking", Message: "raw", Producers: 4, Batch: 1, Rate: 5_000_000},
		StartTime: 0, EndTime: 2_000_000_000, StartCount: 0, EndCount: 10_000_000, Backlog: 12, Behind: 34,
		Cores: 4.5, ProducerCores: 4, ConsumerCores: 0.25, RuntimeCores: 0.125}
	for value := int64(1); value <= 100; value++ {
		result.Latency.record(value)
	}

	line := result.line()

	want := []string{"BenchmarkChanVsRing/mode=paced/rate=5M/msg=raw/P=4/impl=parking/batch=1", "1",
		"200.0 ns/op", "5000000 msgs/s", "4.500 cores", "4.000 producer-cores", "0.2500 consumer-cores",
		"0.1250 runtime-cores", "0.9000 cores-per-Mmsg", "50 p50-ns", "99 p99-ns", "100 p999-ns",
		"34 behind-msgs", "12 backlog-msgs", "1 kept"}
	if got := strings.Split(line, "\t"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("line =\n%q\nwant\n%q", got, want)
	}
}

// skipWhenShort skips a test that runs the benchmark: each run takes about 200ms (StopCPUProfile waits for the profile
// writer), which does not fit `make test`'s one-second timeout.
func skipWhenShort(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the benchmark; skipped with -short")
	}
}
