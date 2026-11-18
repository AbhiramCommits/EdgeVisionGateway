package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/storage"
)

func newAuthFixture(t *testing.T, opts ...AuthOption) (*Authenticator, *storage.MemStore, storage.Device) {
	t.Helper()
	ms := storage.NewMemStore()
	d := storage.Device{DeviceID: "cam-1", SiteID: "site-a", TokenHash: "hash"}
	if err := ms.RegisterDevice(context.Background(), d); err != nil {
		t.Fatalf("register device: %v", err)
	}
	return NewAuthenticator("test-secret", ms, opts...), ms, d
}

func TestIssueTokenFormat(t *testing.T) {
	token := IssueToken("cam-1", "s3cret")
	// base64url("cam-1") = "Y2FtLTE"
	if token[:7] != "Y2FtLTE" {
		t.Errorf("token prefix = %q, want Y2FtLTE", token[:7])
	}
	if len(token) != 7+1+64 {
		t.Errorf("token length = %d, want 72", len(token))
	}
	if token != IssueToken("cam-1", "s3cret") {
		t.Error("token issuance is not deterministic")
	}
	if token == IssueToken("cam-1", "other-secret") {
		t.Error("different secrets produced the same token")
	}
}

func TestAuthenticateValid(t *testing.T) {
	a, _, _ := newAuthFixture(t)
	id, err := a.Authenticate(context.Background(), IssueToken("cam-1", "test-secret"))
	if err != nil || id != "cam-1" {
		t.Fatalf("Authenticate = %q, %v; want cam-1, nil", id, err)
	}
}

func TestAuthenticateTampered(t *testing.T) {
	a, _, _ := newAuthFixture(t)
	token := IssueToken("cam-1", "test-secret")
	bad := token[:len(token)-1] + "0"
	if bad == token {
		bad = token[:len(token)-1] + "1"
	}
	if _, err := a.Authenticate(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered token: want ErrUnauthorized, got %v", err)
	}
	// A token for an unregistered device is rejected even with a valid signature.
	if _, err := a.Authenticate(context.Background(), IssueToken("cam-2", "test-secret")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown device: want ErrUnauthorized, got %v", err)
	}
}

func TestAuthenticateUnknownDeviceIsNegativelyCached(t *testing.T) {
	a, _, _ := newAuthFixture(t)
	token := IssueToken("ghost", "test-secret")
	if _, err := a.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("first: want ErrUnauthorized, got %v", err)
	}
	if a.neg.len() != 1 {
		t.Fatalf("negative cache size = %d, want 1", a.neg.len())
	}
	if _, err := a.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("second: want ErrUnauthorized, got %v", err)
	}
}

func TestAuthenticateMalformed(t *testing.T) {
	a, _, _ := newAuthFixture(t)
	for _, token := range []string{"", "no-dot", "!!!.deadbeef", ".", "abc.", ".def", "a.b.c"} {
		if _, err := a.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("token %q: want ErrUnauthorized, got %v", token, err)
		}
	}
}

func TestAuthenticateAutoRegister(t *testing.T) {
	a, ms, _ := newAuthFixture(t, WithAutoRegister(true))
	id, err := a.Authenticate(context.Background(), IssueToken("cam-new", "test-secret"))
	if err != nil || id != "cam-new" {
		t.Fatalf("Authenticate = %q, %v", id, err)
	}
	if _, err := ms.GetDevice(context.Background(), "cam-new"); err != nil {
		t.Fatalf("auto-registered device missing: %v", err)
	}
}

func TestNegativeCacheExpires(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	a, ms, _ := newAuthFixture(t, WithAuthClock(func() time.Time { return now }), WithNegativeTTL(time.Second))
	token := IssueToken("cam-late", "test-secret")
	if _, err := a.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown: %v", err)
	}
	if err := ms.RegisterDevice(context.Background(), storage.Device{DeviceID: "cam-late"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Still cached as unknown.
	if _, err := a.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cached unknown: %v", err)
	}
	now = now.Add(2 * time.Second)
	if id, err := a.Authenticate(context.Background(), token); err != nil || id != "cam-late" {
		t.Fatalf("after expiry: %q, %v", id, err)
	}
}

func TestNegativeCacheLRUBound(t *testing.T) {
	c := newNegCache(2, time.Minute)
	c.add("a")
	c.add("b")
	c.add("c")
	if c.len() != 2 {
		t.Fatalf("cache size = %d, want 2", c.len())
	}
	if c.contains("a") {
		t.Error("oldest entry should have been evicted")
	}
	if !c.contains("c") {
		t.Error("newest entry missing")
	}
}
