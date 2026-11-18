// Package config loads and validates the gateway's runtime configuration from
// environment variables. Every knob has a sane default so the service starts
// with zero configuration against the local docker-compose stack.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// StoreMode selects the persistence backend.
const (
	StorePostgres = "postgres"
	StoreMemory   = "mem"
)

// Config is the complete runtime configuration for the gateway, the load
// generator and the evaluation harness. Fields are populated by Load from
// EVG_-prefixed environment variables.
type Config struct {
	// HTTPAddr is the listen address for the REST API (default ":8080").
	HTTPAddr string
	// GRPCAddr is the listen address for the gRPC API (default ":9090").
	GRPCAddr string
	// PostgresDSN is the pgx connection string (EVG_POSTGRES_DSN).
	PostgresDSN string
	// RedisAddr is the Redis host:port (EVG_REDIS_ADDR).
	RedisAddr string
	// RedisPassword is optional; Redis runs without auth in compose.
	RedisPassword string

	// QueueCapacity is the hard bound on queued frame jobs.
	QueueCapacity int
	// WorkerCount is the number of detector goroutines.
	WorkerCount int
	// ShedThreshold is the queue occupancy fraction (0,1] above which the
	// gateway starts shedding low-priority frames.
	ShedThreshold float64
	// DetectionCacheTTL is the Redis hot-cache TTL for latest detections.
	DetectionCacheTTL time.Duration
	// HeartbeatTTL is the Redis liveness TTL refreshed by heartbeats.
	HeartbeatTTL time.Duration
	// CacheWindow is how far back a query must reach for the Redis hot cache
	// to be considered authoritative ("recent") instead of Postgres.
	CacheWindow time.Duration
	// DrainDeadline bounds graceful shutdown while workers drain the queue.
	DrainDeadline time.Duration
	// BatchMax is the maximum detections per Postgres COPY batch.
	BatchMax int
	// FlushInterval is the maximum age of a pending detection batch.
	FlushInterval time.Duration

	// Store selects the persistence backend: "postgres" or "mem".
	Store string
	// DeviceSecret is the HMAC secret used to issue and verify device tokens.
	DeviceSecret string
	// DetectorBackend selects "motion" (default) or "onnx".
	DetectorBackend string
	// ONNXModelPath is required when DetectorBackend == "onnx".
	ONNXModelPath string
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// Default returns the built-in configuration. It is what Load starts from
// before applying environment overrides.
func Default() Config {
	return Config{
		HTTPAddr:          ":8080",
		GRPCAddr:          ":9090",
		PostgresDSN:       "postgres://evg:evg@localhost:5432/evg?sslmode=disable",
		RedisAddr:         "localhost:6379",
		RedisPassword:     "",
		QueueCapacity:     4096,
		WorkerCount:       8,
		ShedThreshold:     0.85,
		DetectionCacheTTL: 5 * time.Minute,
		HeartbeatTTL:      30 * time.Second,
		CacheWindow:       2 * time.Minute,
		DrainDeadline:     15 * time.Second,
		BatchMax:          500,
		FlushInterval:     250 * time.Millisecond,
		Store:             StorePostgres,
		DeviceSecret:      "dev-secret-change-me",
		DetectorBackend:   "motion",
		LogLevel:          "info",
	}
}

// Load reads EVG_* environment variables over Default and validates the
// result. It returns a descriptive error when a value is malformed or
// outside its supported range.
func Load() (Config, error) {
	c := Default()

	strEnv(&c.HTTPAddr, "EVG_HTTP_ADDR")
	strEnv(&c.GRPCAddr, "EVG_GRPC_ADDR")
	strEnv(&c.PostgresDSN, "EVG_POSTGRES_DSN")
	strEnv(&c.RedisAddr, "EVG_REDIS_ADDR")
	strEnv(&c.RedisPassword, "EVG_REDIS_PASSWORD")
	strEnv(&c.Store, "EVG_STORE")
	strEnv(&c.DeviceSecret, "EVG_DEVICE_SECRET")
	strEnv(&c.DetectorBackend, "EVG_DETECTOR")
	strEnv(&c.ONNXModelPath, "EVG_ONNX_MODEL")
	strEnv(&c.LogLevel, "EVG_LOG_LEVEL")

	// Collect parse errors so a single bad variable is reported precisely.
	var errs []string
	if err := intEnv(&c.QueueCapacity, "EVG_QUEUE_CAPACITY"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := intEnv(&c.WorkerCount, "EVG_WORKER_COUNT"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := intEnv(&c.BatchMax, "EVG_BATCH_MAX"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := floatEnv(&c.ShedThreshold, "EVG_SHED_THRESHOLD"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := durEnv(&c.DetectionCacheTTL, "EVG_CACHE_TTL"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := durEnv(&c.HeartbeatTTL, "EVG_HEARTBEAT_TTL"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := durEnv(&c.CacheWindow, "EVG_CACHE_WINDOW"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := durEnv(&c.DrainDeadline, "EVG_DRAIN_DEADLINE"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := durEnv(&c.FlushInterval, "EVG_FLUSH_INTERVAL"); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// Validate checks invariants that the rest of the service relies on.
func (c Config) Validate() error {
	switch {
	case c.HTTPAddr == "":
		return fmt.Errorf("config: EVG_HTTP_ADDR must not be empty")
	case c.GRPCAddr == "":
		return fmt.Errorf("config: EVG_GRPC_ADDR must not be empty")
	case c.PostgresDSN == "":
		return fmt.Errorf("config: EVG_POSTGRES_DSN must not be empty")
	case c.RedisAddr == "":
		return fmt.Errorf("config: EVG_REDIS_ADDR must not be empty")
	case c.QueueCapacity <= 0:
		return fmt.Errorf("config: queue capacity must be > 0, got %d", c.QueueCapacity)
	case c.WorkerCount <= 0:
		return fmt.Errorf("config: worker count must be > 0, got %d", c.WorkerCount)
	case c.ShedThreshold <= 0 || c.ShedThreshold > 1:
		return fmt.Errorf("config: shed threshold must be in (0,1], got %g", c.ShedThreshold)
	case c.DetectionCacheTTL <= 0:
		return fmt.Errorf("config: cache TTL must be > 0, got %s", c.DetectionCacheTTL)
	case c.HeartbeatTTL <= 0:
		return fmt.Errorf("config: heartbeat TTL must be > 0, got %s", c.HeartbeatTTL)
	case c.CacheWindow <= 0:
		return fmt.Errorf("config: cache window must be > 0, got %s", c.CacheWindow)
	case c.DrainDeadline <= 0:
		return fmt.Errorf("config: drain deadline must be > 0, got %s", c.DrainDeadline)
	case c.BatchMax <= 0:
		return fmt.Errorf("config: batch max must be > 0, got %d", c.BatchMax)
	case c.FlushInterval <= 0:
		return fmt.Errorf("config: flush interval must be > 0, got %s", c.FlushInterval)
	case c.Store != StorePostgres && c.Store != StoreMemory:
		return fmt.Errorf("config: EVG_STORE must be %q or %q, got %q", StorePostgres, StoreMemory, c.Store)
	case c.DeviceSecret == "":
		return fmt.Errorf("config: EVG_DEVICE_SECRET must not be empty")
	case c.DetectorBackend != "motion" && c.DetectorBackend != "onnx":
		return fmt.Errorf("config: EVG_DETECTOR must be \"motion\" or \"onnx\", got %q", c.DetectorBackend)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: EVG_LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}

func strEnv(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func intEnv(dst *int, key string) error {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s: %q is not an integer", key, v)
	}
	*dst = n
	return nil
}

func floatEnv(dst *float64, key string) error {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("%s: %q is not a number", key, v)
	}
	*dst = f
	return nil
}

func durEnv(dst *time.Duration, key string) error {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %q is not a duration (e.g. 5m, 250ms)", key, v)
	}
	*dst = d
	return nil
}
