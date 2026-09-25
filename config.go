package disruptor

import (
	"errors"
	"reflect"
	"runtime"
	"time"
)

func New(options ...option) (Disruptor, error) {
	config := configuration{}
	Options.apply(options...)(&config)

	if config.BufferCapacity <= 0 {
		return nil, errors.New("buffer capacity must be at least 1")
	} else if config.BufferCapacity&(config.BufferCapacity-1) != 0 {
		return nil, errors.New("the buffer capacity must be a power of two, e.g. 2, 4, 8, 16")
	} else if len(config.HandlerGroups) == 0 {
		return nil, errors.New("no handlers have been provided")
	}

	upperSequence := newSequence()
	if config.WriterCount <= 1 {
		listener, handledBarrier := config.newListeners(newAtomicBarrier(upperSequence))
		sequencer := newSequencer(config.BufferCapacity, upperSequence, handledBarrier, config.WaitStrategy)
		return &defaultDisruptor{ListenCloser: listener, Sequencer: sequencer}, nil
	}

	sequencer := newSharedSequencer(config.BufferCapacity, upperSequence, config.WaitStrategy)
	listener, handledBarrier := config.newListeners(sequencer)
	sequencer.consumerBarrier = handledBarrier // bi-directional dependency
	return &defaultDisruptor{ListenCloser: listener, Sequencer: sequencer}, nil
}
func (this configuration) newListeners(committedBarrier sequenceBarrier) (listener ListenCloser, handledBarrier sequenceBarrier) {
	handledBarrier = committedBarrier

	totalSequences := 0
	for _, handlers := range this.HandlerGroups {
		totalSequences += len(handlers)
	}
	allSequences := newSequences(totalSequences)

	listeners := make([]ListenCloser, len(this.HandlerGroups))
	offset := 0
	for groupIndex, handlers := range this.HandlerGroups {
		sequences := make([]*atomicSequence, len(handlers))
		group := make([]ListenCloser, len(handlers))
		for handlerIndex, handler := range handlers {
			sequences[handlerIndex] = allSequences[offset+handlerIndex]
			group[handlerIndex] = newListener(sequences[handlerIndex], committedBarrier, handledBarrier, this.WaitStrategy, handler)
		}
		handledBarrier = newCompositeBarrier(sequences...) // next group cannot handle beyond the sequences the current group have handled.
		listeners[groupIndex] = newCompositeListener(group)
		offset += len(handlers)
	}

	return newCompositeListener(listeners), handledBarrier
}

// BufferCapacity sets the number of slots in the ring buffer. Must be a power of 2. Default: 1024.
func (singleton) BufferCapacity(value uint32) option {
	return func(this *configuration) { this.BufferCapacity = value }
}

// WriterCount indicates the number of active writers that will be operating on the underlying Sequencer concurrently.
// A value > 1 will configure the Disruptor to utilize a shared, goroutine-safe Sequencer.
func (singleton) WriterCount(value uint8) option {
	return func(this *configuration) { this.WriterCount = value }
}

// WaitStrategy sets the backpressure strategy used by both producers and consumers and has the ability to profoundly
// affect the throughput, latency, and other performance characteristics of the various producers and consumers.
func (singleton) WaitStrategy(value WaitStrategy) option {
	return func(this *configuration) { this.WaitStrategy = value }
}

// NewHandlerGroup defines a set of one or more Handler instances, each of which runs in its own goroutine, and which
// gate together. That is, each group does not allow a subsequent group of Handlers to operate on the underlying ring
// buffer until all Handlers within the current group have completed all operations.
func (singleton) NewHandlerGroup(values ...Handler) option {
	return func(this *configuration) {
		filtered := make([]Handler, 0, len(values))
		for _, value := range values {
			if !isNilHandler(value) {
				filtered = append(filtered, value)
			}
		}

		if len(filtered) > 0 {
			this.HandlerGroups = append(this.HandlerGroups, filtered)
		}
	}
}

// isNilHandler reports whether the Handler is nil, including a typed nil (e.g. a nil *T or nil func stored in the
// interface) which would otherwise pass a plain `value != nil` check and panic later on the listener goroutine.
func isNilHandler(value Handler) bool {
	if value == nil {
		return true
	}

	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Pointer, reflect.Func:
		return reflected.IsNil()
	default:
		return false // e.g. methods on a nil map or nil slice are valid, so those are not treated as nil
	}
}

func (singleton) apply(options ...option) option {
	return func(this *configuration) {
		for _, item := range Options.defaults(options...) {
			item(this)
		}
	}
}
func (singleton) defaults(options ...option) []option {
	return append([]option{
		Options.BufferCapacity(1024),
		Options.WriterCount(1),
		Options.WaitStrategy(defaultWaitStrategy{}),
	}, options...)
}

type configuration struct {
	BufferCapacity uint32
	WriterCount    uint8
	WaitStrategy   WaitStrategy
	HandlerGroups  [][]Handler
}
type singleton struct{}
type option func(*configuration)

var Options singleton

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

type defaultWaitStrategy struct{}

// LockSupport.parkNanos(1L) is more or less equivalent to runtime.Gosched()

func (this defaultWaitStrategy) Gate(int64)    { runtime.Gosched() }
func (this defaultWaitStrategy) Idle(int64)    { time.Sleep(time.Nanosecond * 500) }
func (this defaultWaitStrategy) Reserve(int64) { time.Sleep(time.Nanosecond) }

// //////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
type defaultDisruptor struct {
	ListenCloser
	Sequencer
}
