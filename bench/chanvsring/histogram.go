package main

import (
	"math"
	"math/bits"
)

// histogram counts latencies in nanoseconds in log-linear buckets: exact below 128, then 128 buckets per power of two,
// so a reported value is at most 1/128 (0.8%) above the true one. It never allocates after creation.
type histogram struct {
	counts [bucketCount]uint64
	total  uint64
}

const (
	subBucketBits = 7
	subBuckets    = 1 << subBucketBits
	bucketCount   = (64 - subBucketBits + 1) << subBucketBits
)

func (this *histogram) record(nanos int64) {
	this.counts[bucketOf(uint64(max(nanos, 0)))]++
	this.total++
}

func (this *histogram) count() uint64 { return this.total }

// percentile returns the nearest-rank value at quantile (0 to 1): the highest value of the bucket that holds it.
func (this *histogram) percentile(quantile float64) int64 {
	if this.total == 0 {
		return 0
	}
	rank := max(uint64(math.Ceil(quantile*float64(this.total))), 1)
	var seen uint64
	for index, count := range this.counts {
		if seen += count; seen >= rank {
			return int64(min(highestOf(index), math.MaxInt64))
		}
	}
	return math.MaxInt64
}

func bucketOf(value uint64) int {
	if value < subBuckets {
		return int(value)
	}
	shift := bits.Len64(value) - subBucketBits - 1
	return (shift+1)<<subBucketBits + int(value>>shift) - subBuckets
}

func highestOf(index int) uint64 {
	if index < subBuckets {
		return uint64(index)
	}
	shift := index>>subBucketBits - 1
	lowest := uint64(index&(subBuckets-1)+subBuckets) << shift
	return lowest + 1<<shift - 1
}
