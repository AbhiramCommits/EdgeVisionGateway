package obs

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestQuantilesOnKnownDistribution(t *testing.T) {
	m := New()
	// Uniform 1ms .. 100.99ms, 10 000 samples.
	const n = 10000
	for i := 0; i < n; i++ {
		m.RecordLatency(time.Millisecond + time.Duration(i)*10*time.Microsecond)
	}
	cases := []struct {
		q    float64
		want time.Duration
	}{
		{0.50, 50 * time.Millisecond},
		{0.90, 91 * time.Millisecond},
		{0.99, 100 * time.Millisecond},
	}
	for _, c := range cases {
		got := m.Quantile(c.q)
		// Allow one interpolation bucket of relative error.
		if diff := math.Abs(float64(got-c.want)) / float64(c.want); diff > 0.10 {
			t.Errorf("Quantile(%v) = %s, want ~%s (relative diff %.3f)", c.q, got, c.want, diff)
		}
	}
	if got := m.Quantile(0.5); got <= m.Quantile(0.1) || got >= m.Quantile(0.9) {
		t.Errorf("quantiles not monotonic: p10=%s p50=%s p90=%s", m.Quantile(0.1), m.Quantile(0.5), m.Quantile(0.9))
	}
}

func TestQuantileEmptyIsZero(t *testing.T) {
	if got := New().Quantile(0.99); got != 0 {
		t.Errorf("empty histogram quantile = %s, want 0", got)
	}
}

func TestSnapshotCountersAndJSON(t *testing.T) {
	m := New()
	m.IncFramesReceived()
	m.IncFramesReceived()
	m.IncFramesShed()
	m.IncFramesProcessed()
	m.IncDetectionsWritten(3)
	m.IncAuthFailures()
	m.IncCacheHit()
	m.IncCacheMiss()
	m.IncCacheMiss()
	m.SetQueue(17, 100)
	m.SetAliveDevices(42)
	m.RecordLatency(12 * time.Millisecond)

	s := m.Snapshot()
	if s.FramesReceived != 2 || s.FramesShed != 1 || s.FramesProcessed != 1 {
		t.Errorf("counters wrong: %+v", s)
	}
	if s.DetectionsWriten != 3 || s.AuthFailures != 1 {
		t.Errorf("counters wrong: %+v", s)
	}
	if s.QueueDepth != 17 || s.QueueCapacity != 100 || s.AliveDevices != 42 {
		t.Errorf("gauges wrong: %+v", s)
	}
	if math.Abs(s.CacheHitRate-1.0/3.0) > 1e-9 {
		t.Errorf("cache hit rate = %v, want 1/3", s.CacheHitRate)
	}
	if math.Abs(s.ShedRate-1.0/3.0) > 1e-9 {
		t.Errorf("shed rate = %v, want 1/3", s.ShedRate)
	}
	if s.Latency.Count != 1 || s.Latency.P50MS < 10 || s.Latency.P50MS > 14 {
		t.Errorf("latency stats wrong: %+v", s.Latency)
	}

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	for _, key := range []string{"frames_received", "frames_shed", "queue_depth", "latency"} {
		if _, ok := back[key]; !ok {
			t.Errorf("snapshot JSON missing %q", key)
		}
	}
}

func TestRecordLatencyIgnoresNegative(t *testing.T) {
	m := New()
	m.RecordLatency(-time.Second)
	if got := m.Snapshot().Latency.Count; got != 0 {
		t.Errorf("negative latency counted: count=%d", got)
	}
}

func TestOverflowBeyondMax(t *testing.T) {
	m := New()
	m.RecordLatency(5 * time.Minute)
	s := m.Snapshot()
	if s.Latency.Count != 1 {
		t.Errorf("overflow not counted in count: %+v", s.Latency)
	}
	if s.Latency.MaxMS < 299000 {
		t.Errorf("max = %v ms, want ~300000", s.Latency.MaxMS)
	}
}

func BenchmarkRecordLatency(b *testing.B) {
	m := New()
	for i := 0; i < b.N; i++ {
		m.RecordLatency(time.Duration(i%1000) * time.Microsecond)
	}
}
