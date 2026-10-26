// Package store reads telemetry from ClickHouse and metadata from Postgres. Every statement goes through
// tq (tenant marker enforced). Handlers depend on the API interface so they can be tested with a fake.
package store

import (
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
	"github.com/tasktrooper/telemetry/services/api/internal/tq"
)

// ErrNotFound is returned when an entity does not exist in the tenant.
var ErrNotFound = errors.New("not found")

// ErrInvalid wraps validation errors (→ 400).
type ErrInvalid struct{ Msg string }

func (e ErrInvalid) Error() string { return e.Msg }

// Invalid returns an ErrInvalid.
func Invalid(msg string) error { return ErrInvalid{Msg: msg} }

// Q carries the common query parameters.
type Q struct {
	W   timerange.Window
	Env string
	Now time.Time
}

// DB implements API on ClickHouse + Postgres.
type DB struct {
	ch tq.CH
	pg tq.PG
	// Pool is used for statements that are not tenant scoped (api key lookup only).
	Pool *pgxpool.Pool
	// SecretsKey seals UI-entered credentials (services/pkg/secrets); nil = secrets.Key() on first use.
	SecretsKey []byte
}

// New returns a store.
func New(ch driver.Conn, pg *pgxpool.Pool) *DB {
	return &DB{ch: tq.CH{Conn: ch}, pg: tq.PG{Pool: pg}, Pool: pg}
}

// ---------------------------------------------------------------- helpers

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// Slug mirrors the UI's id derivation: route.replace(/[^a-z0-9]+/gi, '-').replace(/^-|-$/g, ”).
func Slug(s string) string { return strings.Trim(nonAlnum.ReplaceAllString(s, "-"), "-") }

// LowerSlug is Slug lower-cased (database / cluster ids).
func LowerSlug(s string) string { return strings.ToLower(Slug(s)) }

// EndpointID is the UI endpoint id: "post-v1-orders".
func EndpointID(method, route string) string { return strings.ToLower(method) + "-" + Slug(route) }

// fin replaces NaN/Inf with 0 (JSON cannot encode them; ClickHouse returns NaN for empty digests).
func fin(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func qt(xs []float64, i int) float64 {
	if i < len(xs) {
		return fin(xs[i])
	}
	return 0
}

func div(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return fin(a / b)
}

func hexID(raw string) string {
	s := hex.EncodeToString([]byte(raw))
	if strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}

func ms(t time.Time) int64 {
	if t.IsZero() || t.Unix() <= 0 {
		return 0
	}
	return t.UnixMilli()
}

func secs(t time.Time) int64 { return t.Unix() }

// envCond returns " AND env = ?" and its arg when env is set.
func envCond(env string, col string) (string, []any) {
	if env == "" {
		return "", nil
	}
	return " AND " + col + " = ?", []any{env}
}

// redTable picks the RED rollup for the window.
func redTable(w timerange.Window) string {
	if w.Hourly() {
		return "red_1h"
	}
	return "red_1m"
}

// sanitize clamps a free-text limit.
func clampLimit(n, def, maxN int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxN)
}
