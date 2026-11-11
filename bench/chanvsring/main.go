// Command chanvsring compares a Go buffered channel with go-disruptor for the hand-off alone: P producers write a
// message into the channel or a ring slot, and one consumer reads every field of it. The channel and the ring both
// hold 16,384 messages; go-disruptor runs with ParkingWaitStrategy (as vpc-auditor's ring B) or BusySpinWaitStrategy,
// the single-writer sequencer for one producer and the shared one for more.
//
// One run prints one line in Go's benchmark format (benchstat reads it). Flat out (-rate 0), the producers send as
// fast as they can; paced, they send -rate messages per second in total on an absolute schedule, and latency counts
// from each message's scheduled time, so a stalled hand-off cannot hide its delay (coordinated omission).
//
//	chanvsring -impl parking -msg raw -producers 4 -rate 10000000 -warmup 1s -duration 3s
//	chanvsring summarize results.txt   # the median of every metric per row
//
// Pin the process with taskset to one thread per physical core. GOMAXPROCS is Go's default: the CPUs taskset allows.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "summarize" {
		os.Exit(summarizeFiles(os.Args[2:]))
	}
	var config config
	flag.StringVar(&config.Implementation, "impl", "channel", "channel, parking or busyspin")
	flag.StringVar(&config.Message, "msg", "int64", "int64, or raw (224 bytes, the layout of vpc-auditor's contracts.Raw)")
	flag.IntVar(&config.Producers, "producers", 1, "producer goroutines")
	flag.IntVar(&config.Batch, "batch", 1, "slots per Reserve (go-disruptor flat out only: 1 or 16)")
	flag.Int64Var(&config.Rate, "rate", 0, "messages per second across all producers; 0 is flat out")
	flag.DurationVar(&config.Warmup, "warmup", 1e9, "time before the measured window")
	flag.DurationVar(&config.Duration, "duration", 3e9, "the measured window")
	flag.Parse()

	result, err := run(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chanvsring: %s: %v\n", config.name(), err)
		os.Exit(1)
	}
	var sent int64
	for _, each := range result.Sent {
		sent += each
	}
	if result.Errors != 0 || result.Received != sent || result.Checksum != result.expectedChecksum() {
		fmt.Fprintf(os.Stderr, "chanvsring: %s: received %d of %d, %d sequence errors, checksum %d, want %d\n",
			config.name(), result.Received, sent, result.Errors, result.Checksum, result.expectedChecksum())
		os.Exit(1)
	}
	fmt.Println(result.line())
}

func summarizeFiles(paths []string) int {
	readers := []io.Reader{os.Stdin}
	if len(paths) > 0 {
		readers = nil
		for _, path := range paths {
			file, err := os.Open(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "chanvsring:", err)
				return 1
			}
			defer file.Close()
			readers = append(readers, file)
		}
	}
	rows, err := summarize(io.MultiReader(readers...))
	if err != nil {
		fmt.Fprintln(os.Stderr, "chanvsring:", err)
		return 1
	}
	printSummary(os.Stdout, rows)
	return 0
}
