package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// Internal service credential (contracts §16): trusted platform services (trooper-cloud, loadtest-runner)
// send `x-trooper-internal: <TT_INTERNAL_TOKEN>` + `x-trooper-project: <id>` instead of a per-project ingest key.
const (
	HeaderInternal = "X-Trooper-Internal"
	HeaderProject  = "X-Trooper-Project"
)

// ProjectSource resolves a project id to its org; returns ErrInvalidKey when the project does not exist.
type ProjectSource interface {
	ProjectOrg(ctx context.Context, projectID int64) (orgID int64, err error)
}

// Internal validates the internal token and resolves the project's tenant with a TTL cache.
// A nil *Internal (TT_INTERNAL_TOKEN unset) rejects every request.
type Internal struct {
	tokenHash  [32]byte
	src        ProjectSource
	ttl, negTT time.Duration
	log        *slog.Logger

	mu      sync.RWMutex
	entries map[int64]projEntry
	sf      singleflight.Group
	now     func() time.Time
}

type projEntry struct {
	key     *Key // nil = unknown project
	expires time.Time
}

// NewInternal returns nil when token is empty (feature off).
func NewInternal(token string, src ProjectSource, log *slog.Logger) *Internal {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return &Internal{
		tokenHash: sha256.Sum256([]byte(token)), src: src, ttl: 60 * time.Second, negTT: 10 * time.Second, log: log,
		entries: map[int64]projEntry{}, now: time.Now,
	}
}

// Enabled reports whether the internal credential is configured.
func (i *Internal) Enabled() bool { return i != nil }

// Lookup checks token (constant time, over sha256 digests so length does not leak) and resolves project.
// Any failure returns ErrInvalidKey except backend errors without a cached entry.
func (i *Internal) Lookup(ctx context.Context, token, project string) (*Key, error) {
	if i == nil || token == "" || len(token) > 512 {
		return nil, ErrInvalidKey
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(token)))
	if subtle.ConstantTimeCompare(h[:], i.tokenHash[:]) != 1 {
		return nil, ErrInvalidKey
	}
	pid, err := strconv.ParseInt(strings.TrimSpace(project), 10, 64)
	if err != nil || pid <= 0 || pid > int64(^uint32(0)) {
		return nil, ErrInvalidKey
	}
	now := i.now()
	i.mu.RLock()
	e, ok := i.entries[pid]
	i.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return hitProject(e.key)
	}
	v, err, _ := i.sf.Do(strconv.FormatInt(pid, 10), func() (any, error) {
		org, err := i.src.ProjectOrg(ctx, pid)
		switch {
		case errors.Is(err, ErrInvalidKey):
			i.store(pid, projEntry{expires: i.now().Add(i.negTT)})
			return (*Key)(nil), nil
		case err != nil:
			return nil, err
		}
		k := &Key{Tenant: tenant.Tenant{OrgID: uint32(org), ProjectID: uint32(pid)}, Internal: true}
		i.store(pid, projEntry{key: k, expires: i.now().Add(i.ttl)})
		return k, nil
	})
	if err != nil {
		if ok {
			i.log.Warn("project lookup failed, serving stale entry", "project", pid, "err", err)
			return hitProject(e.key)
		}
		return nil, err
	}
	return hitProject(v.(*Key))
}

func hitProject(k *Key) (*Key, error) {
	if k == nil {
		return nil, ErrInvalidKey
	}
	return k, nil
}

func (i *Internal) store(pid int64, e projEntry) {
	i.mu.Lock()
	if len(i.entries) > 100_000 { // bound memory against id enumeration with a valid token
		i.entries = map[int64]projEntry{}
	}
	i.entries[pid] = e
	i.mu.Unlock()
}

// PGProjects resolves projects from Postgres.
type PGProjects struct{ Pool *pgxpool.Pool }

// ProjectOrg implements ProjectSource.
func (p PGProjects) ProjectOrg(ctx context.Context, id int64) (int64, error) {
	var org int64
	err := p.Pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, id).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvalidKey
	}
	return org, err
}

// StaticProjects is an in-memory ProjectSource (project → org) for tests.
type StaticProjects map[int64]int64

// ProjectOrg implements ProjectSource.
func (s StaticProjects) ProjectOrg(_ context.Context, id int64) (int64, error) {
	if org, ok := s[id]; ok {
		return org, nil
	}
	return 0, ErrInvalidKey
}
