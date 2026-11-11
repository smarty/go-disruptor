package main

import "testing"

func TestSchedule_OneProducerOneMessagePerMicrosecond(t *testing.T) {
	schedule := schedule{rate: 1_000_000, producers: 1, producer: 0}

	for _, test := range []struct{ elapsed, due int64 }{
		{0, 0}, {999, 0}, {1_000, 1}, {1_999, 1}, {2_000, 2}, {1_000_000_000, 1_000_000},
	} {
		if due := schedule.due(test.elapsed); due != test.due {
			t.Errorf("due(%d) = %d, want %d", test.elapsed, due, test.due)
		}
	}
	for _, test := range []struct{ sequence, at int64 }{{0, 1_000}, {1, 2_000}, {999_999, 1_000_000_000}} {
		if at := schedule.at(test.sequence); at != test.at {
			t.Errorf("at(%d) = %d, want %d", test.sequence, at, test.at)
		}
	}
}

func TestSchedule_ProducersInterleaveTheTotalRate(t *testing.T) {
	const rate, producers = 3_000_000, 3 // one message every 333.3ns in total; each producer every 1us
	schedules := make([]schedule, producers)
	for index := range schedules {
		schedules[index] = schedule{rate: rate, producers: producers, producer: int64(index)}
	}

	if at := []int64{schedules[0].at(0), schedules[1].at(0), schedules[2].at(0), schedules[0].at(1)}; at[0] != 334 ||
		at[1] != 667 || at[2] != 1_000 || at[3] != 1_334 {
		t.Errorf("first due times = %v, want [334 667 1000 1334]", at)
	}
	for _, elapsed := range []int64{0, 333, 334, 666, 667, 1_000, 12_345, 999_999_999, 4_000_000_000} {
		var total int64
		for _, each := range schedules {
			total += each.due(elapsed)
		}
		if want := elapsed * rate / 1_000_000_000; total != want {
			t.Errorf("due at %dns across producers = %d, want %d", elapsed, total, want)
		}
	}
}

func TestSchedule_EachMessageIsDueExactlyAtItsTime(t *testing.T) {
	for _, schedule := range []schedule{
		{rate: 40_000_000, producers: 6, producer: 5},
		{rate: 1_000_000, producers: 4, producer: 2},
		{rate: 7, producers: 2, producer: 1},
	} {
		for _, sequence := range []int64{0, 1, 2, 63, 64, 1_000, 123_456_789} {
			at := schedule.at(sequence)
			if due := schedule.due(at); due != sequence+1 {
				t.Errorf("%+v: due(at(%d)=%d) = %d, want %d", schedule, sequence, at, due, sequence+1)
			}
			if due := schedule.due(at - 1); due != sequence {
				t.Errorf("%+v: due(at(%d)-1) = %d, want %d", schedule, sequence, due, sequence)
			}
		}
	}
}
