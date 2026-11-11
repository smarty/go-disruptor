package main

// schedule is one producer's share of a paced run, on an absolute schedule like vpc-auditor's cmd/ringload: the
// producers' messages interleave evenly into one stream of `rate` messages per second, and message k of that stream
// (k = sequence*producers + producer) is due k+1 periods after the start. A producer that falls behind sends what is due
// at once; it never skips a message.
type schedule struct {
	rate      int64 // messages per second, across all producers
	producers int64
	producer  int64 // 0 to producers-1
}

// due returns how many of this producer's messages are due `elapsed` nanoseconds after the start.
func (this schedule) due(elapsed int64) int64 {
	total := elapsed * this.rate / nanosPerSecond // messages due across all producers
	if total <= this.producer {
		return 0
	}
	return (total - this.producer + this.producers - 1) / this.producers
}

// at returns when this producer's message `sequence` is due, in nanoseconds after the start: its scheduled send time.
func (this schedule) at(sequence int64) int64 {
	stream := sequence*this.producers + this.producer + 1
	return (stream*nanosPerSecond + this.rate - 1) / this.rate
}

const nanosPerSecond = 1_000_000_000
