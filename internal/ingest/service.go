package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/queue"
	"github.com/edgevision/edgevisiongateway/internal/storage"
)

// ErrShuttingDown is returned by IngestFrame once the service has stopped
// accepting traffic.
var ErrShuttingDown = errors.New("ingest: shutting down")

// RecentTracker remembers the last detection time per device. It feeds the
// low-priority admission class in the queue: a device that just produced a
// detection is likely to produce another near-identical frame, so its frames
// are shed before anyone else's.
type RecentTracker struct {
	mu     sync.Mutex
	last   map[string]time.Time
	window time.Duration
}

// NewRecentTracker returns a tracker whose "recent" window is d.
func NewRecentTracker(d time.Duration) *RecentTracker {
	if d <= 0 {
		d = 500 * time.Millisecond
	}
	return &RecentTracker{last: make(map[string]time.Time), window: d}
}

// Mark records that deviceID produced detections at at.
func (r *RecentTracker) Mark(deviceID string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.last) > 100000 {
		cutoff := at.Add(-10 * r.window)
		for k, v := range r.last {
			if v.Before(cutoff) {
				delete(r.last, k)
			}
		}
	}
	r.last[deviceID] = at
}

// IsRecent reports whether deviceID had a detection within the window.
func (r *RecentTracker) IsRecent(deviceID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.last[deviceID]
	return ok && now.Sub(t) < r.window
}

// ServiceOptions configures the shared service layer.
type ServiceOptions struct {
	Queue       *queue.FrameQueue
	Store       storage.Store
	Metrics     *obs.Metrics
	Recent      *RecentTracker
	CacheWindow time.Duration
	Now         func() time.Time
	Logger      *slog.Logger
}

// Service is the transport-independent application layer used by both REST and
// gRPC. It performs admission control and the cache/database query routing.
type Service struct {
	queue       *queue.FrameQueue
	store       storage.Store
	metrics     *obs.Metrics
	recent      *RecentTracker
	cacheWindow time.Duration
	now         func() time.Time
	log         *slog.Logger
	accepting   atomic.Bool
}

// NewService builds a Service and marks it accepting.
func NewService(o ServiceOptions) *Service {
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Service{
		queue:       o.Queue,
		store:       o.Store,
		metrics:     o.Metrics,
		recent:      o.Recent,
		cacheWindow: o.CacheWindow,
		now:         o.Now,
		log:         o.Logger,
	}
	s.accepting.Store(true)
	return s
}

// IngestOutcome is the admission decision for one frame.
type IngestOutcome struct {
	FrameID    string        `json:"frame_id"`
	QueueDepth int           `json:"queue_depth"`
	RetryAfter time.Duration `json:"-"`
}

// IngestFrame authenticates nothing itself (transports do that and pass the
// verified deviceID) and applies queue admission. A false accepted outcome with
// a nil error means the frame was shed.
func (s *Service) IngestFrame(_ context.Context, deviceID, frameID string, capturedAt time.Time, data []byte, contentType string) (accepted bool, out IngestOutcome, err error) {
	if !s.accepting.Load() {
		return false, IngestOutcome{}, ErrShuttingDown
	}
	now := s.now()
	if capturedAt.IsZero() {
		capturedAt = now
	}
	low := s.recent != nil && s.recent.IsRecent(deviceID, now)
	job := &queue.FrameJob{
		DeviceID:    deviceID,
		FrameID:     frameID,
		CapturedAt:  capturedAt.UTC(),
		EnqueuedAt:  time.Now(),
		Frame:       data,
		ContentType: contentType,
		LowPriority: low,
	}
	out = IngestOutcome{FrameID: frameID, RetryAfter: time.Second}
	if !s.queue.TryEnqueue(job) {
		out.QueueDepth = s.queue.Depth()
		return false, out, nil
	}
	if s.metrics != nil {
		s.metrics.IncFramesReceived()
	}
	out.QueueDepth = s.queue.Depth()
	return true, out, nil
}

// Heartbeat refreshes liveness and the durable last-seen timestamp.
func (s *Service) Heartbeat(ctx context.Context, deviceID string) error {
	if !s.accepting.Load() {
		return ErrShuttingDown
	}
	now := s.now()
	if err := s.store.Heartbeat(ctx, deviceID, now); err != nil {
		return err
	}
	if err := s.store.TouchDevice(ctx, deviceID, now); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("ingest: touch device: %w", err)
	}
	return nil
}

// QueryResult carries detections plus the backend that served them.
type QueryResult struct {
	Detections []storage.Detection `json:"detections"`
	Source     string              `json:"source"`
}

// QueryDevice serves a recent window from the Redis hot cache and older
// windows from Postgres. The cache holds only the newest frame per device, so
// it is used only when that frame falls inside [from, to); otherwise the query
// falls through to Postgres, which has the full history. forceDB skips the
// cache entirely (used by loadgen for unambiguous latency measurement).
func (s *Service) QueryDevice(ctx context.Context, deviceID string, from, to time.Time, limit int, forceDB bool) (QueryResult, error) {
	from, to, err := s.window(from, to)
	if err != nil {
		return QueryResult{}, err
	}
	now := s.now()
	if !forceDB && to.After(now.Add(-s.cacheWindow)) {
		dets, err := s.store.GetLatestDetections(ctx, deviceID)
		switch {
		case err == nil:
			if len(dets) > 0 && !dets[0].CapturedAt.Before(from) {
				return QueryResult{Detections: filterDetections(dets, from, to, limit), Source: "cache"}, nil
			}
		case errors.Is(err, storage.ErrCacheMiss):
			// fall through to Postgres
		default:
			s.log.Warn("hot cache read failed; using postgres", "device_id", deviceID, "err", err)
		}
	}
	dets, err := s.store.QueryByDevice(ctx, deviceID, from, to, limit)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Detections: dets, Source: "db"}, nil
}

// QueryLabel always reads Postgres (label scans are not cached).
func (s *Service) QueryLabel(ctx context.Context, label string, from, to time.Time, limit int) ([]storage.Detection, error) {
	if label == "" {
		return nil, fmt.Errorf("ingest: label is required")
	}
	from, to, err := s.window(from, to)
	if err != nil {
		return nil, err
	}
	return s.store.QueryByLabel(ctx, label, from, to, limit)
}

// StopAccepting flips the service into shutdown mode.
func (s *Service) StopAccepting() { s.accepting.Store(false) }

// Accepting reports whether new frames are currently admitted.
func (s *Service) Accepting() bool { return s.accepting.Load() }

func (s *Service) window(from, to time.Time) (time.Time, time.Time, error) {
	now := s.now()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	if from.IsZero() || !from.Before(to) {
		return time.Time{}, time.Time{}, storage.ErrInvalidRange
	}
	return from, to, nil
}

func filterDetections(dets []storage.Detection, from, to time.Time, limit int) []storage.Detection {
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}
	out := make([]storage.Detection, 0, len(dets))
	for _, d := range dets {
		if d.CapturedAt.Before(from) || !d.CapturedAt.Before(to) {
			continue
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CapturedAt.Equal(out[j].CapturedAt) {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].CapturedAt.After(out[j].CapturedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
