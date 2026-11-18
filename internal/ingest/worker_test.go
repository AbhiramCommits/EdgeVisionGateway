package ingest

import (
	"bytes"
	"context"
	"image/jpeg"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/queue"
	"github.com/edgevision/edgevisiongateway/internal/storage"
	"github.com/edgevision/edgevisiongateway/internal/vision"
)

func TestPoolDecodeDetectPersistCache(t *testing.T) {
	metrics := obs.New()
	ms := storage.NewMemStore(storage.WithMetrics(metrics), storage.WithHeartbeatTTL(time.Minute))
	q := queue.New(8, 0.9, metrics)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recent := NewRecentTracker(500 * time.Millisecond)
	pool := NewPool(PoolOptions{
		Queue:         q,
		Store:         ms,
		Metrics:       metrics,
		Recent:        recent,
		WorkerCount:   1,
		CacheTTL:      time.Minute,
		BatchMax:      10,
		FlushInterval: 5 * time.Millisecond,
		Logger:        logger,
	})
	pool.Start()

	imgs, _ := vision.GenerateSequence(11, 40, 320, 240, 3, 4)
	// Frame 0 is an empty warm-up frame (the detector bootstraps its background
	// from it); frames 20/21 contain moving objects.
	var encoded [3][]byte
	for i, idx := range []int{0, 20, 21} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, imgs[idx], &jpeg.Options{Quality: 85}); err != nil {
			t.Fatalf("encode: %v", err)
		}
		encoded[i] = buf.Bytes()
	}

	now := time.Now().UTC()
	for i, data := range encoded {
		frameID := []string{"f0", "f1", "f2"}[i]
		if !q.TryEnqueue(&queue.FrameJob{
			DeviceID: "cam-w", FrameID: frameID, CapturedAt: now.Add(time.Duration(i) * time.Millisecond),
			EnqueuedAt: time.Now(), Frame: data, ContentType: "image/jpeg",
		}) {
			t.Fatal("enqueue failed")
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if metrics.Snapshot().FramesProcessed >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := metrics.Snapshot()
	if snap.FramesProcessed != 3 {
		t.Fatalf("frames processed = %d, want 3", snap.FramesProcessed)
	}
	if snap.DetectionsWriten == 0 || ms.Len() == 0 {
		t.Fatalf("no detections persisted (written=%d store=%d)", snap.DetectionsWriten, ms.Len())
	}
	cached, err := ms.GetLatestDetections(context.Background(), "cam-w")
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	if len(cached) == 0 {
		t.Fatal("cache has no detections for cam-w")
	}
	for _, d := range cached {
		if d.DeviceID != "cam-w" || d.FrameID != "f2" {
			t.Errorf("cached detection has device/frame %s/%s, want cam-w/f2", d.DeviceID, d.FrameID)
		}
	}
	if snap.Latency.Count != 3 {
		t.Errorf("latency samples = %d, want 3", snap.Latency.Count)
	}
	if !recent.IsRecent("cam-w", time.Now()) {
		t.Error("recent tracker not marked after persistence")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pool.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestPoolShutdownBeforeStart(t *testing.T) {
	metrics := obs.New()
	q := queue.New(2, 0.9, metrics)
	pool := NewPool(PoolOptions{Queue: q, Store: storage.NewMemStore(), Metrics: metrics, WorkerCount: 1})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pool.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestPoolDrainsBacklogOnShutdown(t *testing.T) {
	metrics := obs.New()
	ms := storage.NewMemStore(storage.WithMetrics(metrics))
	q := queue.New(32, 1.0, metrics)
	pool := NewPool(PoolOptions{
		Queue: q, Store: ms, Metrics: metrics, WorkerCount: 2,
		CacheTTL: time.Minute, BatchMax: 100, FlushInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	pool.Start()
	imgs, _ := vision.GenerateSequence(3, 20, 160, 120, 2, 2)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, imgs[15], &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 10; i++ {
		q.TryEnqueue(&queue.FrameJob{DeviceID: "drain", FrameID: "f", CapturedAt: time.Now(), EnqueuedAt: time.Now(), Frame: buf.Bytes()})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Shutdown(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := metrics.Snapshot().FramesProcessed; got != 10 {
		t.Fatalf("frames processed = %d, want 10", got)
	}
	if q.Depth() != 0 {
		t.Errorf("queue depth after drain = %d, want 0", q.Depth())
	}
}
