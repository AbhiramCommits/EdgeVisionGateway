// Package storage defines the persistence surface used by the gateway:
// a durable detection store (Postgres), a device registry and hot detection
// cache (Redis), and an in-memory implementation used by unit tests and by
// benchmarks on machines without Docker.
package storage

import (
	"context"
	"time"
)

// DetectionStore is the durable, queryable detection store. Time windows are
// half-open: [from, to).
type DetectionStore interface {
	// Migrate applies schema.sql and pre-creates today ± 1 day partitions.
	Migrate(ctx context.Context) error
	// EnsurePartition idempotently creates the partition covering day (UTC).
	EnsurePartition(ctx context.Context, day time.Time) error
	// InsertDetections writes a batch of detections atomically per batch.
	InsertDetections(ctx context.Context, dets []Detection) error
	// QueryByDevice returns the newest detections for one device.
	QueryByDevice(ctx context.Context, deviceID string, from, to time.Time, limit int) ([]Detection, error)
	// QueryByLabel returns the newest detections for one label.
	QueryByLabel(ctx context.Context, label string, from, to time.Time, limit int) ([]Detection, error)
	// Close releases the underlying connection pool.
	Close() error
}

// DeviceStore is the device registry: durable registration plus liveness.
type DeviceStore interface {
	// RegisterDevice durably registers or updates a device.
	RegisterDevice(ctx context.Context, d Device) error
	// PutDevice writes the device to the fast registry (Redis hash).
	PutDevice(ctx context.Context, d Device) error
	// GetDevice reads a device from the fast registry.
	GetDevice(ctx context.Context, deviceID string) (Device, error)
	// TouchDevice records a durable last-seen timestamp.
	TouchDevice(ctx context.Context, deviceID string, at time.Time) error
	// Heartbeat refreshes liveness for a device.
	Heartbeat(ctx context.Context, deviceID string, at time.Time) error
	// CountAlive counts devices whose last heartbeat is within the liveness TTL.
	CountAlive(ctx context.Context, now time.Time) (int, error)
}

// CacheStore is the Redis hot path for the newest detections per device.
type CacheStore interface {
	// CacheLatestDetections stores the newest detections with a TTL.
	CacheLatestDetections(ctx context.Context, deviceID string, dets []Detection, ttl time.Duration) error
	// GetLatestDetections reads the cached detections, or ErrCacheMiss.
	GetLatestDetections(ctx context.Context, deviceID string) ([]Detection, error)
}

// Store is the complete persistence surface consumed by the service layer.
// HybridStore combines Postgres + Redis; MemStore implements all of it in one
// process for tests and for no-Docker benchmark runs.
type Store interface {
	DetectionStore
	DeviceStore
	CacheStore
}

// HybridStore wires the durable Postgres store to the Redis registry/cache.
// Registering a device writes both; reads prefer Redis and fall back to
// Postgres, repopulating the registry hash on a miss.
type HybridStore struct {
	DB    *PostgresStore
	Cache *RedisStore
}

// NewHybridStore returns a Store backed by Postgres and Redis.
func NewHybridStore(db *PostgresStore, cache *RedisStore) *HybridStore {
	return &HybridStore{DB: db, Cache: cache}
}

// Migrate implements Store.
func (h *HybridStore) Migrate(ctx context.Context) error { return h.DB.Migrate(ctx) }

// EnsurePartition implements Store.
func (h *HybridStore) EnsurePartition(ctx context.Context, day time.Time) error {
	return h.DB.EnsurePartition(ctx, day)
}

// InsertDetections implements Store.
func (h *HybridStore) InsertDetections(ctx context.Context, dets []Detection) error {
	return h.DB.InsertDetections(ctx, dets)
}

// QueryByDevice implements Store.
func (h *HybridStore) QueryByDevice(ctx context.Context, deviceID string, from, to time.Time, limit int) ([]Detection, error) {
	return h.DB.QueryByDevice(ctx, deviceID, from, to, limit)
}

// QueryByLabel implements Store.
func (h *HybridStore) QueryByLabel(ctx context.Context, label string, from, to time.Time, limit int) ([]Detection, error) {
	return h.DB.QueryByLabel(ctx, label, from, to, limit)
}

// RegisterDevice implements Store.
func (h *HybridStore) RegisterDevice(ctx context.Context, d Device) error {
	if err := h.DB.RegisterDevice(ctx, d); err != nil {
		return err
	}
	return h.Cache.PutDevice(ctx, d)
}

// PutDevice implements Store.
func (h *HybridStore) PutDevice(ctx context.Context, d Device) error {
	return h.Cache.PutDevice(ctx, d)
}

// GetDevice implements Store. Redis is authoritative for hot reads; a miss
// falls through to Postgres and warms the hash.
func (h *HybridStore) GetDevice(ctx context.Context, deviceID string) (Device, error) {
	d, err := h.Cache.GetDevice(ctx, deviceID)
	if err == nil {
		return d, nil
	}
	if err != ErrNotFound {
		return Device{}, err
	}
	d, err = h.DB.GetDevice(ctx, deviceID)
	if err != nil {
		return Device{}, err
	}
	_ = h.Cache.PutDevice(ctx, d) // best-effort warm-up
	return d, nil
}

// TouchDevice implements Store.
func (h *HybridStore) TouchDevice(ctx context.Context, deviceID string, at time.Time) error {
	return h.DB.TouchDevice(ctx, deviceID, at)
}

// Heartbeat implements Store.
func (h *HybridStore) Heartbeat(ctx context.Context, deviceID string, at time.Time) error {
	return h.Cache.Heartbeat(ctx, deviceID, at)
}

// CountAlive implements Store.
func (h *HybridStore) CountAlive(ctx context.Context, now time.Time) (int, error) {
	return h.Cache.CountAlive(ctx, now)
}

// CacheLatestDetections implements Store.
func (h *HybridStore) CacheLatestDetections(ctx context.Context, deviceID string, dets []Detection, ttl time.Duration) error {
	return h.Cache.CacheLatestDetections(ctx, deviceID, dets, ttl)
}

// GetLatestDetections implements Store.
func (h *HybridStore) GetLatestDetections(ctx context.Context, deviceID string) ([]Detection, error) {
	return h.Cache.GetLatestDetections(ctx, deviceID)
}

// Close implements Store.
func (h *HybridStore) Close() error {
	err := h.DB.Close()
	if cerr := h.Cache.Close(); err == nil {
		err = cerr
	}
	return err
}
