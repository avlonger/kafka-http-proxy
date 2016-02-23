/*
* Copyright (C) 2015 Alexey Gladkov <gladkov.alexey@gmail.com>
*
* This file is covered by the GNU General Public License,
* which should be included with kafka-http-proxy as the file COPYING.
 */

package main

import (
	"github.com/facebookgo/metrics"

	"runtime"
	"syscall"
	"time"
)

// SnapshotTimer is a snapshot of the ResponseTimer values.
type SnapshotTimer struct {
	Min   int64
	Max   int64
	Avg   float64
	Count int64

	Rate1   float64
	Rate5   float64
	Rate15  float64
	RateAvg float64

	Percentile05  float64
	Percentile075 float64
	Percentile095 float64
	Percentile099 float64
}

// GetSnapshot creates a snapshot of the ResponseTimer values.
func GetSnapshot(s metrics.Timer) (res *SnapshotTimer) {
	res = &SnapshotTimer{
		Min:           s.Min(),
		Max:           s.Max(),
		Avg:           s.Mean(),
		Count:         s.Count(),
		Rate1:         s.Rate1(),
		Rate5:         s.Rate5(),
		Rate15:        s.Rate15(),
		RateAvg:       s.RateMean(),
		Percentile05:  s.Percentile(0.5),
		Percentile075: s.Percentile(0.75),
		Percentile095: s.Percentile(0.95),
		Percentile099: s.Percentile(0.99),
	}
	return
}

// MetricStats contains statistics about HTTP responses.
type MetricStats struct {
	HTTPStatus       map[int]metrics.Counter
	HTTPResponseTime map[string]metrics.Timer
}

// NewMetricStats creates new MetricStats object.
func NewMetricStats() *MetricStats {
	return &MetricStats{
		HTTPStatus:       NewHTTPStatus([]int{200, 400, 404, 405, 416, 500, 502, 503}),
		HTTPResponseTime: NewTimings([]string{"GET", "POST", "GetTopicList", "GetTopicInfo", "GetPartitionInfo",
			"CommitOffset"}),
	}
}

// RuntimeStat contains runtime statistic.
type RuntimeStat struct {
	Goroutines      int
	CgoCall         int64
	CPU             int
	GoMaxProcs      int
	UsedDescriptors int
}

// GetRuntimeStat creates new RuntimeStat object.
func GetRuntimeStat() *RuntimeStat {
	data := &RuntimeStat{
		Goroutines:      runtime.NumGoroutine(),
		CgoCall:         runtime.NumCgoCall(),
		CPU:             runtime.NumCPU(),
		GoMaxProcs:      runtime.GOMAXPROCS(0),
		UsedDescriptors: 0,
	}

	var nofileLimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &nofileLimit)
	if err != nil {
		return data
	}
	for i := 0; i < int(nofileLimit.Cur); i++ {
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(i), syscall.F_GETFD, 0)
		if errno == 0 {
			data.UsedDescriptors++
		}
	}
	return data
}

// NewHTTPStatus creates object for HTTP status statistic.
func NewHTTPStatus(codes []int) map[int]metrics.Counter {
	HTTPStatus := make(map[int]metrics.Counter)

	for _, code := range codes {
		HTTPStatus[code] = metrics.NewCounter()
	}
	return HTTPStatus
}

// NewCounters creates map of counters
func NewCounters(names []string) map[string]metrics.Counter {
	res := make(map[string]metrics.Counter)

	for _, name := range names {
		res[name] = metrics.NewCounter()
	}
	return res
}

// NewTimings creates map of timings
func NewTimings(names []string) map[string]metrics.Timer {
	res := make(map[string]metrics.Timer)

	for _, name := range names {
		res[name] = metrics.NewTimer()
	}

	go func() {
		for {
			for _, name := range names {
				res[name].Tick()
			}
			time.Sleep(metrics.TickDuration)
		}
	}()

	return res
}
