// Command gateway is the EdgeVisionGateway server: REST + gRPC ingestion,
// bounded-queue backpressure, a CV worker pool, Postgres persistence and a
// Redis hot cache.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/api"
	"github.com/edgevision/edgevisiongateway/internal/config"
	"github.com/edgevision/edgevisiongateway/internal/ingest"
	"github.com/edgevision/edgevisiongateway/internal/obs"
	"github.com/edgevision/edgevisiongateway/internal/queue"
	"github.com/edgevision/edgevisiongateway/internal/storage"
	"github.com/edgevision/edgevisiongateway/internal/vision"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics := obs.New()

	store, ready, err := buildStore(ctx, cfg, metrics, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("store close", "err", err)
		}
	}()

	// Probe the configured detector once so a misconfiguration is logged at
	// startup; each worker then constructs its own instance.
	if probe, err := vision.FromConfig(cfg.DetectorBackend, cfg.ONNXModelPath); err != nil {
		return err
	} else {
		logger.Info("detector configured", "name", probe.Name(), "workers", cfg.WorkerCount)
	}
	if cfg.AutoRegister {
		logger.Warn("auto-registration enabled: signed but unknown devices will be created on first use")
	}

	q := queue.New(cfg.QueueCapacity, cfg.ShedThreshold, metrics)
	recent := ingest.NewRecentTracker(cfg.RecentWindow)
	pool := ingest.NewPool(ingest.PoolOptions{
		Queue:         q,
		Store:         store,
		Metrics:       metrics,
		Recent:        recent,
		WorkerCount:   cfg.WorkerCount,
		NewDetector:   func() vision.Detector { d, _ := vision.FromConfig(cfg.DetectorBackend, cfg.ONNXModelPath); return d },
		CacheTTL:      cfg.DetectionCacheTTL,
		BatchMax:      cfg.BatchMax,
		FlushInterval: cfg.FlushInterval,
		Logger:        logger,
	})
	pool.Start()

	svc := ingest.NewService(ingest.ServiceOptions{
		Queue:       q,
		Store:       store,
		Metrics:     metrics,
		Recent:      recent,
		CacheWindow: cfg.CacheWindow,
		Logger:      logger,
	})
	auth := ingest.NewAuthenticator(cfg.DeviceSecret, store, ingest.WithAutoRegister(cfg.AutoRegister))

	handler := api.NewHTTPHandler(api.HTTPOptions{
		Service: svc,
		Auth:    auth,
		Store:   store,
		Metrics: metrics,
		Queue:   q,
		Logger:  logger,
		Ready:   ready,
	})
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	grpcSrv := api.NewGRPCServer(api.GRPCOptions{
		Service: svc,
		Auth:    auth,
		Store:   store,
		Metrics: metrics,
		Logger:  logger,
	})
	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("rest listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		logger.Info("grpc listening", "addr", cfg.GRPCAddr)
		if err := grpcSrv.Serve(grpcLis); err != nil && !errors.Is(err, net.ErrClosed) {
			errCh <- err
		}
	}()
	logger.Info("gateway ready",
		"store", cfg.Store,
		"queue_capacity", cfg.QueueCapacity,
		"shed_threshold", cfg.ShedThreshold,
		"workers", cfg.WorkerCount,
	)

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
	case err := <-errCh:
		logger.Error("server error; shutting down", "err", err)
	}

	svc.StopAccepting()
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
	_ = httpSrv.Shutdown(httpCtx)
	cancelHTTP()
	grpcDone := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(grpcDone)
	}()
	select {
	case <-grpcDone:
	case <-time.After(5 * time.Second):
		grpcSrv.Stop()
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.DrainDeadline)
	defer cancelDrain()
	if err := pool.Shutdown(drainCtx); err != nil {
		logger.Warn("worker drain did not finish before deadline", "err", err)
	} else {
		logger.Info("workers drained", "queue_depth", q.Depth(), "high_water_mark", q.HighWaterMark())
	}
	snap := metrics.Snapshot()
	logger.Info("final metrics",
		"frames_received", snap.FramesReceived,
		"frames_shed", snap.FramesShed,
		"frames_processed", snap.FramesProcessed,
		"detections_written", snap.DetectionsWriten,
		"p50_ms", snap.Latency.P50MS,
		"p95_ms", snap.Latency.P95MS,
		"p99_ms", snap.Latency.P99MS,
	)
	return nil
}

func buildStore(ctx context.Context, cfg config.Config, metrics *obs.Metrics, logger *slog.Logger) (storage.Store, func(context.Context) error, error) {
	if cfg.Store == config.StoreMemory {
		ms := storage.NewMemStore(storage.WithMetrics(metrics), storage.WithHeartbeatTTL(cfg.HeartbeatTTL))
		logger.Warn("using in-memory store: data is not durable (EVG_STORE=mem)")
		return ms, func(context.Context) error { return nil }, nil
	}
	pg, err := storage.NewPostgresStore(ctx, cfg.PostgresDSN)
	if err != nil {
		return nil, nil, err
	}
	if err := pg.Migrate(ctx); err != nil {
		_ = pg.Close()
		return nil, nil, err
	}
	rd, err := storage.NewRedisStoreFromAddr(ctx, cfg.RedisAddr, cfg.RedisPassword, metrics, cfg.HeartbeatTTL)
	if err != nil {
		_ = pg.Close()
		return nil, nil, err
	}
	ready := func(ctx context.Context) error {
		if err := pg.Ping(ctx); err != nil {
			return err
		}
		return rd.Ping(ctx)
	}
	logger.Info("postgres and redis connected")
	return storage.NewHybridStore(pg, rd), ready, nil
}

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
