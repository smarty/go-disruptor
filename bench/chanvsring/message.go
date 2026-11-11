package main

// raw has the layout of vpc-auditor's contracts.Raw (internal/contracts/raw.go), the message in the Auditor's ring B:
// the same field types in the same order, 224 bytes on 64-bit platforms. The fields are copied, not imported, so this
// tool does not depend on vpc-auditor; time.Time appears as its own layout (stamp).
type raw struct {
	Lines       []byte
	Owner       owner
	RequestID   string
	Host, Boot  string
	Sequence    uint64 // here: producer<<56 | the producer's sequence
	Deployment  uint16
	Kind        string
	Status      string
	Class       uint8
	Termination string
	Received    stamp
	ReadAt      stamp
	RingBytes   int64 // here: the send time of a sampled message, else -1
	Truncated   uint16
}

type owner interface {
	Retain()
	Release()
}

type stamp struct { // the layout of time.Time
	wall uint64
	ext  int64
	loc  *byte
}

// A message is sampled for latency when its sequence is a multiple of sampleStride: the producer stores its send time
// (flat out: the clock; paced: the scheduled time) and the consumer reads the clock when it receives it. Reading the
// clock for every message would cost more than the hand-off itself.
const (
	sampleStride = 64
	sampleMask   = sampleStride - 1
)

// An int64 message is producer<<60 | sampledBit | payload, where the payload is the producer's sequence, or the send
// time when the message is sampled.
const (
	maxProducers = 15
	producerBits = 60
	sampledBit   = 1 << 59
	payloadMask  = sampledBit - 1
)

var (
	sharedLines = []byte("<134>1 2026-11-11T00:00:00Z host haproxy - - - USE ...")
	sharedText  = "01234567-0000-7000-8000-000000000000"
)

// put writes message `sequence` of `producer` into slot. The stamp is used only when the message is sampled.
func put[T any](slot *T, producer int, sequence, sent int64) {
	switch message := any(slot).(type) {
	case *int64:
		*message = word(producer, sequence, sent)
	case *raw:
		*message = makeRaw(producer, sequence, sent)
	}
}

func word(producer int, sequence, sent int64) int64 {
	if sequence&sampleMask == 0 {
		return int64(uint64(producer)<<producerBits | sampledBit | uint64(sent)&payloadMask)
	}
	return int64(uint64(producer)<<producerBits | uint64(sequence)&payloadMask)
}

func makeRaw(producer int, sequence, sent int64) raw {
	if sequence&sampleMask != 0 {
		sent = -1
	}
	return raw{Lines: sharedLines, RequestID: sharedText, Host: sharedText[:8], Boot: sharedText[:13],
		Sequence: uint64(producer)<<56 | uint64(sequence), Deployment: uint16(sequence), Kind: "done",
		Status: "200", Class: uint8(sequence), Termination: "----", Received: stamp{wall: uint64(sequence)},
		ReadAt: stamp{ext: sequence}, RingBytes: sent, Truncated: uint16(sequence >> 16)}
}

// take reads every field of the message in slot and checks that it is the next message of its producer.
func take[T any](slot *T, consumer *consumer) {
	switch message := any(slot).(type) {
	case *int64:
		consumer.word(*message)
	case *raw:
		consumer.raw(message)
	}
}

type window struct{ start, end int64 } // nanoseconds on the run's clock

// consumer is the receiving side's state: the next sequence expected from each producer, the counts, a checksum of
// every field, and the latency of sampled messages received inside the window.
type consumer struct {
	next     []int64
	clock    func() int64
	window   window
	received int64
	errors   int64 // messages that were not the next one of their producer
	checksum uint64
	latency  histogram

	started, ended        bool
	startCount, startTime int64 // received before the first sample at or after window.start, and its time
	endCount, endTime     int64 // received before the first sample at or after window.end, and its time
}

func newConsumer(producers int, window window, clock func() int64) *consumer {
	return &consumer{next: make([]int64, producers), clock: clock, window: window}
}

func (this *consumer) word(message int64) {
	producer, payload := int(uint64(message)>>producerBits), int64(uint64(message)&payloadMask)
	if producer >= len(this.next) {
		this.errors++
		return
	}
	if uint64(message)&sampledBit != 0 {
		if this.next[producer]&sampleMask != 0 {
			this.errors++
		}
		this.sample(payload)
	} else {
		if payload != this.next[producer] {
			this.errors++
		}
		this.checksum += uint64(payload)
	}
	this.next[producer]++
	this.received++
}

func (this *consumer) raw(message *raw) {
	producer, sequence := int(message.Sequence>>56), int64(message.Sequence<<8>>8)
	if producer >= len(this.next) {
		this.errors++
		return
	}
	if sequence != this.next[producer] {
		this.errors++
	}
	this.checksum += fold(message)
	if message.RingBytes >= 0 {
		this.sample(message.RingBytes)
	}
	this.next[producer]++
	this.received++
}

// fold sums every field except RingBytes (the send time, which only the producer knows).
func fold(message *raw) uint64 {
	sum := uint64(len(message.Lines)+len(message.RequestID)+len(message.Host)+len(message.Boot)+len(message.Kind)+
		len(message.Status)+len(message.Termination)) + message.Sequence + uint64(message.Deployment) +
		uint64(message.Class) + uint64(message.Truncated) + message.Received.wall + uint64(message.Received.ext) +
		message.ReadAt.wall + uint64(message.ReadAt.ext)
	if message.Owner != nil || message.Received.loc != nil || message.ReadAt.loc != nil {
		sum++
	}
	return sum
}

func (this *consumer) sample(sent int64) {
	now := this.clock()
	if !this.started && now >= this.window.start {
		this.started, this.startCount, this.startTime = true, this.received, now
	}
	if now >= this.window.end {
		if !this.ended {
			this.ended, this.endCount, this.endTime = true, this.received, now
		}
		return
	}
	if this.started {
		this.latency.record(now - sent)
	}
}

// expectedChecksum is the checksum of every message the producers sent: sent[p] messages from producer p.
func expectedChecksum[T any](sent []int64) uint64 {
	var sum uint64
	var zero T
	switch any(zero).(type) {
	case int64:
		for _, count := range sent {
			multiples := (count + sampleStride - 1) / sampleStride // sequences 0, 64, ... carry a stamp instead
			sum += uint64(count*(count-1)/2) - uint64(sampleStride*multiples*(multiples-1)/2)
		}
	case raw:
		for producer, count := range sent {
			for sequence := range count {
				message := makeRaw(producer, sequence, 0)
				sum += fold(&message)
			}
		}
	}
	return sum
}
