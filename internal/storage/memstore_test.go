package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/obs"
)

func testDet(device, frame, label string, at time.Time, conf float64) Detection {
	return Detection{
		DeviceID: device, FrameID: frame, CapturedAt: at,
		Label: label, Confidence: conf, X: 1, Y: 2, W: 30, H: 40,
	}
}

func TestMemStoreDetectionRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	dets := []Detection{
		testDet("cam-1", "f1", "person", base, 0.9),
		testDet("cam-1", "f2", "object", base.Add(time.Second), 0.8),
		testDet("cam-2", "f3", "person", base.Add(2*time.Second), 0.7),
	}
	if err := m.InsertDetections(ctx, dets); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if m.Len() != 3 {
		t.Fatalf("len = %d, want 3", m.Len())
	}

	got, err := m.QueryByDevice(ctx, "cam-1", base.Add(-time.Hour), base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("query device: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("query device returned %d rows, want 2", len(got))
	}
	if !got[0].CapturedAt.After(got[1].CapturedAt) {
		t.Errorf("results not newest-first: %s then %s", got[0].CapturedAt, got[1].CapturedAt)
	}
	if got[0].ID == 0 {
		t.Error("insert should assign ids")
	}

	byLabel, err := m.QueryByLabel(ctx, "person", base.Add(-time.Hour), base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("query label: %v", err)
	}
	if len(byLabel) != 2 {
		t.Fatalf("query label returned %d rows, want 2", len(byLabel))
	}
}

func TestMemStoreTimeFilteringIsHalfOpen(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	m.InsertDetections(ctx, []Detection{
		testDet("cam", "before", "person", base.Add(-time.Millisecond), 0.5),
		testDet("cam", "at-from", "person", base, 0.5),
		testDet("cam", "inside", "person", base.Add(time.Second), 0.5),
		testDet("cam", "at-to", "person", base.Add(2*time.Second), 0.5),
	})
	got, err := m.QueryByDevice(ctx, "cam", base, base.Add(2*time.Second), 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 || got[0].FrameID != "inside" || got[1].FrameID != "at-from" {
		t.Fatalf("half-open window wrong: %+v", got)
	}
}

func TestMemStoreLimitReturnsNewest(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var dets []Detection
	for i := 0; i < 10; i++ {
		dets = append(dets, testDet("cam", "f", "person", base.Add(time.Duration(i)*time.Second), 0.5))
	}
	m.InsertDetections(ctx, dets)
	got, err := m.QueryByDevice(ctx, "cam", base, base.Add(time.Minute), 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit ignored: got %d", len(got))
	}
	if !got[0].CapturedAt.Equal(base.Add(9 * time.Second)) {
		t.Errorf("first row = %s, want newest", got[0].CapturedAt)
	}
}

func TestMemStoreInvalidRange(t *testing.T) {
	m := NewMemStore()
	now := time.Now()
	if _, err := m.QueryByDevice(context.Background(), "cam", now, now, 10); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("want ErrInvalidRange, got %v", err)
	}
}

func TestMemStoreDeviceLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	m := NewMemStore(WithClock(func() time.Time { return now }))
	d := Device{DeviceID: "cam-9", SiteID: "site-a", TokenHash: "abc"}
	if err := m.RegisterDevice(ctx, d); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := m.GetDevice(ctx, "cam-9")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SiteID != "site-a" || got.Status != "active" || got.CreatedAt.IsZero() {
		t.Errorf("bad device: %+v", got)
	}
	if _, err := m.GetDevice(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing device: want ErrNotFound, got %v", err)
	}
	later := now.Add(time.Minute)
	if err := m.TouchDevice(ctx, "cam-9", later); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ = m.GetDevice(ctx, "cam-9")
	if !got.LastSeenAt.Equal(later) {
		t.Errorf("last seen = %s, want %s", got.LastSeenAt, later)
	}
	if err := m.TouchDevice(ctx, "ghost", later); !errors.Is(err, ErrNotFound) {
		t.Errorf("touch unknown: want ErrNotFound, got %v", err)
	}
}

func TestMemStoreHeartbeatExpiryWithInjectedClock(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	clock := now
	m := NewMemStore(WithClock(func() time.Time { return clock }), WithHeartbeatTTL(10*time.Second))
	ctx := context.Background()
	if err := m.Heartbeat(ctx, "cam-1", now); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if n, _ := m.CountAlive(ctx, now); n != 1 {
		t.Fatalf("alive = %d, want 1", n)
	}
	clock = now.Add(9 * time.Second)
	if n, _ := m.CountAlive(ctx, clock); n != 1 {
		t.Fatalf("alive after 9s = %d, want 1", n)
	}
	clock = now.Add(11 * time.Second)
	if n, _ := m.CountAlive(ctx, clock); n != 0 {
		t.Fatalf("alive after 11s = %d, want 0 (TTL expired)", n)
	}
}

func TestMemStoreCacheExpiryAndMetrics(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	clock := now
	metrics := obs.New()
	m := NewMemStore(WithClock(func() time.Time { return clock }), WithMetrics(metrics))
	ctx := context.Background()

	dets := []Detection{testDet("cam-1", "f1", "person", now, 0.9)}
	if err := m.CacheLatestDetections(ctx, "cam-1", dets, time.Minute); err != nil {
		t.Fatalf("cache: %v", err)
	}
	got, err := m.GetLatestDetections(ctx, "cam-1")
	if err != nil || len(got) != 1 || got[0].FrameID != "f1" {
		t.Fatalf("cache get: %+v err=%v", got, err)
	}
	// Mutating the returned slice must not corrupt the cache.
	got[0].FrameID = "mutated"
	again, _ := m.GetLatestDetections(ctx, "cam-1")
	if again[0].FrameID != "f1" {
		t.Error("cache returned aliased slice")
	}
	if metrics.Snapshot().CacheHits != 2 {
		t.Errorf("cache hits = %d, want 2", metrics.Snapshot().CacheHits)
	}

	clock = now.Add(2 * time.Minute)
	if _, err := m.GetLatestDetections(ctx, "cam-1"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expired cache: want ErrCacheMiss, got %v", err)
	}
	if got := metrics.Snapshot().CacheMisses; got != 1 {
		t.Errorf("cache misses = %d, want 1", got)
	}
	if _, err := m.GetLatestDetections(ctx, "never-cached"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("missing cache: want ErrCacheMiss, got %v", err)
	}
}

func TestMemStoreDefaultLimit(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	base := time.Now().UTC().Add(-time.Hour)
	dets := make([]Detection, 150)
	for i := range dets {
		dets[i] = testDet("cam", "f", "person", base.Add(time.Duration(i)*time.Second), 0.5)
	}
	m.InsertDetections(ctx, dets)
	got, err := m.QueryByDevice(ctx, "cam", base.Add(-time.Hour), base.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("default limit = %d, want 100", len(got))
	}
}
