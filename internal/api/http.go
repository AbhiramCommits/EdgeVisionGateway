// Package api exposes the gateway over REST (net/http) and gRPC. Both
// transports authenticate with the same Authenticator and call the same
// ingest.Service, so behaviour (including shedding) is identical.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/ingest"
	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/queue"
	"github.com/edgevision/edgevisiongateway/internal/storage"
)

// HTTPOptions configures the REST handler.
type HTTPOptions struct {
	Service       *ingest.Service
	Auth          *ingest.Authenticator
	Store         storage.Store
	Metrics       *obs.Metrics
	Queue         *queue.FrameQueue
	Logger        *slog.Logger
	Ready         func(context.Context) error
	Now           func() time.Time
	MaxFrameBytes int64
}

// HTTPHandler holds the REST dependencies.
type HTTPHandler struct {
	svc     *ingest.Service
	auth    *ingest.Authenticator
	store   storage.Store
	metrics *obs.Metrics
	queue   *queue.FrameQueue
	log     *slog.Logger
	ready   func(context.Context) error
	now     func() time.Time
	maxBody int64
}

// NewHTTPHandler builds the mux with the full REST surface.
func NewHTTPHandler(o HTTPOptions) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = 8 << 20
	}
	h := &HTTPHandler{
		svc:     o.Service,
		auth:    o.Auth,
		store:   o.Store,
		metrics: o.Metrics,
		queue:   o.Queue,
		log:     o.Logger,
		ready:   o.Ready,
		now:     o.Now,
		maxBody: o.MaxFrameBytes,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/frames", h.handleIngest)
	mux.HandleFunc("POST /v1/heartbeat", h.handleHeartbeat)
	mux.HandleFunc("GET /v1/devices/{id}/detections", h.handleDeviceDetections)
	mux.HandleFunc("GET /v1/detections", h.handleLabelDetections)
	mux.HandleFunc("GET /healthz", h.handleHealth)
	mux.HandleFunc("GET /readyz", h.handleReady)
	mux.HandleFunc("GET /metrics", h.handleMetrics)
	return h.middleware(mux)
}

func (h *HTTPHandler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := randomID()
		w.Header().Set("X-Request-ID", reqID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, reqID)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				h.log.Error("panic serving request", "request_id", reqID, "panic", fmt.Sprint(p))
				if !rec.wrote {
					writeError(rec, http.StatusInternalServerError, "internal error")
				}
			}
		}()
		next.ServeHTTP(rec, r.WithContext(ctx))
		if r.URL.Path != "/metrics" && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			h.log.Info("http request",
				"request_id", reqID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
			)
		}
	})
}

type requestIDKey struct{}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// authenticate resolves the Bearer token, writing 401 on failure.
func (h *HTTPHandler) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := bearerToken(r.Header.Get("Authorization"))
	deviceID, err := h.auth.Authenticate(r.Context(), token)
	if err != nil {
		if h.metrics != nil {
			h.metrics.IncAuthFailures()
		}
		writeError(w, http.StatusUnauthorized, "invalid device token")
		return "", false
	}
	return deviceID, true
}

func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func (h *HTTPHandler) handleIngest(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if hdr := r.Header.Get("X-Device-ID"); hdr != "" && hdr != deviceID {
		if h.metrics != nil {
			h.metrics.IncAuthFailures()
		}
		writeError(w, http.StatusUnauthorized, "token does not match device_id header")
		return
	}
	frameID := r.Header.Get("X-Frame-ID")
	if frameID == "" {
		frameID = "f-" + randomID()
	}
	var capturedAt time.Time
	if raw := r.Header.Get("X-Captured-At"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "X-Captured-At must be RFC3339")
			return
		}
		capturedAt = t
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	data, contentType, err := readFrame(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty frame body")
		return
	}

	accepted, out, err := h.svc.IngestFrame(r.Context(), deviceID, frameID, capturedAt, data, contentType)
	if err != nil {
		if errors.Is(err, ingest.ErrShuttingDown) {
			writeError(w, http.StatusServiceUnavailable, "gateway is shutting down")
			return
		}
		h.log.Error("ingest failed", "device_id", deviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	if !accepted {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":          "queue saturated: frame shed",
			"retry_after_ms": out.RetryAfter.Milliseconds(),
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":      "queued",
		"frame_id":    out.FrameID,
		"queue_depth": out.QueueDepth,
	})
}

// readFrame accepts either a raw JPEG/PNG body or multipart/form-data with a
// "frame" file part.
func readFrame(r *http.Request) ([]byte, string, error) {
	ct := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)
	if mediaType == "multipart/form-data" {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			return nil, "", fmt.Errorf("invalid multipart body: %v", err)
		}
		file, hdr, err := r.FormFile("frame")
		if err != nil {
			return nil, "", errors.New("multipart body needs a \"frame\" file field")
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return nil, "", fmt.Errorf("read frame part: %v", err)
		}
		partType := hdr.Header.Get("Content-Type")
		if partType == "" {
			partType = ct
		}
		return data, partType, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read body: %v", err)
	}
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}

func (h *HTTPHandler) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if hdr := r.Header.Get("X-Device-ID"); hdr != "" && hdr != deviceID {
		if h.metrics != nil {
			h.metrics.IncAuthFailures()
		}
		writeError(w, http.StatusUnauthorized, "token does not match device_id header")
		return
	}
	if err := h.svc.Heartbeat(r.Context(), deviceID); err != nil {
		if errors.Is(err, ingest.ErrShuttingDown) {
			writeError(w, http.StatusServiceUnavailable, "gateway is shutting down")
			return
		}
		h.log.Error("heartbeat failed", "device_id", deviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	alive, err := h.store.CountAlive(r.Context(), h.now())
	if err != nil {
		alive = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "device_id": deviceID, "alive_devices": alive})
}

func (h *HTTPHandler) handleDeviceDetections(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	pathID := r.PathValue("id")
	if pathID != deviceID {
		writeError(w, http.StatusForbidden, "token does not match device path")
		return
	}
	from, to, limit, err := parseQueryWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	forceDB := r.URL.Query().Get("source") == "db"
	res, err := h.svc.QueryDevice(r.Context(), deviceID, from, to, limit, forceDB)
	if err != nil {
		if errors.Is(err, storage.ErrInvalidRange) {
			writeError(w, http.StatusBadRequest, "from must be before to")
			return
		}
		h.log.Error("device query failed", "device_id", deviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if res.Detections == nil {
		res.Detections = []storage.Detection{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *HTTPHandler) handleLabelDetections(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authenticate(w, r); !ok {
		return
	}
	label := r.URL.Query().Get("label")
	if label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	from, to, limit, err := parseQueryWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dets, err := h.svc.QueryLabel(r.Context(), label, from, to, limit)
	if err != nil {
		if errors.Is(err, storage.ErrInvalidRange) {
			writeError(w, http.StatusBadRequest, "from must be before to")
			return
		}
		h.log.Error("label query failed", "label", label, "err", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if dets == nil {
		dets = []storage.Detection{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"detections": dets, "source": "db"})
}

func parseQueryWindow(r *http.Request) (from, to time.Time, limit int, err error) {
	q := r.URL.Query()
	if raw := q.Get("from"); raw != "" {
		from, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}, time.Time{}, 0, errors.New("from must be RFC3339")
		}
	}
	if raw := q.Get("to"); raw != "" {
		to, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}, time.Time{}, 0, errors.New("to must be RFC3339")
		}
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 0 {
			return time.Time{}, time.Time{}, 0, errors.New("limit must be a non-negative integer")
		}
	}
	return from, to, limit, nil
}

func (h *HTTPHandler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *HTTPHandler) handleReady(w http.ResponseWriter, r *http.Request) {
	if h.ready == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not ready", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (h *HTTPHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := h.metrics.Snapshot()
	snap.QueueDepth = h.queue.Depth()
	snap.QueueCapacity = h.queue.Capacity()
	snap.QueueHighWater = h.queue.HighWaterMark()
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if alive, err := h.store.CountAlive(ctx, h.now()); err == nil {
		snap.AliveDevices = int64(alive)
	}
	writeJSON(w, http.StatusOK, snap)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
