package cmd

import (
	"sync/atomic"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

const MetricWindowSizeSeconds = 5

// LatencyEvent represents a latency measurement event
type LatencyEvent struct {
	LatencyMicros int64
	Timestamp     time.Time
}

// PerformanceStats tracks performance metrics with channel-based latency collection
type PerformanceStats struct {
	TotalOps   int64
	SuccessOps int64
	FailedOps  int64
	Histogram  *hdrhistogram.Histogram
	StartTime  time.Time

	// Channel-based latency collection (no locks needed)
	latencyChannel chan LatencyEvent
	errorChannel   chan struct{}
	done           chan struct{}

	// Per-second histograms (only accessed by stats goroutine)
	currentWindowStartSecond int64
	currentHistogram         *hdrhistogram.Histogram
	windowedHistograms       map[int64]*hdrhistogram.Histogram
	
	// Per-second metrics for high-fidelity monitoring
	// These fields enable CloudWatch to emit metrics with 1-second granularity
	// instead of the default 5-second metric window
	perSecondOps      map[int64]int64 // Operations per second (rolling 60s window)
	perSecondErrors   map[int64]int64 // Errors per second (rolling 60s window)
	lastSecond        int64           // Track last second for metrics
	currentSecondOps  int64           // Current second operations counter
	currentSecondErrors int64         // Current second errors counter
}

func NewPerformanceStats() *PerformanceStats {
	// Create histogram with 1 microsecond to 1 minute range, 3 significant digits
	hist := hdrhistogram.New(1, 60*1000*1000, 3)

	ps := &PerformanceStats{
		Histogram:          hist,
		StartTime:          time.Now(),
		windowedHistograms: make(map[int64]*hdrhistogram.Histogram),
		currentHistogram:   hdrhistogram.New(1, 60*1000*1000, 3),
		latencyChannel:     make(chan LatencyEvent, 1000000), // Buffered channel to prevent blocking
		errorChannel:       make(chan struct{}, 100),         // Buffered for errors
		done:               make(chan struct{}),
		perSecondOps:       make(map[int64]int64),
		perSecondErrors:    make(map[int64]int64),
		lastSecond:         time.Now().Unix(),
	}

	// Start the stats collection goroutine
	go ps.statsCollector()

	return ps
}

// statsCollector runs in a dedicated goroutine to process latency events without locks
func (ps *PerformanceStats) statsCollector() {
	for {
		select {
		case event := <-ps.latencyChannel:
			currentSecond := event.Timestamp.Unix()

			// Track per-second metrics for high-fidelity CloudWatch monitoring
			if currentSecond != ps.lastSecond {
				// Save current second's data before moving to new second
				if ps.currentSecondOps > 0 || ps.currentSecondErrors > 0 {
					ps.perSecondOps[ps.lastSecond] = ps.currentSecondOps
					ps.perSecondErrors[ps.lastSecond] = ps.currentSecondErrors
				}
				// Reset counters for new second
				ps.currentSecondOps = 0
				ps.currentSecondErrors = 0
				ps.lastSecond = currentSecond
				
				// Clean up old per-second data (maintain 60-second rolling window)
				// This prevents unbounded memory growth during long-running tests
				for sec := range ps.perSecondOps {
					if currentSecond - sec > 60 {
						delete(ps.perSecondOps, sec)
						delete(ps.perSecondErrors, sec)
					}
				}
			}
			ps.currentSecondOps++

			// Record in overall histogram (no lock needed, single goroutine)
			ps.Histogram.RecordValue(event.LatencyMicros)

			// Record in current monitoring window histogram (no lock needed, single goroutine)
			if currentSecond-ps.currentWindowStartSecond >= MetricWindowSizeSeconds {
				if ps.currentHistogram.TotalCount() > 0 {
					ps.windowedHistograms[ps.currentWindowStartSecond] = ps.currentHistogram
				}
				ps.currentWindowStartSecond = currentSecond
				ps.currentHistogram = hdrhistogram.New(1, 60*1000*1000, 3)
			}
			ps.currentHistogram.RecordValue(event.LatencyMicros)

			// No atomic needed - only this goroutine modifies these counters
			ps.SuccessOps++
			ps.TotalOps++

		case <-ps.errorChannel:
			currentSecond := time.Now().Unix()
			
			// Track per-second errors
			if currentSecond != ps.lastSecond {
				// Save current second's data
				if ps.currentSecondOps > 0 || ps.currentSecondErrors > 0 {
					ps.perSecondOps[ps.lastSecond] = ps.currentSecondOps
					ps.perSecondErrors[ps.lastSecond] = ps.currentSecondErrors
				}
				// Reset for new second
				ps.currentSecondOps = 0
				ps.currentSecondErrors = 0
				ps.lastSecond = currentSecond
			}
			ps.currentSecondErrors++
			
			// No atomic needed - only this goroutine modifies these counters
			ps.FailedOps++
			ps.TotalOps++

		case <-ps.done:
			return
		}
	}
}

// RecordLatency sends a latency event to the stats collector (lock-free)
func (ps *PerformanceStats) RecordLatency(latencyMicros int64) {
	select {
	case ps.latencyChannel <- LatencyEvent{
		LatencyMicros: latencyMicros,
		Timestamp:     time.Now(),
	}:
		// Event sent successfully
	default:
		// Channel is full, drop the event to prevent blocking
		// This is acceptable for high-throughput scenarios
	}
}

// RecordError sends an error event to the stats collector (lock-free)
func (ps *PerformanceStats) RecordError() {
	select {
	case ps.errorChannel <- struct{}{}:
		// Error event sent successfully
	default:
		// Channel is full, drop the event to prevent blocking
	}
}

func (ps *PerformanceStats) GetQPS() float64 {
	elapsed := time.Since(ps.StartTime).Seconds()
	if elapsed == 0 {
		return 0
	}
	return float64(atomic.LoadInt64(&ps.TotalOps)) / elapsed
}

func (ps *PerformanceStats) GetStats() (int64, int64, int64, float64, int64, int64, int64) {
	total := atomic.LoadInt64(&ps.TotalOps)
	success := atomic.LoadInt64(&ps.SuccessOps)
	failed := atomic.LoadInt64(&ps.FailedOps)
	qps := ps.GetQPS()

	// Note: Reading from histogram without lock is safe for reads
	// The worst case is we get slightly stale data, which is acceptable for monitoring
	var p50, p95, p99 int64
	if ps.Histogram.TotalCount() > 0 {
		p50 = ps.Histogram.ValueAtQuantile(50)
		p95 = ps.Histogram.ValueAtQuantile(95)
		p99 = ps.Histogram.ValueAtQuantile(99)
	}

	return total, success, failed, qps, p50, p95, p99
}

// GetPreviousWindowStats returns stats for the previous metrics window. We want to return previous window vs current since
// current metric window can still be filling up and have stale/incomplete data since were not using locks on these.
func (ps *PerformanceStats) GetPreviousWindowStats() (int64, int64, int64, int64, int64) {
	histToUse := ps.windowedHistograms[ps.currentWindowStartSecond-MetricWindowSizeSeconds]
	if histToUse == nil || histToUse.TotalCount() == 0 {
		return 0, 0, 0, 0, 0
	}

	return histToUse.TotalCount(),
		histToUse.ValueAtQuantile(50),
		histToUse.ValueAtQuantile(95),
		histToUse.ValueAtQuantile(99),
		histToUse.Max()
}

// GetOverallStats returns overall statistics
func (ps *PerformanceStats) GetOverallStats() (int64, int64, int64, float64) {
	total := atomic.LoadInt64(&ps.TotalOps)
	success := atomic.LoadInt64(&ps.SuccessOps)
	failed := atomic.LoadInt64(&ps.FailedOps)
	qps := ps.GetQPS()

	return total, success, failed, qps
}

// GetOpsLastSecond returns operations count for the last completed second
func (ps *PerformanceStats) GetOpsLastSecond() int64 {
	lastSec := time.Now().Unix() - 1
	if ops, exists := ps.perSecondOps[lastSec]; exists {
		return ops
	}
	return 0
}

// GetErrorsLastSecond returns error count for the last completed second
func (ps *PerformanceStats) GetErrorsLastSecond() int64 {
	lastSec := time.Now().Unix() - 1
	if errors, exists := ps.perSecondErrors[lastSec]; exists {
		return errors
	}
	return 0
}

// GetLastSecondPercentile returns percentile for the last second's latencies
func (ps *PerformanceStats) GetLastSecondPercentile(percentile float64) int64 {
	lastSec := time.Now().Unix() - 1
	
	// Check if we have a histogram for the last second in the windowed histograms
	if hist, exists := ps.windowedHistograms[lastSec]; exists && hist.TotalCount() > 0 {
		return hist.ValueAtQuantile(percentile)
	}
	
	// Fallback to current histogram if within the current window
	if ps.currentHistogram != nil && ps.currentHistogram.TotalCount() > 0 {
		return ps.currentHistogram.ValueAtQuantile(percentile)
	}
	
	return 0
}

// Close shuts down the stats collector goroutine
func (ps *PerformanceStats) Close() {
	close(ps.done)
}
