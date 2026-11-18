// Package queue implements the bounded frame queue with load shedding.
package queue

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/obs"
)

// FrameJob is one frame awaiting detection.
type FrameJob struct {
	DeviceID    string
	FrameID     string
	CapturedAt  time.Time
	EnqueuedAt  time.Time
	Frame       []byte
	ContentType string
	// LowPriority marks frames from devices that produced a detection within
	// the recent-detection window. They are shed first.
	LowPriority bool
}

// FrameQueue is a fixed-capacity channel plus admission control.
//
// Shedding policy: at or above ShedThreshold*Capacity, low-priority frames are
// rejected immediately, preserving queue space for frames from devices that
// have not been seen recently (whose content is less likely to be duplicated in
// the hot cache). Once the channel itself is full, any frame is rejected: low
// priority frames first, but high-priority frames can no longer be admitted
// either. Rejections are counted in obs.Metrics.IncFramesShed and returned to
// the caller as false so the transport can answer 429 / ResourceExhausted.
type FrameQueue struct {
	mu            sync.Mutex
	ch            chan *FrameJob
	closed        bool
	done          chan struct{}
	capacity      int
	shedThreshold float64
	metrics       *obs.Metrics
	hwm           atomic.Int64
	shedLow       atomic.Int64
	shedHigh      atomic.Int64
}

// New creates a queue. capacity <= 0 panics because it is a programming error;
// threshold outside (0,1] is clamped to (0,1].
func New(capacity int, shedThreshold float64, metrics *obs.Metrics) *FrameQueue {
	if capacity <= 0 {
		panic("queue: capacity must be > 0")
	}
	if shedThreshold <= 0 || shedThreshold > 1 {
		shedThreshold = 0.85
	}
	return &FrameQueue{
		ch:            make(chan *FrameJob, capacity),
		done:          make(chan struct{}),
		capacity:      capacity,
		shedThreshold: shedThreshold,
		metrics:       metrics,
	}
}

// TryEnqueue admits j without blocking. It returns false when the frame was
// shed (low-priority early rejection, full queue, or shutdown).
func (q *FrameQueue) TryEnqueue(j *FrameJob) bool {
	if j == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.countShed(j)
		return false
	}
	limit := int(math.Ceil(q.shedThreshold * float64(q.capacity)))
	if j.LowPriority && len(q.ch) >= limit {
		q.countShed(j)
		return false
	}
	select {
	case q.ch <- j:
		q.raiseHighWater(len(q.ch))
		return true
	default:
		q.countShed(j)
		return false
	}
}

func (q *FrameQueue) countShed(j *FrameJob) {
	if j.LowPriority {
		q.shedLow.Add(1)
	} else {
		q.shedHigh.Add(1)
	}
	if q.metrics != nil {
		q.metrics.IncFramesShed()
	}
}

func (q *FrameQueue) raiseHighWater(depth int) {
	for {
		cur := q.hwm.Load()
		if int64(depth) <= cur || q.hwm.CompareAndSwap(cur, int64(depth)) {
			return
		}
	}
}

// Jobs is the receive side consumed by workers.
func (q *FrameQueue) Jobs() <-chan *FrameJob { return q.ch }

// Done is closed when the queue stops accepting frames, signalling workers to
// drain the remaining backlog and exit.
func (q *FrameQueue) Done() <-chan struct{} { return q.done }

// StopAccepting makes every subsequent TryEnqueue fail and tells workers to
// drain. It is idempotent.
func (q *FrameQueue) StopAccepting() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.done)
	}
	q.mu.Unlock()
}

// Depth is the current number of queued frames.
func (q *FrameQueue) Depth() int { return len(q.ch) }

// Capacity is the fixed maximum depth.
func (q *FrameQueue) Capacity() int { return q.capacity }

// HighWaterMark is the largest depth observed since creation.
func (q *FrameQueue) HighWaterMark() int { return int(q.hwm.Load()) }

// ShedLow returns how many low-priority frames were rejected. It exists for
// tests and benchmark reporting.
func (q *FrameQueue) ShedLow() int64 { return q.shedLow.Load() }

// ShedHigh returns how many high-priority frames were rejected (hard capacity
// or shutdown). It exists for tests and benchmark reporting.
func (q *FrameQueue) ShedHigh() int64 { return q.shedHigh.Load() }
