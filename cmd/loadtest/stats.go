package main

import (
	"slices"
	"time"
)

// summary 是一组耗时的分布。压测关心的是尾部，所以除中位数外只列高分位。
type summary struct {
	N   int
	Min time.Duration
	P50 time.Duration
	P90 time.Duration
	P99 time.Duration
	Max time.Duration
}

// summarize 汇总一组耗时，不改动调用方的切片。
func summarize(samples []time.Duration) summary {
	if len(samples) == 0 {
		return summary{}
	}
	sorted := slices.Sorted(slices.Values(samples))
	return summary{
		N:   len(sorted),
		Min: sorted[0],
		P50: percentile(sorted, 50),
		P90: percentile(sorted, 90),
		P99: percentile(sorted, 99),
		Max: sorted[len(sorted)-1],
	}
}

// percentile 用最近秩法取分位数：排序后的第 ceil(n × p/100) 个样本。
// 不在相邻两个样本之间插值，报出来的数字就一定是真测到过的一次请求。
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (len(sorted)*p + 99) / 100
	return sorted[min(max(rank, 1), len(sorted))-1]
}
