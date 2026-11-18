//go:build integration

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func testPostgresDSN() string {
	if v := os.Getenv("EVG_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://evg:evg@localhost:5432/evg?sslmode=disable"
}

func newTestPostgres(t *testing.T) *PostgresStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := NewPostgresStore(ctx, testPostgresDSN())
	if err != nil {
		t.Fatalf("connect postgres: %v (is docker compose up?)", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func TestPostgresDeviceRegistry(t *testing.T) {
	s := newTestPostgres(t)
	ctx := context.Background()
	id := unique("dev")
	d := Device{DeviceID: id, SiteID: "site-x", TokenHash: "hash", Status: "active"}
	if err := s.RegisterDevice(ctx, d); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SiteID != "site-x" || got.TokenHash != "hash" || got.CreatedAt.IsZero() {
		t.Errorf("bad device: %+v", got)
	}
	at := time.Now().UTC().Add(-time.Minute)
	if err := s.TouchDevice(ctx, id, at); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := s.TouchDevice(ctx, unique("ghost"), at); !errors.Is(err, ErrNotFound) {
		t.Errorf("touch unknown: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetDevice(ctx, unique("ghost")); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown: want ErrNotFound, got %v", err)
	}
}

func TestPostgresInsertAndQuery(t *testing.T) {
	s := newTestPostgres(t)
	ctx := context.Background()
	dev := unique("dev")
	base := time.Now().UTC().Truncate(time.Second)

	dets := []Detection{
		{DeviceID: dev, FrameID: "f1", CapturedAt: base, Label: "person", Confidence: 0.9, X: 1, Y: 2, W: 3, H: 4},
		{DeviceID: dev, FrameID: "f2", CapturedAt: base.Add(time.Second), Label: "object", Confidence: 0.8, X: 5, Y: 6, W: 7, H: 8},
		{DeviceID: dev, FrameID: "f3", CapturedAt: base.Add(2 * time.Second), Label: "person", Confidence: 0.7, X: 9, Y: 10, W: 11, H: 12},
	}
	if err := s.InsertDetections(ctx, dets); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(), `DELETE FROM detections WHERE device_id = $1`, dev)
	})

	got, err := s.QueryByDevice(ctx, dev, base.Add(-time.Second), base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("query device: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	if got[0].ID == 0 || got[0].FrameID != "f3" {
		t.Errorf("newest row wrong: %+v", got[0])
	}
	if got[0].Confidence < 0.69 || got[0].Confidence > 0.71 {
		t.Errorf("confidence round-trip = %v, want 0.7", got[0].Confidence)
	}

	limited, err := s.QueryByDevice(ctx, dev, base.Add(-time.Second), base.Add(time.Minute), 2)
	if err != nil {
		t.Fatalf("query limit: %v", err)
	}
	if len(limited) != 2 || limited[0].FrameID != "f3" {
		t.Errorf("limit wrong: %+v", limited)
	}

	byLabel, err := s.QueryByLabel(ctx, "person", base.Add(-time.Second), base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("query label: %v", err)
	}
	if len(byLabel) != 2 {
		t.Errorf("label query returned %d rows, want 2", len(byLabel))
	}

	// Half-open window: [base, base+2s) excludes the row at base+2s.
	window, err := s.QueryByDevice(ctx, dev, base, base.Add(2*time.Second), 10)
	if err != nil {
		t.Fatalf("query window: %v", err)
	}
	if len(window) != 2 {
		t.Errorf("half-open window returned %d rows, want 2", len(window))
	}

	if _, err := s.QueryByDevice(ctx, dev, base, base, 10); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("want ErrInvalidRange, got %v", err)
	}
}

// TestPostgresPartitionRouting proves that a row captured on day D physically
// lands in the detections_YYYYMMDD partition for D, by reading tableoid.
func TestPostgresPartitionRouting(t *testing.T) {
	s := newTestPostgres(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 5, 15, 30, 0, 0, time.UTC)
	if err := s.EnsurePartition(ctx, day); err != nil {
		t.Fatalf("ensure partition: %v", err)
	}
	dev := unique("part")
	d := Detection{DeviceID: dev, FrameID: "f1", CapturedAt: day, Label: "person", Confidence: 0.5, X: 1, Y: 1, W: 2, H: 2}
	if err := s.InsertDetections(ctx, []Detection{d}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(), `DELETE FROM detections WHERE device_id = $1`, dev)
	})

	var partition string
	if err := s.Pool().QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM detections WHERE device_id = $1`, dev).Scan(&partition); err != nil {
		t.Fatalf("tableoid query: %v", err)
	}
	want := "detections_20261005"
	if partition != want {
		t.Errorf("row landed in %q, want %q", partition, want)
	}

	// The partition must be registered in pg_class as a partition of detections.
	var n int
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE c.relname = $1 AND p.relname = 'detections'`, want).Scan(&n); err != nil {
		t.Fatalf("pg_class query: %v", err)
	}
	if n != 1 {
		t.Errorf("partition %s not attached to detections in pg_class (count=%d)", want, n)
	}

	// EnsurePartition must be idempotent.
	if err := s.EnsurePartition(ctx, day); err != nil {
		t.Fatalf("ensure partition twice: %v", err)
	}
}

func TestPostgresMigrateCreatesDefaultPartitions(t *testing.T) {
	s := newTestPostgres(t)
	ctx := context.Background()
	day := time.Now().UTC()
	for _, delta := range []int{-1, 0, 1} {
		name := "detections_" + day.AddDate(0, 0, delta).Format("20060102")
		var exists bool
		if err := s.Pool().QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			t.Fatalf("to_regclass: %v", err)
		}
		if !exists {
			t.Errorf("expected partition %s after Migrate", name)
		}
	}
}
