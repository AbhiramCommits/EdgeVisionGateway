package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/ingest"
	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/queue"
	"github.com/edgevision/edgevisiongateway/internal/storage"
)

type apiFixture struct {
	handler http.Handler
	store   *storage.MemStore
	metrics *obs.Metrics
	queue   *queue.FrameQueue
	svc     *ingest.Service
	token   string
}

func newAPIFixture(t *testing.T, capacity int, threshold float64) *apiFixture {
	t.Helper()
	metrics := obs.New()
	store := storage.NewMemStore(storage.WithMetrics(metrics), storage.WithHeartbeatTTL(time.Minute))
	if err := store.RegisterDevice(context.Background(), storage.Device{DeviceID: "cam-1", SiteID: "site-a"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	q := queue.New(capacity, threshold, metrics)
	svc := ingest.NewService(ingest.ServiceOptions{
		Queue:       q,
		Store:       store,
		Metrics:     metrics,
		Recent:      ingest.NewRecentTracker(500 * time.Millisecond),
		CacheWindow: 2 * time.Minute,
	})
	auth := ingest.NewAuthenticator("test-secret", store)
	return &apiFixture{
		handler: NewHTTPHandler(HTTPOptions{
			Service: svc, Auth: auth, Store: store, Metrics: metrics, Queue: q,
		}),
		store: store, metrics: metrics, queue: q, svc: svc,
		token: ingest.IssueToken("cam-1", "test-secret"),
	}
}

func (f *apiFixture) do(method, path string, body []byte, token string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

func TestIngestAccepted(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	rec := f.do("POST", "/v1/frames", []byte("jpeg-bytes"), f.token, map[string]string{
		"Content-Type": "image/jpeg", "X-Frame-ID": "frame-1",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if body["status"] != "queued" || body["frame_id"] != "frame-1" {
		t.Errorf("body = %v", body)
	}
	if got := f.metrics.Snapshot().FramesReceived; got != 1 {
		t.Errorf("frames_received = %d, want 1", got)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID")
	}
}

func TestIngestAuthFailures(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	cases := []struct {
		name  string
		token string
	}{
		{"missing token", ""},
		{"malformed token", "not-a-token"},
		{"token for unknown device", ingest.IssueToken("ghost", "test-secret")},
		{"wrong secret", ingest.IssueToken("cam-1", "wrong")},
	}
	for _, c := range cases {
		rec := f.do("POST", "/v1/frames", []byte("x"), c.token, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", c.name, rec.Code)
		}
	}
	if got := f.metrics.Snapshot().AuthFailures; got != int64(len(cases)) {
		t.Errorf("auth_failures = %d, want %d", got, len(cases))
	}

	rec := f.do("POST", "/v1/frames", []byte("x"), f.token, map[string]string{"X-Device-ID": "cam-2"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("device mismatch: status = %d, want 401", rec.Code)
	}
}

func TestIngestShedReturns429(t *testing.T) {
	f := newAPIFixture(t, 1, 1.0)
	first := f.do("POST", "/v1/frames", []byte("x"), f.token, nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.Code)
	}
	second := f.do("POST", "/v1/frames", []byte("x"), f.token, nil)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body=%s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if got := f.metrics.Snapshot().FramesShed; got != 1 {
		t.Errorf("frames_shed = %d, want 1", got)
	}
}

func TestIngestWhileShuttingDown(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	f.svc.StopAccepting()
	rec := f.do("POST", "/v1/frames", []byte("x"), f.token, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestIngestMultipart(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("frame", "frame.jpg")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	part.Write([]byte("jpeg-bytes"))
	mw.Close()
	rec := f.do("POST", "/v1/frames", buf.Bytes(), f.token, map[string]string{"Content-Type": mw.FormDataContentType()})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

func TestIngestRejectsBadCapturedAt(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	rec := f.do("POST", "/v1/frames", []byte("x"), f.token, map[string]string{"X-Captured-At": "yesterday"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHeartbeat(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	rec := f.do("POST", "/v1/heartbeat", nil, f.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	alive, err := f.store.CountAlive(context.Background(), time.Now().UTC())
	if err != nil || alive != 1 {
		t.Fatalf("alive = %d, err=%v, want 1", alive, err)
	}
	if f.do("POST", "/v1/heartbeat", nil, "", nil).Code != http.StatusUnauthorized {
		t.Error("heartbeat without token should be 401")
	}
}

func TestQueryCacheAndDBPaths(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	ctx := context.Background()
	now := time.Now().UTC()
	cacheDets := []storage.Detection{{
		DeviceID: "cam-1", FrameID: "cached-frame", CapturedAt: now.Add(-time.Second),
		Label: "person", Confidence: 0.9, X: 1, Y: 2, W: 30, H: 40,
	}}
	if err := f.store.CacheLatestDetections(ctx, "cam-1", cacheDets, time.Minute); err != nil {
		t.Fatalf("cache: %v", err)
	}
	if err := f.store.InsertDetections(ctx, []storage.Detection{{
		DeviceID: "cam-1", FrameID: "db-frame", CapturedAt: now.Add(-time.Minute),
		Label: "object", Confidence: 0.8, X: 5, Y: 6, W: 10, H: 10,
	}}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	from := now.Add(-5 * time.Minute).Format(time.RFC3339Nano)
	to := now.Add(time.Minute).Format(time.RFC3339Nano)
	rec := f.do("GET", "/v1/devices/cam-1/detections?from="+from+"&to="+to, nil, f.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cache query status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var cacheRes struct {
		Detections []storage.Detection `json:"detections"`
		Source     string              `json:"source"`
	}
	decodeBody(t, rec, &cacheRes)
	if cacheRes.Source != "cache" || len(cacheRes.Detections) != 1 || cacheRes.Detections[0].FrameID != "cached-frame" {
		t.Errorf("cache path = %+v", cacheRes)
	}

	rec = f.do("GET", "/v1/devices/cam-1/detections?source=db&from="+from+"&to="+to, nil, f.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("db query status = %d", rec.Code)
	}
	var dbRes struct {
		Detections []storage.Detection `json:"detections"`
		Source     string              `json:"source"`
	}
	decodeBody(t, rec, &dbRes)
	if dbRes.Source != "db" || len(dbRes.Detections) != 1 || dbRes.Detections[0].FrameID != "db-frame" {
		t.Errorf("db path = %+v", dbRes)
	}

	rec = f.do("GET", "/v1/devices/cam-2/detections", nil, f.token, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("path mismatch status = %d, want 403", rec.Code)
	}
	if f.do("GET", "/v1/devices/cam-1/detections", nil, "", nil).Code != http.StatusUnauthorized {
		t.Error("query without token should be 401")
	}
}

func TestQueryLabel(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	now := time.Now().UTC()
	if err := f.store.InsertDetections(context.Background(), []storage.Detection{{
		DeviceID: "cam-1", FrameID: "f", CapturedAt: now, Label: "person", Confidence: 0.7,
	}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	from := now.Add(-time.Minute).Format(time.RFC3339Nano)
	to := now.Add(time.Minute).Format(time.RFC3339Nano)
	rec := f.do("GET", "/v1/detections?label=person&from="+from+"&to="+to, nil, f.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		Detections []storage.Detection `json:"detections"`
		Source     string              `json:"source"`
	}
	decodeBody(t, rec, &res)
	if len(res.Detections) != 1 || res.Source != "db" {
		t.Errorf("label result = %+v", res)
	}
	if f.do("GET", "/v1/detections", nil, f.token, nil).Code != http.StatusBadRequest {
		t.Error("missing label should be 400")
	}
	if f.do("GET", "/v1/detections?label=person&from="+to+"&to="+from, nil, f.token, nil).Code != http.StatusBadRequest {
		t.Error("inverted window should be 400")
	}
}

func TestMetricsAndHealthEndpoints(t *testing.T) {
	f := newAPIFixture(t, 4, 0.9)
	f.do("POST", "/v1/frames", []byte("x"), f.token, nil)
	f.do("POST", "/v1/heartbeat", nil, f.token, nil)

	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := f.do("GET", path, nil, "", nil); rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
	rec := f.do("GET", "/metrics", nil, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	var snap obs.Snapshot
	decodeBody(t, rec, &snap)
	if snap.FramesReceived != 1 || snap.QueueCapacity != 4 || snap.AliveDevices != 1 {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.QueueDepth != 1 {
		t.Errorf("queue_depth = %d, want 1", snap.QueueDepth)
	}
}
