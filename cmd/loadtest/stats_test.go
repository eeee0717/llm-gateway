package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSummarizeTakesRealSamplesNotInterpolatedOnes(t *testing.T) {
	// 1ms 到 100ms 各一个，倒着放，顺便盯住排序。
	samples := make([]time.Duration, 0, 100)
	for i := 100; i >= 1; i-- {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}

	got := summarize(samples)

	require.Equal(t, 100, got.N)
	require.Equal(t, 1*time.Millisecond, got.Min)
	require.Equal(t, 50*time.Millisecond, got.P50)
	require.Equal(t, 90*time.Millisecond, got.P90)
	require.Equal(t, 99*time.Millisecond, got.P99)
	require.Equal(t, 100*time.Millisecond, got.Max)
}

// 分位数取的是实际测到过的那个样本，不在相邻两个之间插值：报出来的数字必须是真发生过的。
func TestSummarizeRoundsTheRankUp(t *testing.T) {
	samples := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}

	got := summarize(samples)

	require.Equal(t, 20*time.Millisecond, got.P50) // 第 ceil(3×0.5)=2 个
	require.Equal(t, 30*time.Millisecond, got.P90) // 第 ceil(3×0.9)=3 个
}

func TestSummarizeLeavesTheCallersSliceAlone(t *testing.T) {
	samples := []time.Duration{3, 1, 2}

	summarize(samples)

	require.Equal(t, []time.Duration{3, 1, 2}, samples)
}
