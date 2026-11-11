package main

import (
	"bytes"
	"context"
	"runtime/pprof"
	"sync"
	"testing"
	"time"
)

func TestCPUByLabel_SplitsAProfileByRole(t *testing.T) {
	skipWhenShort(t)
	var buffer bytes.Buffer
	if err := pprof.StartCPUProfile(&buffer); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	group.Go(func() {
		pprof.Do(context.Background(), pprof.Labels("role", "busy"), func(context.Context) { spin(300 * time.Millisecond) })
	})
	group.Go(func() {
		pprof.Do(context.Background(), pprof.Labels("role", "idle"), func(context.Context) { time.Sleep(300 * time.Millisecond) })
	})
	group.Wait()
	pprof.StopCPUProfile()

	split, err := cpuByLabel(buffer.Bytes(), "role")

	if err != nil {
		t.Fatal(err)
	}
	if busy := split["busy"]; busy < 150*time.Millisecond || busy > 450*time.Millisecond {
		t.Errorf("busy = %s, want about 300ms (split %v)", busy, split)
	}
	if idle := split["idle"]; idle > 30*time.Millisecond {
		t.Errorf("idle = %s, want about 0 (split %v)", idle, split)
	}
}

func TestCPUByLabel_RejectsWhatIsNotAProfile(t *testing.T) {
	if _, err := cpuByLabel([]byte("not a profile"), "role"); err == nil {
		t.Error("no error for bytes that are not a gzipped profile")
	}
}

func spin(duration time.Duration) {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
	}
}
