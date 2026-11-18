package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/edgevision/edgevisiongateway/internal/obs"
)

// RedisStore holds the device registry (hashes), the fleet liveness sorted set
// and the per-device latest-detection cache. All keys are namespaced.
type RedisStore struct {
	rdb          *redis.Client
	metrics      *obs.Metrics
	heartbeatTTL time.Duration
}

// NewRedisStoreFromAddr dials addr, authenticates with password (may be empty)
// and pings the server.
func NewRedisStoreFromAddr(ctx context.Context, addr, password string, metrics *obs.Metrics, heartbeatTTL time.Duration) (*RedisStore, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           0,
		PoolSize:     32,
		MinIdleConns: 4,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("storage: ping redis: %w", err)
	}
	return NewRedisStore(rdb, metrics, heartbeatTTL), nil
}

// NewRedisStore wraps an existing client (used by integration tests).
func NewRedisStore(rdb *redis.Client, metrics *obs.Metrics, heartbeatTTL time.Duration) *RedisStore {
	if heartbeatTTL <= 0 {
		heartbeatTTL = 30 * time.Second
	}
	return &RedisStore{rdb: rdb, metrics: metrics, heartbeatTTL: heartbeatTTL}
}

// Client exposes the underlying Redis client for health checks.
func (s *RedisStore) Client() *redis.Client { return s.rdb }

// Ping verifies Redis is reachable.
func (s *RedisStore) Ping(ctx context.Context) error { return s.rdb.Ping(ctx).Err() }

// Close releases the client.
func (s *RedisStore) Close() error { return s.rdb.Close() }

func deviceKey(id string) string { return "device:" + id }
func aliveKey(id string) string  { return "device:" + id + ":alive" }
func latestKey(id string) string { return "det:" + id + ":latest" }

const aliveSetKey = "devices:alive"

// PutDevice writes the device hash. Registry hashes do not expire; liveness is
// tracked separately so a stale hash is still useful for authentication.
func (s *RedisStore) PutDevice(ctx context.Context, d Device) error {
	if d.Status == "" {
		d.Status = "active"
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.LastSeenAt.IsZero() {
		d.LastSeenAt = d.CreatedAt
	}
	err := s.rdb.HSet(ctx, deviceKey(d.DeviceID), map[string]any{
		"device_id":    d.DeviceID,
		"site_id":      d.SiteID,
		"token_hash":   d.TokenHash,
		"status":       d.Status,
		"created_at":   d.CreatedAt.UTC().Format(time.RFC3339Nano),
		"last_seen_at": d.LastSeenAt.UTC().Format(time.RFC3339Nano),
	}).Err()
	if err != nil {
		return fmt.Errorf("storage: put device: %w", err)
	}
	return nil
}

// GetDevice reads the device hash, returning ErrNotFound when absent.
func (s *RedisStore) GetDevice(ctx context.Context, deviceID string) (Device, error) {
	m, err := s.rdb.HGetAll(ctx, deviceKey(deviceID)).Result()
	if err != nil {
		return Device{}, fmt.Errorf("storage: get device: %w", err)
	}
	if len(m) == 0 {
		return Device{}, ErrNotFound
	}
	d := Device{
		DeviceID:  m["device_id"],
		SiteID:    m["site_id"],
		TokenHash: m["token_hash"],
		Status:    m["status"],
	}
	if d.DeviceID == "" {
		d.DeviceID = deviceID
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, m["created_at"])
	d.LastSeenAt, _ = time.Parse(time.RFC3339Nano, m["last_seen_at"])
	return d, nil
}

// Heartbeat marks a device alive: a TTL key for fast existence checks plus a
// score in the devices:alive sorted set for O(log n) fleet counts.
func (s *RedisStore) Heartbeat(ctx context.Context, deviceID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, aliveKey(deviceID), strconv.FormatInt(at.Unix(), 10), s.heartbeatTTL)
	pipe.ZAdd(ctx, aliveSetKey, redis.Z{Score: float64(at.Unix()), Member: deviceID})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("storage: heartbeat: %w", err)
	}
	return nil
}

// CountAlive prunes sorted-set members older than the liveness TTL and returns
// the remaining cardinality. It is O(log n + removed).
func (s *RedisStore) CountAlive(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-s.heartbeatTTL).Unix()
	pipe := s.rdb.TxPipeline()
	pipe.ZRemRangeByScore(ctx, aliveSetKey, "-inf", strconv.FormatInt(cutoff, 10))
	card := pipe.ZCard(ctx, aliveSetKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("storage: count alive: %w", err)
	}
	return int(card.Val()), nil
}

// CacheLatestDetections JSON-encodes dets into det:{device}:latest with a TTL.
func (s *RedisStore) CacheLatestDetections(ctx context.Context, deviceID string, dets []Detection, ttl time.Duration) error {
	if dets == nil {
		dets = []Detection{}
	}
	b, err := json.Marshal(dets)
	if err != nil {
		return fmt.Errorf("storage: marshal cache: %w", err)
	}
	if err := s.rdb.Set(ctx, latestKey(deviceID), b, ttl).Err(); err != nil {
		return fmt.Errorf("storage: set cache: %w", err)
	}
	return nil
}

// GetLatestDetections reads the cache. A missing key is ErrCacheMiss and is
// counted as a miss in obs.Metrics; every successful read counts as a hit.
func (s *RedisStore) GetLatestDetections(ctx context.Context, deviceID string) ([]Detection, error) {
	b, err := s.rdb.Get(ctx, latestKey(deviceID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			if s.metrics != nil {
				s.metrics.IncCacheMiss()
			}
			return nil, ErrCacheMiss
		}
		return nil, fmt.Errorf("storage: get cache: %w", err)
	}
	var dets []Detection
	if err := json.Unmarshal(b, &dets); err != nil {
		return nil, fmt.Errorf("storage: decode cache: %w", err)
	}
	if s.metrics != nil {
		s.metrics.IncCacheHit()
	}
	return dets, nil
}
