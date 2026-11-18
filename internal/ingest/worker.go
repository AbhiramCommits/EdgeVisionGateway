package ingest

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder
	_ "image/png"  // register PNG decoder
	"log/slog"
	"sync"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/queue"
	"github.com/edgevision/edgevisiongateway/internal/storage"
	"github.com/edgevision/edgevisiongateway/internal/vision"
)

// PoolOptions configures the detection worker pool.
type PoolOptions struct {
	Queue         *queue.FrameQueue
	Store         storage.Store
	Metrics       *obs.Metrics
	Recent        *RecentTracker
	WorkerCount   int
	NewDetector   func() vision.Detector
	CacheTTL      time.Duration
	BatchMax      int
	FlushInterval time.Duration
	Logger        *slog.Logger
}

// Pool runs WorkerCount goroutines, each with its own detector (and therefore
// its own background model). Frames are decoded, detected, batched and written
// with a COPY-style batch insert; the per-device hot cache is refreshed after
// the batch is durable, and end-to-end latency (enqueue → persisted) is
// recorded in obs.Metrics.
type Pool struct {
	opts  PoolOptions
	wg    sync.WaitGroup
	start sync.Once
}

type pendingFrame struct {
	deviceID   string
	enqueuedAt time.Time
	dets       []storage.Detection
}

// NewPool validates options and returns a stopped pool.
func NewPool(o PoolOptions) *Pool {
	if o.WorkerCount <= 0 {
		o.WorkerCount = 1
	}
	if o.BatchMax <= 0 {
		o.BatchMax = 100
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 100 * time.Millisecond
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.NewDetector == nil {
		o.NewDetector = func() vision.Detector { return vision.NewMotionDetector() }
	}
	return &Pool{opts: o}
}

// Start launches the workers. It is idempotent.
func (p *Pool) Start() {
	p.start.Do(func() {
		for i := 0; i < p.opts.WorkerCount; i++ {
			p.wg.Add(1)
			go p.worker(i)
		}
	})
}

// Shutdown stops admission, drains the queue, flushes pending batches and waits
// for workers. It returns ctx.Err() if the deadline expires first.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.opts.Queue.StopAccepting()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	det := p.opts.NewDetector()
	if det == nil {
		det = vision.NewMotionDetector()
	}
	batch := make([]pendingFrame, 0, p.opts.BatchMax)
	ticker := time.NewTicker(p.opts.FlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.flush(batch)
		batch = batch[:0]
	}
	process := func(job *queue.FrameJob) {
		pf, ok := p.detect(job, det)
		if !ok {
			return
		}
		batch = append(batch, pf)
		if len(batch) >= p.opts.BatchMax {
			flush()
		}
	}

	for {
		select {
		case job := <-p.opts.Queue.Jobs():
			process(job)
		case <-ticker.C:
			flush()
		case <-p.opts.Queue.Done():
			// Admission is closed and no producer can add more work, so drain
			// whatever is buffered and exit.
			for {
				select {
				case job := <-p.opts.Queue.Jobs():
					process(job)
				default:
					flush()
					return
				}
			}
		}
	}
}

// detect decodes one frame and runs the detector. A panic inside a detector is
// converted into a dropped frame rather than killing the worker.
func (p *Pool) detect(job *queue.FrameJob, det vision.Detector) (pf pendingFrame, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			p.opts.Logger.Error("detector panic", "worker_error", fmt.Sprint(r), "device_id", job.DeviceID)
			if p.opts.Metrics != nil {
				p.opts.Metrics.IncDetectErrors()
			}
			ok = false
		}
	}()
	img, _, err := image.Decode(bytes.NewReader(job.Frame))
	if err != nil {
		p.opts.Logger.Warn("frame decode failed", "device_id", job.DeviceID, "frame_id", job.FrameID, "err", err)
		if p.opts.Metrics != nil {
			p.opts.Metrics.IncDetectErrors()
		}
		return pendingFrame{}, false
	}
	preds, err := det.Detect(img)
	if err != nil {
		p.opts.Logger.Warn("detection failed", "device_id", job.DeviceID, "frame_id", job.FrameID, "err", err)
		if p.opts.Metrics != nil {
			p.opts.Metrics.IncDetectErrors()
		}
		return pendingFrame{}, false
	}
	dets := make([]storage.Detection, 0, len(preds))
	for _, d := range preds {
		dets = append(dets, storage.Detection{
			DeviceID:   job.DeviceID,
			FrameID:    job.FrameID,
			CapturedAt: job.CapturedAt,
			Label:      d.Label,
			Confidence: d.Confidence,
			X:          d.Box.X,
			Y:          d.Box.Y,
			W:          d.Box.W,
			H:          d.Box.H,
		})
	}
	return pendingFrame{deviceID: job.DeviceID, enqueuedAt: job.EnqueuedAt, dets: dets}, true
}

// flush persists a batch, then refreshes caches and records latency.
func (p *Pool) flush(batch []pendingFrame) {
	all := make([]storage.Detection, 0, len(batch)*4)
	for _, pf := range batch {
		all = append(all, pf.dets...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if len(all) > 0 {
		if err := p.opts.Store.InsertDetections(ctx, all); err != nil {
			p.opts.Logger.Error("persist detections failed", "count", len(all), "err", err)
			if p.opts.Metrics != nil {
				p.opts.Metrics.IncDetectErrors()
			}
			return
		}
	}
	persistedAt := time.Now()
	for _, pf := range batch {
		if p.opts.Metrics != nil {
			p.opts.Metrics.IncFramesProcessed()
			p.opts.Metrics.IncDetectionsWritten(len(pf.dets))
			p.opts.Metrics.RecordLatency(persistedAt.Sub(pf.enqueuedAt))
		}
		if p.opts.Recent != nil {
			p.opts.Recent.Mark(pf.deviceID, persistedAt)
		}
		if err := p.opts.Store.CacheLatestDetections(ctx, pf.deviceID, pf.dets, p.opts.CacheTTL); err != nil {
			p.opts.Logger.Warn("cache write failed", "device_id", pf.deviceID, "err", err)
		}
	}
}
