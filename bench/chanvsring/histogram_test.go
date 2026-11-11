package main

import (
	"math"
	"testing"
)

func TestHistogram_SmallValuesAreExact(t *testing.T) {
	histogram := &histogram{}
	for value := int64(1); value <= 100; value++ {
		histogram.record(value)
	}

	if count := histogram.count(); count != 100 {
		t.Errorf("count = %d, want 100", count)
	}
	for _, test := range []struct {
		quantile float64
		want     int64
	}{{0.5, 50}, {0.99, 99}, {0.999, 100}, {1, 100}, {0.01, 1}} {
		if got := histogram.percentile(test.quantile); got != test.want {
			t.Errorf("percentile(%v) = %d, want %d", test.quantile, got, test.want)
		}
	}
}

func TestHistogram_LargeValuesWithinOnePercent(t *testing.T) {
	histogram := &histogram{}
	for value := int64(1); value <= 100_000; value++ {
		histogram.record(value * 10_000) // 10us to 1s
	}

	for _, test := range []struct {
		quantile float64
		want     int64
	}{{0.5, 500_000_000}, {0.99, 990_000_000}, {0.999, 999_000_000}} {
		got := histogram.percentile(test.quantile)
		if relative := math.Abs(float64(got-test.want)) / float64(test.want); relative > 0.01 {
			t.Errorf("percentile(%v) = %d, want %d within 1%% (off by %.2f%%)", test.quantile, got, test.want, 100*relative)
		}
		if got < test.want {
			t.Errorf("percentile(%v) = %d, below the true value %d: must report the bucket's highest value", test.quantile,
				got, test.want)
		}
	}
}

func TestHistogram_NegativeCountsAsZeroAndEmptyIsZero(t *testing.T) {
	histogram := &histogram{}
	if got := histogram.percentile(0.99); got != 0 {
		t.Errorf("empty percentile = %d, want 0", got)
	}

	histogram.record(-5)
	histogram.record(math.MaxInt64)

	if got := histogram.percentile(0.5); got != 0 {
		t.Errorf("p50 of {-5, max} = %d, want 0", got)
	}
	if got := histogram.percentile(1); got < math.MaxInt64/2 {
		t.Errorf("p100 of {-5, max} = %d, want near %d", got, int64(math.MaxInt64))
	}
}
