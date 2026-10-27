// Package replay validates session replay segments (contracts §23) and derives their storage key.
// Shared by ingest-gw (validation) and ingest-writer (index row + S3 key).
package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"
)

// Limits.
const (
	MaxSessionIDLen = 128
	MaxSegment      = 999_999 // key uses %06d
	maxStrLen       = 256     // service, env, version, view_name, user ids
)

// Resource is the segment's app identity.
type Resource struct {
	Service    string `json:"service"`
	Env        string `json:"env"`
	Version    string `json:"version"`
	Platform   string `json:"platform"` // web | android | ios
	SDKName    string `json:"sdk_name"`
	SDKVersion string `json:"sdk_version"`
}

// Stats are SDK-counted interactions in the segment.
type Stats struct {
	Clicks     int64 `json:"clicks"`
	Errors     int64 `json:"errors"`
	RageClicks int64 `json:"rage_clicks"`
	Inputs     int64 `json:"inputs"`
}

// Segment is the JSON body of POST /v1/replay.
type Segment struct {
	Resource        Resource          `json:"resource"`
	SessionID       string            `json:"session_id"`
	Segment         *int64            `json:"segment"`
	StartTS         int64             `json:"start_ts"` // unix ms
	EndTS           int64             `json:"end_ts"`
	Format          string            `json:"format"` // rrweb | wireframe
	HasFullSnapshot bool              `json:"has_full_snapshot"`
	ViewID          string            `json:"view_id"`
	ViewName        string            `json:"view_name"`
	UserID          string            `json:"user_id"`
	AnonymousID     string            `json:"anonymous_id"`
	Stats           Stats             `json:"stats"`
	Events          []json.RawMessage `json:"events"`
}

// Decode parses and validates a segment.
func Decode(body []byte) (*Segment, error) {
	var s Segment
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("invalid JSON: trailing data")
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Segment) validate() error {
	if s.SessionID == "" {
		return errors.New("session_id is required")
	}
	if len(s.SessionID) > MaxSessionIDLen || !safeID(s.SessionID) {
		return errors.New("session_id must be ≤ 128 characters of [A-Za-z0-9._-]")
	}
	if s.Segment == nil {
		return errors.New("segment is required")
	}
	if *s.Segment < 0 || *s.Segment > MaxSegment {
		return fmt.Errorf("segment must be between 0 and %d", MaxSegment)
	}
	switch s.Format {
	case "rrweb", "wireframe":
	case "":
		return errors.New("format is required")
	default:
		return fmt.Errorf("unknown format %q (rrweb | wireframe)", s.Format)
	}
	if s.Events == nil {
		return errors.New("events is required")
	}
	if s.StartTS < 0 || s.EndTS < 0 {
		return errors.New("start_ts / end_ts must be unix ms")
	}
	return nil
}

func safeID(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return s != "." && s != ".."
}

// Window returns the segment's [start, end] (received is used when the SDK sent no timestamps; end ≥ start).
func (s *Segment) Window(received time.Time) (start, end time.Time) {
	start, end = received, received
	if s.StartTS > 0 {
		start = time.UnixMilli(s.StartTS).UTC()
	}
	if s.EndTS > 0 {
		end = time.UnixMilli(s.EndTS).UTC()
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

// Key is the S3 object key: <org>/<project>/<yyyy-mm-dd>/<session_id>/<segment:06d>.json.gz (date = start_ts UTC).
func Key(org, project uint32, start time.Time, sessionID string, segment int64) string {
	return strconv.FormatUint(uint64(org), 10) + "/" + strconv.FormatUint(uint64(project), 10) + "/" +
		start.UTC().Format(time.DateOnly) + "/" + sessionID + "/" + fmt.Sprintf("%06d", segment) + ".json.gz"
}

// Cap bounds a string field to the row limit.
func Cap(s string) string {
	if len(s) <= maxStrLen {
		return s
	}
	i := maxStrLen
	for i > maxStrLen-4 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
