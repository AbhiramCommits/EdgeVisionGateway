// Package ingest contains the authentication, admission control and worker
// machinery shared by the REST and gRPC transports.
package ingest

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/edgevision/edgevisiongateway/internal/storage"
)

// ErrUnauthorized is the sentinel wrapped by every authentication failure, so
// transports can map any auth error to 401 / codes.Unauthenticated.
var ErrUnauthorized = errors.New("auth: unauthorized")

// IssueToken builds the per-device bearer token:
//
//	base64url(deviceID) + "." + hex(HMAC-SHA256(deviceID, secret))
//
// URL-safe base64 without padding is used so the token is header-safe; the
// signature still covers exactly the device ID.
func IssueToken(deviceID, secret string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(deviceID)) + "." + signToken(deviceID, secret)
}

func signToken(deviceID, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(deviceID))
	return hex.EncodeToString(mac.Sum(nil))
}

// Authenticator verifies device tokens against the shared HMAC secret and the
// device registry. Unknown device IDs are negatively cached so a flood of valid
// signatures for unregistered devices cannot hammer Redis.
type Authenticator struct {
	secret       []byte
	store        storage.Store
	neg          *negCache
	autoRegister bool
}

// AuthOption customises an Authenticator.
type AuthOption func(*Authenticator)

// WithAutoRegister registers a signed-but-unknown device on first use. It is
// intended for the simulated fleet and benchmarks; leave it off in production.
func WithAutoRegister(on bool) AuthOption {
	return func(a *Authenticator) { a.autoRegister = on }
}

// WithAuthClock injects a clock (used by negative-cache tests).
func WithAuthClock(fn func() time.Time) AuthOption {
	return func(a *Authenticator) {
		if fn != nil {
			a.neg.now = fn
		}
	}
}

// WithNegativeTTL sets how long an unknown-device result is cached.
func WithNegativeTTL(d time.Duration) AuthOption {
	return func(a *Authenticator) {
		if d > 0 {
			a.neg.ttl = d
		}
	}
}

// WithNegativeSize bounds the negative LRU.
func WithNegativeSize(n int) AuthOption {
	return func(a *Authenticator) {
		if n > 0 {
			a.neg.max = n
		}
	}
}

// NewAuthenticator returns an Authenticator using secret and store.
func NewAuthenticator(secret string, store storage.Store, opts ...AuthOption) *Authenticator {
	a := &Authenticator{
		secret: []byte(secret),
		store:  store,
		neg:    newNegCache(1024, 30*time.Second),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Authenticate validates token and returns the device ID it was issued for.
// Signature comparison uses hmac.Equal (constant time).
func (a *Authenticator) Authenticate(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("%w: empty token", ErrUnauthorized)
	}
	sep := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			sep = i
			break
		}
	}
	if sep <= 0 || sep == len(token)-1 {
		return "", fmt.Errorf("%w: malformed token", ErrUnauthorized)
	}
	encodedID, sig := token[:sep], token[sep+1:]
	idBytes, err := base64.RawURLEncoding.DecodeString(encodedID)
	if err != nil || len(idBytes) == 0 {
		return "", fmt.Errorf("%w: malformed device id encoding", ErrUnauthorized)
	}
	deviceID := string(idBytes)
	want := signToken(deviceID, string(a.secret))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return "", fmt.Errorf("%w: bad signature", ErrUnauthorized)
	}
	if a.neg.contains(deviceID) {
		return "", fmt.Errorf("%w: unknown device", ErrUnauthorized)
	}
	if _, err := a.store.GetDevice(ctx, deviceID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			if a.autoRegister {
				d := storage.Device{DeviceID: deviceID, SiteID: "auto", TokenHash: tokenHash(token), Status: "active"}
				if rerr := a.store.RegisterDevice(ctx, d); rerr != nil {
					return "", fmt.Errorf("auth: auto-register device: %w", rerr)
				}
				return deviceID, nil
			}
			a.neg.add(deviceID)
			return "", fmt.Errorf("%w: unknown device", ErrUnauthorized)
		}
		return "", fmt.Errorf("auth: device lookup: %w", err)
	}
	return deviceID, nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// negCache is a tiny mutex-protected LRU with per-entry expiry.
type negCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	max     int
	ttl     time.Duration
	now     func() time.Time
}

type negEntry struct {
	id      string
	expires time.Time
}

func newNegCache(max int, ttl time.Duration) *negCache {
	return &negCache{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		max:     max,
		ttl:     ttl,
		now:     time.Now,
	}
}

func (c *negCache) contains(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[id]
	if !ok {
		return false
	}
	e := el.Value.(*negEntry)
	if c.now().After(e.expires) {
		c.order.Remove(el)
		delete(c.entries, id)
		return false
	}
	c.order.MoveToFront(el)
	return true
}

func (c *negCache) add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[id]; ok {
		e := el.Value.(*negEntry)
		e.expires = c.now().Add(c.ttl)
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&negEntry{id: id, expires: c.now().Add(c.ttl)})
	c.entries[id] = el
	for c.order.Len() > c.max {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.entries, back.Value.(*negEntry).id)
	}
}

func (c *negCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
