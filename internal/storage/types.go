package storage

import (
	"errors"
	"time"
)

// Sentinel errors returned by every Store implementation.
var (
	// ErrNotFound means the requested device or row does not exist.
	ErrNotFound = errors.New("storage: not found")
	// ErrCacheMiss means the Redis hot cache had no entry for the key.
	ErrCacheMiss = errors.New("storage: cache miss")
	// ErrInvalidRange means the caller supplied from >= to.
	ErrInvalidRange = errors.New("storage: invalid time range")
)

// Device is a registered edge camera. The gateway never stores raw device
// secrets: only the HMAC of the issued token (TokenHash) is persisted.
type Device struct {
	DeviceID   string    `json:"device_id"`
	SiteID     string    `json:"site_id"`
	TokenHash  string    `json:"token_hash,omitempty"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// Detection is one object detection row, stored flattened so it maps directly
// onto the partitioned detections table and the Redis JSON cache.
type Detection struct {
	ID         int64     `json:"id,omitempty"`
	DeviceID   string    `json:"device_id"`
	FrameID    string    `json:"frame_id"`
	CapturedAt time.Time `json:"captured_at"`
	Label      string    `json:"label"`
	Confidence float64   `json:"confidence"`
	X          int       `json:"x"`
	Y          int       `json:"y"`
	W          int       `json:"w"`
	H          int       `json:"h"`
}
