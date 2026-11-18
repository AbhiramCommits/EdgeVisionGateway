// Package obs provides process-local telemetry: atomic counters and a
// log-spaced latency histogram that needs no external metrics system. The
// gateway exposes Snapshot as JSON on /metrics.
package obs

import (
	"math"
	"sort"
	"sync/atomic"
	"time"
)

// Histogram bounds: 128 log-spaced buckets from 10µs to 120s. The ratio
// between consecutive bounds is a constant ~1.137, so a quantile is resolved
// to within one bucket width (~14%) after linear interpolation.
const (
	histMin   = 10 * time.Microsecond
	histMax   = 120 * time.Second
	histCount = 128
)

// Metrics is the gateway's in-process telemetry sink. All counter methods are
// safe for concurrent use; the histogram updates are individual atomic adds,
// so recording never takes a lock.
type Metrics struct {
	framesReceived   atomic.Int64
	framesShed       atomic.Int64
	framesProcessed  atomic.Int64
	detectionsWriten atomic.Int64
	authFailures     atomic.Int64
	cacheHits        atomic.Int64
	cacheMisses      atomic.Int64
	queueDepth       atomic.Int64
	queueCapacity    atomic.Int64
	aliveDevices     atomic.Int64
	detectErrors     atomic.Int64

	bounds   []time.Duration
	counts   []atomic.Int64
	latCount atomic.Int64
	latSumNs atomic.Int64
	latMaxNs atomic.Int64
	overflow atomic.Int64
}

// New returns a ready Metrics value with log-spaced bucket bounds.
func New() *Metrics {
	m := &Metrics{
		bounds: make([]time.Duration, histCount),
		counts: make([]atomic.Int64, histCount),
	}
	ratio := math.Pow(float64(histMax)/float64(histMin), 1/float64(histCount-1))
	for i := 0; i < histCount; i++ {
		m.bounds[i] = time.Duration(float64(histMin) * math.Pow(ratio, float64(i))) //nolint:gosec // bounded
	}
	return m
}

// IncFramesReceived counts frames accepted for processing.
func (m *Metrics) IncFramesReceived() { m.framesReceived.Add(1) }

// IncFramesShed counts frames rejected by load shedding.
func (m *Metrics) IncFramesShed() { m.framesShed.Add(1) }

// IncFramesProcessed counts frames that completed detection and persistence.
func (m *Metrics) IncFramesProcessed() { m.framesProcessed.Add(1) }

// IncDetectionsWritten counts individual detection rows persisted.
func (m *Metrics) IncDetectionsWritten(n int) { m.detectionsWriten.Add(int64(n)) }

// IncAuthFailures counts rejected authentication attempts.
func (m *Metrics) IncAuthFailures() { m.authFailures.Add(1) }

// IncCacheHit counts Redis hot-cache hits.
func (m *Metrics) IncCacheHit() { m.cacheHits.Add(1) }

// IncCacheMiss counts Redis hot-cache misses.
func (m *Metrics) IncCacheMiss() { m.cacheMisses.Add(1) }

// IncDetectErrors counts frames that failed decoding or detection.
func (m *Metrics) IncDetectErrors() { m.detectErrors.Add(1) }

// SetQueue records current queue depth and capacity for the snapshot.
func (m *Metrics) SetQueue(depth, capacity int) {
	m.queueDepth.Store(int64(depth))
	m.queueCapacity.Store(int64(capacity))
}

// SetAliveDevices records the last observed fleet size.
func (m *Metrics) SetAliveDevices(n int) { m.aliveDevices.Store(int64(n)) }

// RecordLatency adds one end-to-end (enqueue → persisted) observation to the
// histogram. Values outside [10µs, 120s] land in the overflow bucket and are
// still counted in Count/Sum/Max.
func (m *Metrics) RecordLatency(d time.Duration) {
	if d < 0 {
		return
	}
	m.latCount.Add(1)
	m.latSumNs.Add(int64(d))
	for {
		old := m.latMaxNs.Load()
		if int64(d) <= old || m.latMaxNs.CompareAndSwap(old, int64(d)) {
			break
		}
	}
	idx := sort.Search(len(m.bounds), func(i int) bool { return m.bounds[i] >= d })
	if idx == len(m.bounds) {
		m.overflow.Add(1)
		return
	}
	m.counts[idx].Add(1)
}

// Quantile returns the q-quantile (0<q<=1) of recorded latencies using linear
// interpolation inside the containing log-spaced bucket. It returns 0 when no
// samples have been recorded.
func (m *Metrics) Quantile(q float64) time.Duration {
	if q <= 0 || q > 1 {
		return 0
	}
	total := m.latCount.Load()
	if total == 0 {
		return 0
	}
	target := q * float64(total)
	var cum float64
	for i := range m.counts {
		c := float64(m.counts[i].Load())
		if c == 0 {
			continue
		}
		if cum+c >= target {
			// Bucket i covers (bounds[i-1], bounds[i]]; interpolate inside it.
			var lo float64
			if i > 0 {
				lo = float64(m.bounds[i-1])
			}
			hi := float64(m.bounds[i])
			frac := 0.5
			if c > 0 {
				frac = (target - cum) / c
			}
			return time.Duration(lo + frac*(hi-lo))
		}
		cum += c
	}
	// Target fell into overflow: report the observed maximum.
	return time.Duration(m.latMaxNs.Load())
}

// LatencyStats is the JSON-serialisable latency view.
type LatencyStats struct {
	Count  int64   `json:"count"`
	SumMS  float64 `json:"sum_ms"`
	MeanMS float64 `json:"mean_ms"`
	MaxMS  float64 `json:"max_ms"`
	P50MS  float64 `json:"p50_ms"`
	P90MS  float64 `json:"p90_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	P999MS float64 `json:"p999_ms"`
}

// Snapshot is a point-in-time view of all metrics. It is designed to be
// marshalled to JSON for the /metrics endpoint and for benchmark artefacts.
type Snapshot struct {
	FramesReceived   int64        `json:"frames_received"`
	FramesShed       int64        `json:"frames_shed"`
	FramesProcessed  int64        `json:"frames_processed"`
	DetectionsWriten int64        `json:"detections_written"`
	AuthFailures     int64        `json:"auth_failures"`
	DetectErrors     int64        `json:"detect_errors"`
	CacheHits        int64        `json:"cache_hits"`
	CacheMisses      int64        `json:"cache_misses"`
	CacheHitRate     float64      `json:"cache_hit_rate"`
	ShedRate         float64      `json:"shed_rate"`
	QueueDepth       int          `json:"queue_depth"`
	QueueCapacity    int          `json:"queue_capacity"`
	QueueHighWater   int          `json:"queue_high_water_mark"`
	AliveDevices     int64        `json:"alive_devices"`
	Latency          LatencyStats `json:"latency"`
}

// Snapshot returns a consistent-enough view of the metrics. Individual
// counters are read atomically; the result is a plain struct safe to marshal.
func (m *Metrics) Snapshot() Snapshot {
	recv := m.framesReceived.Load()
	shed := m.framesShed.Load()
	hits := m.cacheHits.Load()
	misses := m.cacheMisses.Load()
	count := m.latCount.Load()
	sum := time.Duration(m.latSumNs.Load())
	var mean float64
	if count > 0 {
		mean = float64(sum) / float64(count) / float64(time.Millisecond)
	}
	s := Snapshot{
		FramesReceived:   recv,
		FramesShed:       shed,
		FramesProcessed:  m.framesProcessed.Load(),
		DetectionsWriten: m.detectionsWriten.Load(),
		AuthFailures:     m.authFailures.Load(),
		DetectErrors:     m.detectErrors.Load(),
		CacheHits:        hits,
		CacheMisses:      misses,
		QueueDepth:       int(m.queueDepth.Load()),
		QueueCapacity:    int(m.queueCapacity.Load()),
		AliveDevices:     m.aliveDevices.Load(),
		Latency: LatencyStats{
			Count:  count,
			SumMS:  float64(sum) / float64(time.Millisecond),
			MeanMS: mean,
			MaxMS:  float64(m.latMaxNs.Load()) / float64(time.Millisecond),
			P50MS:  ms(m.Quantile(0.50)),
			P90MS:  ms(m.Quantile(0.90)),
			P95MS:  ms(m.Quantile(0.95)),
			P99MS:  ms(m.Quantile(0.99)),
			P999MS: ms(m.Quantile(0.999)),
		},
	}
	if total := hits + misses; total > 0 {
		s.CacheHitRate = float64(hits) / float64(total)
	}
	if total := recv + shed; total > 0 {
		s.ShedRate = float64(shed) / float64(total)
	}
	return s
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
