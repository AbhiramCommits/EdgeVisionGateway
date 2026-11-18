package queue

import (
	"sync"
	"testing"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/obs"
)

func job(device string, low bool) *FrameJob {
	return &FrameJob{DeviceID: device, FrameID: "f1", EnqueuedAt: time.Now(), LowPriority: low}
}

func TestCapacityAndShedding(t *testing.T) {
	m := obs.New()
	q := New(2, 1.0, m)
	if !q.TryEnqueue(job("a", false)) || !q.TryEnqueue(job("b", false)) {
		t.Fatal("first two enqueues should be accepted")
	}
	if q.Depth() != 2 {
		t.Fatalf("depth = %d, want 2", q.Depth())
	}
	if q.TryEnqueue(job("c", false)) {
		t.Fatal("third enqueue should be shed")
	}
	if q.Depth() != 2 {
		t.Fatalf("depth after shed = %d, want 2", q.Depth())
	}
	if got := q.HighWaterMark(); got != 2 {
		t.Errorf("high water mark = %d, want 2", got)
	}
	if got := m.Snapshot().FramesShed; got != 1 {
		t.Errorf("frames_shed = %d, want 1", got)
	}
	if got := q.ShedHigh(); got != 1 {
		t.Errorf("shedHigh = %d, want 1", got)
	}
}

func TestLowPriorityShedBeforeHighPriority(t *testing.T) {
	m := obs.New()
	q := New(10, 0.5, m) // threshold at depth 5
	for i := 0; i < 5; i++ {
		if !q.TryEnqueue(job("hi", false)) {
			t.Fatalf("high priority enqueue %d rejected", i)
		}
	}
	if q.TryEnqueue(job("low", true)) {
		t.Fatal("low-priority frame should be shed at threshold")
	}
	if got := q.ShedLow(); got != 1 {
		t.Errorf("shedLow = %d, want 1", got)
	}
	// High-priority frames are still admitted until the hard capacity.
	for i := 0; i < 5; i++ {
		if !q.TryEnqueue(job("hi", false)) {
			t.Fatalf("high priority enqueue above threshold %d rejected", i)
		}
	}
	if q.Depth() != 10 {
		t.Fatalf("depth = %d, want 10", q.Depth())
	}
	if q.TryEnqueue(job("hi", false)) {
		t.Fatal("full queue must reject high-priority frames")
	}
}

func TestStopAccepting(t *testing.T) {
	q := New(4, 0.85, obs.New())
	q.StopAccepting()
	if q.TryEnqueue(job("a", false)) {
		t.Fatal("enqueue after StopAccepting should fail")
	}
	select {
	case <-q.Done():
	default:
		t.Fatal("Done() should be closed after StopAccepting")
	}
	q.StopAccepting() // idempotent
}

func TestConcurrentEnqueueNoLoss(t *testing.T) {
	const total = 2000
	m := obs.New()
	q := New(256, 0.85, m)
	var accepted, rejected int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok := q.TryEnqueue(job("device", i%3 == 0))
			mu.Lock()
			if ok {
				accepted++
			} else {
				rejected++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if accepted+rejected != total {
		t.Fatalf("accepted+rejected = %d, want %d", accepted+rejected, total)
	}
	if rejected != int(m.Snapshot().FramesShed) {
		t.Errorf("rejected = %d, but metrics frames_shed = %d", rejected, m.Snapshot().FramesShed)
	}
	if accepted != q.Depth() {
		t.Errorf("accepted = %d, queued depth = %d (no consumer, so they must match)", accepted, q.Depth())
	}
	if q.Depth() > q.Capacity() {
		t.Errorf("depth %d exceeds capacity %d", q.Depth(), q.Capacity())
	}
	if q.HighWaterMark() > q.Capacity() {
		t.Errorf("high water mark %d exceeds capacity %d", q.HighWaterMark(), q.Capacity())
	}
	if q.ShedLow()+q.ShedHigh() != int64(rejected) {
		t.Errorf("low+high shed = %d, rejected = %d", q.ShedLow()+q.ShedHigh(), rejected)
	}
}
