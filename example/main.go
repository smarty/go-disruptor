package main

import (
	"fmt"

	"github.com/smarty/go-disruptor"
)

func main() {
	myDisruptor, _ := disruptor.New(
		disruptor.Options.BufferCapacity(bufferSize),
		disruptor.Options.NewHandlerGroup(simpleHandler{}))
	defer func() { _ = myDisruptor.Close() }()

	go publish(myDisruptor)

	myDisruptor.Listen()
}

func publish(myDisruptor disruptor.Disruptor) {
	defer func() { _ = myDisruptor.Close() }()

	for sequence := int64(0); sequence <= iterations; {
		sequence = myDisruptor.Reserve(reservations)

		for lower := sequence - reservations + 1; lower <= sequence; lower++ {
			ringBuffer[lower&bufferMask] = lower
		}

		myDisruptor.Commit(sequence-reservations+1, sequence)
	}
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

type simpleHandler struct{}

func (this simpleHandler) Handle(lower, upper int64) {
	for ; lower <= upper; lower++ {
		message := ringBuffer[lower&bufferMask]
		if message != lower {
			panic(fmt.Errorf("race condition: %d %d", message, lower))
		}
	}
}

const (
	bufferSize   = 1024 * 64
	bufferMask   = bufferSize - 1
	iterations   = 128 * 1024 * 32
	reservations = 1
)

var ringBuffer = [bufferSize]int64{}
