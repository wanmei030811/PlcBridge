// Package bench —— sampler：周期性采样 runtime.ReadMemStats +
// plcbridge metrics（通过 HTTP scrape） +
// mock PLC 状态，输出到 CSV 文件。
package bench

import (
	"encoding/csv"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

// PlcSnapshot 描述 mock PLC 池当前累计的读/写请求数。
type PlcSnapshot struct {
	Reads  uint64
	Writes uint64
	Queues int // placeholder for forward queue length（当前未注入）
}

// Sampler 配置项。
type Sampler struct {
	outPath string
	period  time.Duration

	plcSnap func() PlcSnapshot
	metrics func() (CollectDurSnapshot, error) // 暴露给外部注入（通常 = ScrapeCollectDuration）

	// runtime metrics
	lastPauseNs atomic.Uint64

	// csv writer
	w        *csv.Writer
	f        *os.File
	rowStart time.Time
}

// NewSampler 创建一个 sampler，每 period 秒采样一次，写到 outPath CSV。
//
// plcSnap / metrics 由 bench 注入；metrics 为 nil 时不抓 metrics。
func NewSampler(outPath string, period time.Duration,
	plcSnap func() PlcSnapshot,
	metricsFn func() (CollectDurSnapshot, error)) (*Sampler, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", outPath, err)
	}
	w := csv.NewWriter(f)
	if err := w.Write([]string{
		"t_offset_s",
		"goroutines",
		"heap_alloc_mb",
		"heap_sys_mb",
		"heap_inuse_mb",
		"num_gc",
		"pause_total_ms",
		"mock_reads_delta",
		"mock_writes_delta",
		"queue_total",
		"hist_count",
		"p50_ms",
		"p99_ms",
		"max_ms",
	}); err != nil {
		f.Close()
		return nil, err
	}
	s := &Sampler{
		outPath:  outPath,
		period:   period,
		plcSnap:  plcSnap,
		metrics:  metricsFn,
		w:        w,
		f:        f,
		rowStart: time.Now(),
	}
	go s.loop()
	return s, nil
}

func (s *Sampler) loop() {
	t := time.NewTicker(s.period)
	defer t.Stop()
	var prevReads, prevWrites uint64
	for range t.C {
		snap := s.plcSnap()
		metricsSnap := CollectDurSnapshot{}
		if s.metrics != nil {
			if m, err := s.metrics(); err == nil {
				metricsSnap = m
			}
		}
		if err := s.writeRow(snap, prevReads, prevWrites, metricsSnap); err != nil {
			fmt.Fprintf(os.Stderr, "[sampler] write: %v\n", err)
			return
		}
		prevReads = snap.Reads
		prevWrites = snap.Writes
	}
}

func (s *Sampler) writeRow(snap PlcSnapshot, prevReads, prevWrites uint64, ms CollectDurSnapshot) error {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	elapsed := time.Since(s.rowStart).Seconds()
	rDelta := snap.Reads - prevReads
	wDelta := snap.Writes - prevWrites

	s.w.Write([]string{
		fmt.Sprintf("%.1f", elapsed),
		fmt.Sprintf("%d", runtime.NumGoroutine()),
		fmt.Sprintf("%.1f", float64(mem.HeapAlloc)/1024/1024),
		fmt.Sprintf("%.1f", float64(mem.HeapSys)/1024/1024),
		fmt.Sprintf("%.1f", float64(mem.HeapInuse)/1024/1024),
		fmt.Sprintf("%d", mem.NumGC),
		fmt.Sprintf("%.1f", float64(mem.PauseTotalNs)/1e6),
		fmt.Sprintf("%d", rDelta),
		fmt.Sprintf("%d", wDelta),
		fmt.Sprintf("%d", snap.Queues),
		fmt.Sprintf("%d", ms.Count),
		fmt.Sprintf("%.2f", ms.P50MS),
		fmt.Sprintf("%.2f", ms.P99MS),
		fmt.Sprintf("%.2f", ms.MaxMS),
	})
	s.w.Flush()
	return nil
}

// Close 停止采样，关闭 csv。
func (s *Sampler) Close() error {
	// 写最后一帧
	snap := s.plcSnap()
	ms := CollectDurSnapshot{}
	if s.metrics != nil {
		if m, err := s.metrics(); err == nil {
			ms = m
		}
	}
	_ = s.writeRow(snap, 0, 0, ms)
	s.w.Flush()
	return s.f.Close()
}
