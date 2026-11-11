package main

import (
	"strings"
	"testing"
)

func TestSummarize_MediansPerRowAndKeptUpCounts(t *testing.T) {
	input := strings.Join([]string{
		"goos: linux",
		"BenchmarkChanVsRing/mode=paced/rate=1M/msg=int64/P=1/impl=channel/batch=1\t1\t1000 ns/op\t1000000 msgs/s\t3 p99-ns\t1 kept",
		"BenchmarkChanVsRing/mode=paced/rate=1M/msg=int64/P=1/impl=channel/batch=1\t1\t1000 ns/op\t990000 msgs/s\t1 p99-ns\t0 kept",
		"BenchmarkChanVsRing/mode=paced/rate=1M/msg=int64/P=1/impl=channel/batch=1\t1\t1000 ns/op\t995000 msgs/s\t2 p99-ns\t1 kept",
		"BenchmarkChanVsRing/mode=flat/rate=max/msg=int64/P=1/impl=busyspin/batch=1\t1\t10 ns/op\t100 msgs/s\t5 p99-ns\t0 kept",
		"BenchmarkChanVsRing/mode=flat/rate=max/msg=int64/P=1/impl=busyspin/batch=1\t1\t10 ns/op\t300 msgs/s\t7 p99-ns\t0 kept",
		"chanvsring: some log line",
	}, "\n")

	rows, err := summarize(strings.NewReader(input))

	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	paced, flat := rows[0], rows[1]
	if paced.Name != "mode=paced/rate=1M/msg=int64/P=1/impl=channel/batch=1" || paced.Runs != 3 ||
		paced.Median["msgs/s"] != 995_000 || paced.Median["p99-ns"] != 2 || paced.Kept != 2 {
		t.Errorf("paced row = %+v", paced)
	}
	if flat.Runs != 2 || flat.Median["msgs/s"] != 200 || flat.Median["p99-ns"] != 6 || flat.Kept != 0 {
		t.Errorf("flat row = %+v", flat)
	}
}

func TestSummarize_RejectsAMalformedResultLine(t *testing.T) {
	if _, err := summarize(strings.NewReader("BenchmarkChanVsRing/x\t1\tfast ns/op")); err == nil {
		t.Error("no error for a value that is not a number")
	}
}
