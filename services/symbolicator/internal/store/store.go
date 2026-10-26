// Package store keeps artifacts and per-release index objects in S3-compatible blob storage.
//
// Layout (contracts.md §12, plus the app segment so two apps can share a release name):
//
//	<project>/<kind>/<app>/<release>/<file name>
//	<project>/<kind>/<app>/<release>/index.json
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kinds accepted by POST /v1/artifacts.
const (
	KindSourcemap = "sourcemap"
	KindDSYM      = "dsym"
	KindR8        = "r8"
	KindProguard  = "proguard"
	KindDart      = "dart" // Flutter --split-debug-info symbols (extension to contracts.md §12)
	KindNDK       = "ndk"  // Android unstripped native .so (ELF, GNU build id) (extension)
)

// ValidKind reports whether k is a known artifact kind.
func ValidKind(k string) bool {
	switch k {
	case KindSourcemap, KindDSYM, KindR8, KindProguard, KindDart, KindNDK:
		return true
	}
	return false
}

// DebugID is one image (arch slice) inside a native debug file.
type DebugID struct {
	ID     string `json:"id"`
	Arch   string `json:"arch"`
	Format string `json:"format"`
}

// File is one stored artifact.
type File struct {
	Name       string `json:"name"` // relative name as uploaded (e.g. assets/app.min.js.map)
	Key        string `json:"key"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	UploadedAt int64  `json:"uploadedAt"` // unix ms
	// sourcemap: the minified file this map belongs to (relative name) and, when uploaded, its key
	Minified    string `json:"minified,omitempty"`
	MinifiedKey string `json:"minifiedKey,omitempty"`
	// minified JS: the map named by its //# sourceMappingURL (relative name)
	SourceMap string `json:"sourceMap,omitempty"`
	// dsym / dart
	Debug []DebugID `json:"debug,omitempty"`
	// r8 / proguard
	MapID string `json:"mapId,omitempty"`
}

// Index lists the artifacts of one (project, kind, app, release).
type Index struct {
	Project   int64  `json:"project"`
	Kind      string `json:"kind"`
	App       string `json:"app"`
	Release   string `json:"release"`
	UpdatedAt int64  `json:"updatedAt"`
	Files     []File `json:"files"`
}

// Upsert adds f, replacing an entry with the same name.
func (ix *Index) Upsert(f File) {
	for i := range ix.Files {
		if ix.Files[i].Name == f.Name {
			ix.Files[i] = f
			return
		}
	}
	ix.Files = append(ix.Files, f)
	sort.Slice(ix.Files, func(i, j int) bool { return ix.Files[i].Name < ix.Files[j].Name })
}

// Objects is the blob API the store needs (S3 in production, Mem in tests).
type Objects interface {
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, error) // ErrNotFound for missing keys
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
	Ping(ctx context.Context) error
}

// ErrNotFound is returned for missing objects.
var ErrNotFound = errors.New("not found")

// Store keeps artifacts and their per-release indexes.
type Store struct {
	o Objects

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	ixMu   sync.Mutex
	ixMemo map[string]memo
}

type memo struct {
	ix  *Index
	at  time.Time
	err error
}

// NewStore wraps an Objects implementation.
func NewStore(o Objects) *Store {
	return &Store{o: o, locks: map[string]*sync.Mutex{}, ixMemo: map[string]memo{}}
}

// Prefix returns "<project>/<kind>/<app>/<release>/" with app and release path-escaped.
func Prefix(project int64, kind, app, release string) string {
	return fmt.Sprintf("%d/%s/%s/%s/", project, kind, url.PathEscape(app), url.PathEscape(release))
}

// ObjectKey returns the key of a file inside a release prefix.
func ObjectKey(project int64, kind, app, release, name string) string {
	return Prefix(project, kind, app, release) + name
}

// Ping checks the blob store.
func (s *Store) Ping(ctx context.Context) error { return s.o.Ping(ctx) }

// PutFile uploads a local file.
func (s *Store) PutFile(ctx context.Context, key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return s.o.Put(ctx, key, f, st.Size(), contentType)
}

// PutBytes uploads data.
func (s *Store) PutBytes(ctx context.Context, key string, data []byte, contentType string) error {
	return s.o.Put(ctx, key, bytes.NewReader(data), int64(len(data)), contentType)
}

// Get downloads an object, refusing ones larger than limit bytes.
func (s *Store) Get(ctx context.Context, key string, limit int64) ([]byte, error) {
	rc, err := s.o.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("object %s exceeds %d bytes", key, limit)
	}
	return data, nil
}

// GetToFile downloads an object to path (atomically via a .part file).
func (s *Store) GetToFile(ctx context.Context, key, path string) error {
	rc, err := s.o.Open(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List returns all keys under prefix.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	return s.o.List(ctx, prefix)
}

// Delete removes keys (all attempted; first error returned).
func (s *Store) Delete(ctx context.Context, keys ...string) error {
	var first error
	for _, k := range keys {
		if err := s.o.Delete(ctx, k); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Lock serialises read-modify-write of one index within this process.
func (s *Store) Lock(prefix string) func() {
	s.mu.Lock()
	l, ok := s.locks[prefix]
	if !ok {
		l = &sync.Mutex{}
		s.locks[prefix] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

const indexTTL = 10 * time.Second

// LoadIndex returns the index for a release (empty index when none exists). Cached for 10 s.
func (s *Store) LoadIndex(ctx context.Context, project int64, kind, app, release string) (*Index, error) {
	key := Prefix(project, kind, app, release) + "index.json"
	s.ixMu.Lock()
	if m, ok := s.ixMemo[key]; ok && time.Since(m.at) < indexTTL {
		s.ixMu.Unlock()
		return m.ix, m.err
	}
	s.ixMu.Unlock()
	ix, err := s.loadIndex(ctx, key)
	if err == nil {
		ix.Project, ix.Kind, ix.App, ix.Release = project, kind, app, release
	}
	s.ixMu.Lock()
	s.ixMemo[key] = memo{ix: ix, at: time.Now(), err: err}
	s.ixMu.Unlock()
	return ix, err
}

func (s *Store) loadIndex(ctx context.Context, key string) (*Index, error) {
	data, err := s.Get(ctx, key, 64<<20)
	if errors.Is(err, ErrNotFound) {
		return &Index{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ix Index
	if err := json.Unmarshal(data, &ix); err != nil {
		return nil, fmt.Errorf("index %s: %w", key, err)
	}
	return &ix, nil
}

// LoadIndexFresh bypasses the memo (used under Lock before modifying).
func (s *Store) LoadIndexFresh(ctx context.Context, project int64, kind, app, release string) (*Index, error) {
	ix, err := s.loadIndex(ctx, Prefix(project, kind, app, release)+"index.json")
	if err == nil {
		ix.Project, ix.Kind, ix.App, ix.Release = project, kind, app, release
	}
	return ix, err
}

// SaveIndex writes the index and refreshes the memo.
func (s *Store) SaveIndex(ctx context.Context, ix *Index) error {
	ix.UpdatedAt = time.Now().UnixMilli()
	data, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	key := Prefix(ix.Project, ix.Kind, ix.App, ix.Release) + "index.json"
	if err := s.PutBytes(ctx, key, data, "application/json"); err != nil {
		return err
	}
	s.ixMu.Lock()
	s.ixMemo[key] = memo{ix: ix, at: time.Now()}
	s.ixMu.Unlock()
	return nil
}

// DeleteRelease removes every object of (project, kind, app, release) including its index.
func (s *Store) DeleteRelease(ctx context.Context, project int64, kind, app, release string) (int, error) {
	prefix := Prefix(project, kind, app, release)
	unlock := s.Lock(prefix)
	defer unlock()
	keys, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	err = s.Delete(ctx, keys...)
	s.ixMu.Lock()
	delete(s.ixMemo, prefix+"index.json")
	s.ixMu.Unlock()
	return len(keys), err
}

// ListIndexes returns every index of a project, optionally filtered by app / release.
func (s *Store) ListIndexes(ctx context.Context, project int64, app, release string) ([]*Index, error) {
	keys, err := s.List(ctx, strconv.FormatInt(project, 10)+"/")
	if err != nil {
		return nil, err
	}
	var out []*Index
	for _, k := range keys {
		parts := strings.Split(k, "/")
		if len(parts) != 5 || parts[4] != "index.json" {
			continue
		}
		a, err1 := url.PathUnescape(parts[2])
		r, err2 := url.PathUnescape(parts[3])
		if err1 != nil || err2 != nil || (app != "" && a != app) || (release != "" && r != release) {
			continue
		}
		ix, err := s.LoadIndex(ctx, project, parts[1], a, r)
		if err != nil {
			return nil, err
		}
		if len(ix.Files) > 0 {
			out = append(out, ix)
		}
	}
	return out, nil
}
