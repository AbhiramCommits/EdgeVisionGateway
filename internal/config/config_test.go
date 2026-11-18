package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearEVG unset every EVG_* variable for the duration of a test so the
// developer's shell cannot influence assertions. t.Setenv handles restoration.
func clearEVG(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "EVG_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEVG(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() defaults: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
	if c.GRPCAddr != ":9090" {
		t.Errorf("GRPCAddr = %q, want :9090", c.GRPCAddr)
	}
	if c.QueueCapacity != 4096 {
		t.Errorf("QueueCapacity = %d, want 4096", c.QueueCapacity)
	}
	if c.WorkerCount != 8 {
		t.Errorf("WorkerCount = %d, want 8", c.WorkerCount)
	}
	if c.ShedThreshold != 0.85 {
		t.Errorf("ShedThreshold = %g, want 0.85", c.ShedThreshold)
	}
	if c.DetectionCacheTTL != 5*time.Minute {
		t.Errorf("DetectionCacheTTL = %s, want 5m", c.DetectionCacheTTL)
	}
	if c.Store != StorePostgres {
		t.Errorf("Store = %q, want postgres", c.Store)
	}
	if c.PostgresDSN == "" || c.RedisAddr == "" {
		t.Error("DSN/RedisAddr must have defaults")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	clearEVG(t)
	t.Setenv("EVG_HTTP_ADDR", ":18080")
	t.Setenv("EVG_QUEUE_CAPACITY", "64")
	t.Setenv("EVG_SHED_THRESHOLD", "0.5")
	t.Setenv("EVG_CACHE_TTL", "90s")
	t.Setenv("EVG_STORE", "mem")
	t.Setenv("EVG_DETECTOR", "motion")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if c.HTTPAddr != ":18080" || c.QueueCapacity != 64 || c.ShedThreshold != 0.5 {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.DetectionCacheTTL != 90*time.Second {
		t.Errorf("cache TTL = %s, want 90s", c.DetectionCacheTTL)
	}
	if c.Store != StoreMemory {
		t.Errorf("Store = %q, want mem", c.Store)
	}
}

func TestValidateRejectsBadShedThreshold(t *testing.T) {
	clearEVG(t)
	t.Setenv("EVG_SHED_THRESHOLD", "1.5")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() with shed threshold 1.5: want error, got nil")
	}
	if !strings.Contains(err.Error(), "shed threshold") {
		t.Errorf("error %q does not mention shed threshold", err)
	}
}

func TestValidateRejectsBadWorkerCount(t *testing.T) {
	clearEVG(t)
	t.Setenv("EVG_WORKER_COUNT", "0")
	if _, err := Load(); err == nil {
		t.Fatal("Load() with 0 workers: want error, got nil")
	}
}

func TestValidateRejectsBadStore(t *testing.T) {
	clearEVG(t)
	t.Setenv("EVG_STORE", "sqlite")
	if _, err := Load(); err == nil {
		t.Fatal("Load() with unknown store: want error, got nil")
	}
}

func TestLoadRejectsNonNumericInt(t *testing.T) {
	clearEVG(t)
	t.Setenv("EVG_QUEUE_CAPACITY", "lots")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "not an integer") {
		t.Fatalf("want integer parse error, got %v", err)
	}
}

func TestValidateAcceptsBoundaryShedThreshold(t *testing.T) {
	c := Default()
	c.ShedThreshold = 1
	if err := c.Validate(); err != nil {
		t.Fatalf("shed threshold 1 should be valid: %v", err)
	}
}
