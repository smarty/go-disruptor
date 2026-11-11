package main

import (
	"testing"
	"unsafe"
)

func TestRaw_IsTheSizeOfTheAuditorsRingBSlot(t *testing.T) {
	if size := unsafe.Sizeof(raw{}); size != 224 {
		t.Errorf("unsafe.Sizeof(raw{}) = %d, want 224 (vpc-auditor contracts.Raw on 64-bit)", size)
	}
}

func TestConsumer_WordsInOrderAreCountedAndChecked(t *testing.T) {
	consumer := newConsumer(2, window{start: 0, end: 1 << 62}, func() int64 { return 1_000 })
	var word int64
	for sequence := range int64(130) {
		for producer := range 2 {
			put(&word, producer, sequence, 900)
			take(&word, consumer)
		}
	}

	if consumer.received != 260 || consumer.errors != 0 {
		t.Errorf("received, errors = %d, %d, want 260, 0", consumer.received, consumer.errors)
	}
	if want := expectedChecksum[int64]([]int64{130, 130}); consumer.checksum != want {
		t.Errorf("checksum = %d, want %d", consumer.checksum, want)
	}
	// Sequences 0, 64 and 128 of each producer carry the stamp 900; received at 1000, so 100ns each.
	if samples, p50 := consumer.latency.count(), consumer.latency.percentile(0.5); samples != 6 || p50 != 100 {
		t.Errorf("latency samples, p50 = %d, %d, want 6, 100", samples, p50)
	}
}

func TestConsumer_RawsInOrderAreCountedAndChecked(t *testing.T) {
	consumer := newConsumer(3, window{start: 0, end: 1 << 62}, func() int64 { return 1_100 })
	var message raw
	for sequence := range int64(70) {
		for producer := range 3 {
			put(&message, producer, sequence, 1_000)
			take(&message, consumer)
		}
	}

	if consumer.received != 210 || consumer.errors != 0 {
		t.Errorf("received, errors = %d, %d, want 210, 0", consumer.received, consumer.errors)
	}
	if want := expectedChecksum[raw]([]int64{70, 70, 70}); consumer.checksum != want {
		t.Errorf("checksum = %d, want %d", consumer.checksum, want)
	}
	if samples, p50 := consumer.latency.count(), consumer.latency.percentile(0.5); samples != 6 || p50 != 100 {
		t.Errorf("latency samples, p50 = %d, %d, want 6, 100", samples, p50)
	}
}

func TestConsumer_DetectsLossDuplicationAndCorruption(t *testing.T) {
	for _, test := range []struct {
		name      string
		sequences []int64
	}{
		{"lost", []int64{0, 1, 3}},
		{"duplicated", []int64{0, 1, 1}},
		{"reordered", []int64{0, 2, 1}},
	} {
		words, raws := newConsumer(1, window{}, zeroClock), newConsumer(1, window{}, zeroClock)
		var word int64
		var message raw
		for _, sequence := range test.sequences {
			put(&word, 0, sequence, 0)
			take(&word, words)
			put(&message, 0, sequence, 0)
			take(&message, raws)
		}
		if words.errors == 0 || raws.errors == 0 {
			t.Errorf("%s: errors (int64, raw) = %d, %d, want both above 0", test.name, words.errors, raws.errors)
		}
	}

	consumer := newConsumer(1, window{}, zeroClock)
	var message raw
	put(&message, 0, 0, 0)
	message.Deployment++ // one field changed in transit
	take(&message, consumer)
	if want := expectedChecksum[raw]([]int64{1}); consumer.checksum == want {
		t.Errorf("checksum %d matches although a field changed", consumer.checksum)
	}
}

func TestConsumer_MarksTheWindowFromSampledMessages(t *testing.T) {
	now := int64(0)
	consumer := newConsumer(1, window{start: 1_000, end: 2_000}, func() int64 { return now })
	var word int64
	for sequence := range int64(64 * 4) { // samples at 0, 64, 128 and 192
		now = sequence * 10 // receive times 0, 640, 1280, 1920 at the samples
		put(&word, 0, sequence, now)
		take(&word, consumer)
	}
	now = 2_560
	put(&word, 0, 256, now)
	take(&word, consumer)

	if consumer.startCount != 128 || consumer.startTime != 1_280 {
		t.Errorf("start count, time = %d, %d, want 128, 1280", consumer.startCount, consumer.startTime)
	}
	if consumer.endCount != 256 || consumer.endTime != 2_560 {
		t.Errorf("end count, time = %d, %d, want 256, 2560", consumer.endCount, consumer.endTime)
	}
	if samples := consumer.latency.count(); samples != 2 { // 1280 and 1920 are inside [1000, 2000)
		t.Errorf("latency samples = %d, want 2 (only inside the window)", samples)
	}
}

func zeroClock() int64 { return 0 }
