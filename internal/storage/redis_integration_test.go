//go:build integration

package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/edgevision/edgevisiongateway/internal/obs"
)

func testRedisAddr() string {
	if v := os.Getenv("EVG_TEST_REDIS_ADDR"); v != "" {
		return v
	}
	return "localhost:6379"
}

func newTestRedis(t *testing.T) (*RedisStore, *redis.Client) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: testRedisAddr(), DB: 15})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect redis: %v (is docker compose up?)", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis test db: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisStore(rdb, nil, 2*time.Second), rdb
}

func TestRedisDeviceRegistry(t *testing.T) {
	s, rdb := newTestRedis(t)
	ctx := context.Background()
	id := unique("rdev")
	d := Device{DeviceID: id, SiteID: "site-r", TokenHash: "h", CreatedAt: time.Now().UTC()}
	if err := s.PutDevice(ctx, d); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SiteID != "site-r" || got.TokenHash != "h" || got.Status != "active" {
		t.Errorf("bad device: %+v", got)
	}
	if _, err := s.GetDevice(ctx, unique("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: want ErrNotFound, got %v", err)
	}
	// It really is a hash, not a string blob.
	if typ := rdb.Type(ctx, deviceKey(id)).Val(); typ != "hash" {
		t.Errorf("device key type = %q, want hash", typ)
	}
}

func TestRedisHeartbeatTTLAndAliveCount(t *testing.T) {
	s, rdb := newTestRedis(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := unique("alive")
	if err := s.Heartbeat(ctx, id, now); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if ttl := rdb.TTL(ctx, aliveKey(id)).Val(); ttl <= 0 || ttl > 3*time.Second {
		t.Errorf("alive key TTL = %s, want (0,3s]", ttl)
	}
	n, err := s.CountAlive(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("CountAlive = %d, err=%v, want 1", n, err)
	}
	// A heartbeat 10s ago is outside the 2s liveness window and is pruned.
	if err := s.Heartbeat(ctx, id, now.Add(-10*time.Second)); err != nil {
		t.Fatalf("old heartbeat: %v", err)
	}
	n, err = s.CountAlive(ctx, now)
	if err != nil {
		t.Fatalf("CountAlive: %v", err)
	}
	if n != 0 {
		t.Errorf("CountAlive after stale heartbeat = %d, want 0", n)
	}
	if card := rdb.ZCard(ctx, aliveSetKey).Val(); card != 0 {
		t.Errorf("alive set not pruned: card=%d", card)
	}
}

func TestRedisHotCacheTTLAndMetrics(t *testing.T) {
	s, _ := newTestRedis(t)
	metrics := obs.New()
	s.metrics = metrics
	ctx := context.Background()
	id := unique("cache")
	dets := []Detection{{DeviceID: id, FrameID: "f1", CapturedAt: time.Now().UTC(), Label: "person", Confidence: 0.9, X: 1, Y: 2, W: 3, H: 4}}
	if err := s.CacheLatestDetections(ctx, id, dets, time.Second); err != nil {
		t.Fatalf("cache: %v", err)
	}
	got, err := s.GetLatestDetections(ctx, id)
	if err != nil {
		t.Fatalf("get cache: %v", err)
	}
	if len(got) != 1 || got[0].FrameID != "f1" || got[0].Confidence != 0.9 {
		t.Errorf("cache round-trip wrong: %+v", got)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := s.GetLatestDetections(ctx, id); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expired cache: want ErrCacheMiss, got %v", err)
	}
	snap := metrics.Snapshot()
	if snap.CacheHits != 1 || snap.CacheMisses != 1 {
		t.Errorf("metrics hits/misses = %d/%d, want 1/1", snap.CacheHits, snap.CacheMisses)
	}
}
