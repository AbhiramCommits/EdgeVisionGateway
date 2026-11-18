// Command loadgen simulates a fleet of edge cameras against a running gateway.
//
// Each simulated device owns a token, sends pre-encoded synthetic JPEG frames
// on a jittered schedule, and records the outcome (202/429/error) plus
// client-observed upload latency. A sampled subset of frames is queried back
// through GET /v1/devices/{id}/detections to measure true
// ingest → queryable latency.
//
// The target gateway must run with EVG_AUTO_REGISTER=true (or have its devices
// pre-registered) because loadgen mints tokens from the shared secret.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/jpeg"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/edgevision/edgevisiongateway/internal/ingest"
	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/pb"
	"github.com/edgevision/edgevisiongateway/internal/storage"
	"github.com/edgevision/edgevisiongateway/internal/vision"
)

type options struct {
	devices         int
	fpsPerDevice    float64
	duration        time.Duration
	target          string
	httpAddr        string
	grpcAddr        string
	imageSize       string
	burstAt         time.Duration
	burstMultiplier float64
	burstDuration   time.Duration
	sampleEvery     int
	sampleStride    int
	pollInterval    time.Duration
	maxInflight     int
	seed            int64
	secret          string
	out             string
	scenario        string
	workers         int
	queryTimeout    time.Duration
}

type report struct {
	Scenario        string           `json:"scenario"`
	Target          string           `json:"target"`
	Devices         int              `json:"devices"`
	FPSPerDevice    float64          `json:"fps_per_device"`
	OfferedFPS      float64          `json:"offered_fps"`
	DurationSeconds float64          `json:"duration_seconds"`
	ElapsedSeconds  float64          `json:"elapsed_seconds"`
	BurstAtSeconds  float64          `json:"burst_at_seconds"`
	BurstMultiplier float64          `json:"burst_multiplier"`
	BurstSeconds    float64          `json:"burst_duration_seconds"`
	Requested       int64            `json:"requested"`
	Accepted        int64            `json:"accepted"`
	Shed            int64            `json:"shed"`
	Errors          int64            `json:"errors"`
	AcceptedFPS     float64          `json:"accepted_fps"`
	ShedPct         float64          `json:"shed_pct"`
	ErrorPct        float64          `json:"error_pct"`
	UploadLatency   obs.LatencyStats `json:"upload_latency_ms"`
	E2ESamples      int64            `json:"e2e_samples"`
	E2ESkipped      int64            `json:"e2e_skipped"`
	E2ELatency      obs.LatencyStats `json:"e2e_latency_ms"`
	FramePoolSize   int              `json:"frame_pool_size"`
	FrameBytes      int              `json:"frame_bytes"`
	SampleStride    int              `json:"sample_device_stride"`
	PollIntervalMS  float64          `json:"poll_interval_ms"`
	ErrorBreakdown  map[string]int64 `json:"error_breakdown"`
	CPUCount        int              `json:"loadgen_cpu_count"`
	GatewayWorkers  int              `json:"gateway_workers"`
}

type runner struct {
	opts      options
	frames    [][]byte
	http      *http.Client
	grpcConn  *grpc.ClientConn
	start     time.Time
	requested atomic.Int64
	accepted  atomic.Int64
	shed      atomic.Int64
	errs      atomic.Int64
	upload    *obs.Metrics
	e2e       *obs.Metrics
	e2eOK     atomic.Int64
	e2eSkip   atomic.Int64
	sampleWG  sync.WaitGroup
	sem       chan struct{}
	errMu     sync.Mutex
	errKinds  map[string]int64
}

func main() {
	var o options
	flag.IntVar(&o.devices, "devices", 500, "number of simulated devices")
	flag.Float64Var(&o.fpsPerDevice, "fps-per-device", 2, "frames per second per device outside bursts")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "run duration")
	flag.StringVar(&o.target, "target", "http", "transport: http or grpc")
	flag.StringVar(&o.httpAddr, "http-addr", "http://localhost:8080", "gateway REST base URL (also used for latency read-back)")
	flag.StringVar(&o.grpcAddr, "grpc-addr", "localhost:9090", "gateway gRPC address")
	flag.StringVar(&o.imageSize, "image-size", "640x480", "frame WxH")
	flag.DurationVar(&o.burstAt, "burst-at", 0, "start of the burst relative to run start (0 disables)")
	flag.Float64Var(&o.burstMultiplier, "burst-multiplier", 5, "rate multiplier during the burst")
	flag.DurationVar(&o.burstDuration, "burst-duration", 15*time.Second, "length of the burst")
	flag.IntVar(&o.sampleEvery, "sample-every", 10, "query back 1 frame out of N from sampled devices (0 disables)")
	flag.IntVar(&o.sampleStride, "sample-device-stride", 50, "only every Nth device performs latency read-backs")
	flag.DurationVar(&o.pollInterval, "poll-interval", 100*time.Millisecond, "polling interval for a sampled frame")
	flag.IntVar(&o.maxInflight, "max-inflight-queries", 16, "cap on concurrent latency read-backs")
	flag.Int64Var(&o.seed, "seed", 7, "frame generator seed")
	flag.StringVar(&o.secret, "secret", envOr("EVG_DEVICE_SECRET", "dev-secret-change-me"), "token HMAC secret")
	flag.StringVar(&o.out, "out", "docs/results/raw/loadgen.json", "JSON report path")
	flag.StringVar(&o.scenario, "scenario", "adhoc", "scenario label stored in the report")
	flag.IntVar(&o.workers, "gateway-workers", 0, "gateway worker count (recorded only, for the report)")
	flag.DurationVar(&o.queryTimeout, "query-timeout", 5*time.Second, "max wait for a sampled frame to become queryable")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(o options) error {
	if o.devices <= 0 || o.fpsPerDevice <= 0 || o.duration <= 0 {
		return fmt.Errorf("devices, fps-per-device and duration must be positive")
	}
	if o.target != "http" && o.target != "grpc" {
		return fmt.Errorf("target must be http or grpc")
	}
	width, height, err := parseSize(o.imageSize)
	if err != nil {
		return err
	}
	frames, err := buildFramePool(width, height, o.seed)
	if err != nil {
		return err
	}
	r := &runner{
		opts:     o,
		frames:   frames,
		upload:   obs.New(),
		e2e:      obs.New(),
		errKinds: make(map[string]int64),
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        8192,
				MaxIdleConnsPerHost: 8192,
				MaxConnsPerHost:     0,
				DisableCompression:  true,
				WriteBufferSize:     32 << 10,
			},
		},
	}
	if o.maxInflight > 0 {
		r.sem = make(chan struct{}, o.maxInflight)
	}
	if o.target == "grpc" {
		conn, err := grpc.NewClient(o.grpcAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(16<<20)),
		)
		if err != nil {
			return fmt.Errorf("grpc dial: %w", err)
		}
		r.grpcConn = conn
		defer conn.Close()
	}
	defer r.http.CloseIdleConnections()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r.start = time.Now()
	var wg sync.WaitGroup
	for i := 0; i < o.devices; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r.device(ctx, idx)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(r.start)
	// Sampled read-backs can legitimately finish after the sending window when
	// the queue is backed up; they do not extend the measured offered window.
	r.sampleWG.Wait()

	req := r.requested.Load()
	acc := r.accepted.Load()
	shed := r.shed.Load()
	errs := r.errs.Load()
	uploadSnap := r.upload.Snapshot()
	e2eSnap := r.e2e.Snapshot()
	rep := report{
		Scenario:        o.scenario,
		Target:          o.target,
		Devices:         o.devices,
		FPSPerDevice:    o.fpsPerDevice,
		OfferedFPS:      float64(o.devices) * o.fpsPerDevice,
		DurationSeconds: o.duration.Seconds(),
		ElapsedSeconds:  elapsed.Seconds(),
		Requested:       req,
		Accepted:        acc,
		Shed:            shed,
		Errors:          errs,
		AcceptedFPS:     float64(acc) / elapsed.Seconds(),
		UploadLatency:   uploadSnap.Latency,
		E2ESamples:      r.e2eOK.Load(),
		E2ESkipped:      r.e2eSkip.Load(),
		E2ELatency:      e2eSnap.Latency,
		FramePoolSize:   len(frames),
		FrameBytes:      len(frames[0]),
		SampleStride:    o.sampleStride,
		PollIntervalMS:  float64(o.pollInterval.Microseconds()) / 1000.0,
		ErrorBreakdown:  r.errKinds,
		CPUCount:        numCPU(),
		GatewayWorkers:  o.workers,
	}
	if o.burstAt > 0 {
		rep.BurstAtSeconds = o.burstAt.Seconds()
		rep.BurstMultiplier = o.burstMultiplier
		rep.BurstSeconds = o.burstDuration.Seconds()
	}
	if req > 0 {
		rep.ShedPct = 100 * float64(shed) / float64(req)
		rep.ErrorPct = 100 * float64(errs) / float64(req)
	}
	if err := writeReport(o.out, rep); err != nil {
		return err
	}
	fmt.Printf("scenario=%s target=%s offered=%.0ffps accepted=%d shed=%d(%.1f%%) errors=%d accepted_fps=%.0f p50=%.1fms p95=%.1fms p99=%.1fms e2e_n=%d e2e_p50=%.1fms e2e_p95=%.1fms e2e_p99=%.1fms\n",
		rep.Scenario, rep.Target, rep.OfferedFPS, rep.Accepted, rep.Shed, rep.ShedPct, rep.Errors,
		rep.AcceptedFPS, rep.UploadLatency.P50MS, rep.UploadLatency.P95MS, rep.UploadLatency.P99MS,
		rep.E2ESamples, rep.E2ELatency.P50MS, rep.E2ELatency.P95MS, rep.E2ELatency.P99MS)
	return nil
}

func (r *runner) device(ctx context.Context, idx int) {
	deviceID := fmt.Sprintf("dev-%05d", idx)
	token := ingest.IssueToken(deviceID, r.opts.secret)
	rng := rand.New(rand.NewSource(int64(idx) + 1))
	base := time.Duration(float64(time.Second) / r.opts.fpsPerDevice)
	// Spread the fleet's first sends across one base interval.
	startDelay := time.Duration(rng.Float64() * float64(base))
	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}
	seq := 0
	frameIdx := idx % len(r.frames)
	lastHeartbeat := time.Time{}
	for {
		now := time.Now()
		elapsed := now.Sub(r.start)
		if elapsed >= r.opts.duration {
			return
		}
		if lastHeartbeat.IsZero() || now.Sub(lastHeartbeat) >= 20*time.Second {
			r.heartbeat(ctx, deviceID, token)
			lastHeartbeat = now
		}
		interval := base
		if r.opts.burstAt > 0 && elapsed >= r.opts.burstAt && elapsed < r.opts.burstAt+r.opts.burstDuration {
			interval = time.Duration(float64(base) / r.opts.burstMultiplier)
			if interval < time.Millisecond {
				interval = time.Millisecond
			}
		}
		jitter := time.Duration((rng.Float64()*0.2 - 0.1) * float64(interval))
		wait := interval + jitter
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		seq++
		frameID := fmt.Sprintf("%s-%06d", deviceID, seq)
		sample := r.opts.sampleEvery > 0 && seq%r.opts.sampleEvery == 0 &&
			(idx%r.opts.sampleStride == 0 || r.opts.sampleStride <= 1)
		// Wait before sending so the first frame goes out immediately.
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if time.Since(r.start) >= r.opts.duration {
			return
		}
		sentAt := time.Now()
		r.send(ctx, deviceID, token, frameID, r.frames[frameIdx], sentAt, sample)
		frameIdx = (frameIdx + 1) % len(r.frames)
	}
}

func (r *runner) send(ctx context.Context, deviceID, token, frameID string, frame []byte, sentAt time.Time, sample bool) {
	r.requested.Add(1)
	start := time.Now()
	var outcome int
	var errKind string
	switch r.opts.target {
	case "grpc":
		outcome, errKind = r.sendGRPC(ctx, deviceID, token, frameID, frame, sentAt)
	default:
		outcome, errKind = r.sendHTTP(ctx, deviceID, token, frameID, frame, sentAt)
	}
	r.upload.RecordLatency(time.Since(start))
	switch outcome {
	case 202:
		r.accepted.Add(1)
		if sample {
			// Measure off the send path and cap in-flight read-backs so the
			// measurement itself cannot dominate the benchmark's query load.
			acquired := true
			if r.sem != nil {
				select {
				case r.sem <- struct{}{}:
				default:
					acquired = false
					r.e2eSkip.Add(1)
				}
			}
			if acquired {
				r.sampleWG.Add(1)
				go func() {
					defer r.sampleWG.Done()
					if r.sem != nil {
						defer func() { <-r.sem }()
					}
					r.measureQueryable(ctx, deviceID, token, frameID, sentAt)
				}()
			}
		}
	case 429:
		r.shed.Add(1)
	default:
		r.errs.Add(1)
		r.recordError(errKind)
	}
}

func (r *runner) recordError(kind string) {
	if kind == "" {
		kind = "unknown"
	}
	r.errMu.Lock()
	r.errKinds[kind]++
	r.errMu.Unlock()
}

func (r *runner) sendHTTP(ctx context.Context, deviceID, token, frameID string, frame []byte, sentAt time.Time) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.opts.httpAddr, "/")+"/v1/frames", bytes.NewReader(frame))
	if err != nil {
		return -1, "request_build"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("X-Device-ID", deviceID)
	req.Header.Set("X-Frame-ID", frameID)
	req.Header.Set("X-Captured-At", sentAt.UTC().Format(time.RFC3339Nano))
	resp, err := r.http.Do(req)
	if err != nil {
		return -1, "transport"
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == 202 || resp.StatusCode == 429 {
		return resp.StatusCode, ""
	}
	return resp.StatusCode, fmt.Sprintf("http_%d", resp.StatusCode)
}

func (r *runner) sendGRPC(ctx context.Context, deviceID, token, frameID string, frame []byte, sentAt time.Time) (int, string) {
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	_, err := pb.NewGatewayClient(r.grpcConn).IngestFrame(ctx, &pb.IngestFrameRequest{
		DeviceId:    deviceID,
		FrameId:     frameID,
		CapturedAt:  timestamppb.New(sentAt),
		Frame:       frame,
		ContentType: "image/jpeg",
	})
	if err == nil {
		return 202, ""
	}
	if status.Code(err) == codes.ResourceExhausted {
		return 429, ""
	}
	return -1, "grpc_" + status.Code(err).String()
}

func (r *runner) heartbeat(ctx context.Context, deviceID, token string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.opts.httpAddr, "/")+"/v1/heartbeat", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.http.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// measureQueryable polls the device query API until the sampled frame appears
// in Postgres, then records ingest → queryable latency. The read-back forces
// the DB path and uses a narrow window around the send time so it identifies
// this exact frame even when newer frames have replaced the Redis hot cache.
func (r *runner) measureQueryable(ctx context.Context, deviceID, token, frameID string, sentAt time.Time) {
	deadline := time.Now().Add(r.opts.queryTimeout)
	from := sentAt.Add(-100 * time.Millisecond).UTC()
	to := sentAt.Add(100 * time.Millisecond).UTC()
	for time.Now().Before(deadline) {
		dets, err := r.queryLatest(ctx, deviceID, token, from, to)
		if err == nil {
			for _, d := range dets {
				if d.FrameID == frameID {
					r.e2e.RecordLatency(time.Since(sentAt))
					r.e2eOK.Add(1)
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.opts.pollInterval):
		}
	}
	r.e2eSkip.Add(1)
}

func (r *runner) queryLatest(ctx context.Context, deviceID, token string, from, to time.Time) ([]storage.Detection, error) {
	url := fmt.Sprintf("%s/v1/devices/%s/detections?from=%s&to=%s&limit=500&source=db",
		strings.TrimRight(r.opts.httpAddr, "/"),
		deviceID,
		from.Format(time.RFC3339Nano),
		to.Format(time.RFC3339Nano))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("query status %d", resp.StatusCode)
	}
	var body struct {
		Detections []storage.Detection `json:"detections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Detections, nil
}

func buildFramePool(width, height int, seed int64) ([][]byte, error) {
	// 18 post-warm-up frames of moving objects, each with distinct positions.
	imgs, _ := vision.GenerateSequence(seed, 30, width, height, 4, 5)
	out := make([][]byte, 0, 18)
	for i := 12; i < 30; i++ {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, imgs[i], &jpeg.Options{Quality: 85}); err != nil {
			return nil, fmt.Errorf("encode frame: %w", err)
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}

func parseSize(s string) (int, int, error) {
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil {
		return 0, 0, fmt.Errorf("image-size must be WxH, got %q", s)
	}
	if w < 64 || h < 64 || w > 4096 || h > 4096 {
		return 0, 0, fmt.Errorf("image-size out of range: %dx%d", w, h)
	}
	return w, h, nil
}

func numCPU() int { return runtime.NumCPU() }

func writeReport(path string, rep report) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
