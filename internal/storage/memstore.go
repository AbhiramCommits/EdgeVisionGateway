package storage

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/obs"
)

// MemStore is a goroutine-safe in-memory Store. It exists so unit tests and
// no-Docker benchmark runs need zero external services, and it deliberately
// mimics the Postgres semantics: half-open time windows, newest-first ordering,
// COPY-equivalent batch inserts and Redis-equivalent TTL expiry.
type MemStore struct {
	mu           sync.RWMutex
	dets         []Detection
	seq          int64
	devices      map[string]Device
	alive        map[string]time.Time
	cache        map[string]memCacheEntry
	now          func() time.Time
	heartbeatTTL time.Duration
	metrics      *obs.Metrics
}

type memCacheEntry struct {
	dets    []Detection
	expires time.Time
}

// MemOption customises a MemStore.
type MemOption func(*MemStore)

// WithClock injects a clock so TTL expiry can be tested without sleeping.
func WithClock(fn func() time.Time) MemOption {
	return func(m *MemStore) {
		if fn != nil {
			m.now = fn
		}
	}
}

// WithHeartbeatTTL sets the liveness window used by Heartbeat and CountAlive.
func WithHeartbeatTTL(d time.Duration) MemOption {
	return func(m *MemStore) {
		if d > 0 {
			m.heartbeatTTL = d
		}
	}
}

// WithMetrics wires cache hit/miss counters into the shared metrics instance.
func WithMetrics(metrics *obs.Metrics) MemOption {
	return func(m *MemStore) { m.metrics = metrics }
}

// NewMemStore returns an empty in-memory store.
func NewMemStore(opts ...MemOption) *MemStore {
	m := &MemStore{
		devices:      make(map[string]Device),
		alive:        make(map[string]time.Time),
		cache:        make(map[string]memCacheEntry),
		now:          func() time.Time { return time.Now().UTC() },
		heartbeatTTL: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Migrate implements Store; nothing to do in memory.
func (m *MemStore) Migrate(context.Context) error { return nil }

// EnsurePartition implements Store; partitions are a Postgres concept.
func (m *MemStore) EnsurePartition(context.Context, time.Time) error { return nil }

// InsertDetections implements Store.
func (m *MemStore) InsertDetections(_ context.Context, dets []Detection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range dets {
		if d.CapturedAt.IsZero() {
			d.CapturedAt = m.now()
		}
		m.seq++
		d.ID = m.seq
		m.dets = append(m.dets, d)
	}
	return nil
}

// QueryByDevice implements Store.
func (m *MemStore) QueryByDevice(_ context.Context, deviceID string, from, to time.Time, limit int) ([]Detection, error) {
	if !from.Before(to) {
		return nil, ErrInvalidRange
	}
	limit = clampLimit(limit)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Detection, 0, limit)
	for _, d := range m.dets {
		if d.DeviceID == deviceID && inWindow(d.CapturedAt, from, to) {
			out = append(out, d)
		}
	}
	return sortAndLimit(out, limit), nil
}

// QueryByLabel implements Store.
func (m *MemStore) QueryByLabel(_ context.Context, label string, from, to time.Time, limit int) ([]Detection, error) {
	if !from.Before(to) {
		return nil, ErrInvalidRange
	}
	limit = clampLimit(limit)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Detection, 0, limit)
	for _, d := range m.dets {
		if d.Label == label && inWindow(d.CapturedAt, from, to) {
			out = append(out, d)
		}
	}
	return sortAndLimit(out, limit), nil
}

// RegisterDevice implements Store.
func (m *MemStore) RegisterDevice(_ context.Context, d Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putDeviceLocked(d)
	return nil
}

// PutDevice implements Store; in memory the registry is the store.
func (m *MemStore) PutDevice(ctx context.Context, d Device) error {
	return m.RegisterDevice(ctx, d)
}

func (m *MemStore) putDeviceLocked(d Device) {
	now := m.now()
	if d.Status == "" {
		d.Status = "active"
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.LastSeenAt.IsZero() {
		d.LastSeenAt = d.CreatedAt
	}
	if old, ok := m.devices[d.DeviceID]; ok {
		d.CreatedAt = old.CreatedAt
		if d.LastSeenAt.Before(old.LastSeenAt) {
			d.LastSeenAt = old.LastSeenAt
		}
	}
	m.devices[d.DeviceID] = d
}

// GetDevice implements Store.
func (m *MemStore) GetDevice(_ context.Context, deviceID string) (Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.devices[deviceID]
	if !ok {
		return Device{}, ErrNotFound
	}
	return d, nil
}

// TouchDevice implements Store.
func (m *MemStore) TouchDevice(_ context.Context, deviceID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[deviceID]
	if !ok {
		return ErrNotFound
	}
	if at.After(d.LastSeenAt) {
		d.LastSeenAt = at
		m.devices[deviceID] = d
	}
	return nil
}

// Heartbeat implements Store.
func (m *MemStore) Heartbeat(_ context.Context, deviceID string, at time.Time) error {
	if at.IsZero() {
		at = m.now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive[deviceID] = at.Add(m.heartbeatTTL)
	return nil
}

// CountAlive implements Store.
func (m *MemStore) CountAlive(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, until := range m.alive {
		if now.Before(until) {
			n++
		} else {
			delete(m.alive, id)
		}
	}
	return n, nil
}

// CacheLatestDetections implements Store. Values are JSON round-tripped so the
// in-memory cache has the same isolation properties as Redis.
func (m *MemStore) CacheLatestDetections(_ context.Context, deviceID string, dets []Detection, ttl time.Duration) error {
	if dets == nil {
		dets = []Detection{}
	}
	b, err := json.Marshal(dets)
	if err != nil {
		return err
	}
	var copyDets []Detection
	if err := json.Unmarshal(b, &copyDets); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache[deviceID] = memCacheEntry{dets: copyDets, expires: m.now().Add(ttl)}
	return nil
}

// GetLatestDetections implements Store.
func (m *MemStore) GetLatestDetections(_ context.Context, deviceID string) ([]Detection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[deviceID]
	if !ok || !m.now().Before(e.expires) {
		delete(m.cache, deviceID)
		if m.metrics != nil {
			m.metrics.IncCacheMiss()
		}
		return nil, ErrCacheMiss
	}
	if m.metrics != nil {
		m.metrics.IncCacheHit()
	}
	out := make([]Detection, len(e.dets))
	copy(out, e.dets)
	return out, nil
}

// Close implements Store.
func (m *MemStore) Close() error { return nil }

// Len reports how many detections are stored. Test helper.
func (m *MemStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.dets)
}

func inWindow(t, from, to time.Time) bool {
	return !t.Before(from) && t.Before(to)
}

func sortAndLimit(in []Detection, limit int) []Detection {
	sort.Slice(in, func(i, j int) bool {
		if in[i].CapturedAt.Equal(in[j].CapturedAt) {
			return in[i].ID > in[j].ID
		}
		return in[i].CapturedAt.After(in[j].CapturedAt)
	})
	if len(in) > limit {
		in = in[:limit]
	}
	return in
}

var _ Store = (*MemStore)(nil)
