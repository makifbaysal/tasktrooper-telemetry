package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/jsmap"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/native"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/retrace"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/store"
)

// spooled is one uploaded file part written to a temp file.
type spooled struct {
	name string // sanitized relative name
	path string
	size int64
	sha  string
}

type uploadError struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

const maxParts = 2000

// upload handles POST /v1/artifacts (multipart: kind, app, release, file parts).
// File part names are kept relative ("assets/app.min.js.map"): the raw Content-Disposition filename is used,
// because mime/multipart's FileName() strips directories.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*s.cfg.MaxDebugFile+maxRequestOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}
	tmp, err := os.MkdirTemp(s.cfg.CacheDir, "upload-")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "temp dir")
		return
	}
	defer os.RemoveAll(tmp)

	fields := map[string]string{}
	var files []spooled
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeErr(w, http.StatusRequestEntityTooLarge, "request too large")
			} else {
				writeErr(w, http.StatusBadRequest, "malformed multipart body")
			}
			return
		}
		if i >= maxParts {
			writeErr(w, http.StatusBadRequest, "too many parts")
			return
		}
		fname := rawFileName(part.Header.Get("Content-Disposition"))
		if fname == "" {
			v, _ := io.ReadAll(io.LimitReader(part, 4096))
			fields[part.FormName()] = strings.TrimSpace(string(v))
			continue
		}
		name, ok := sanitizeName(fname)
		if !ok {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid file name %q", fname))
			return
		}
		sp, err := spool(part, tmp, len(files), s.cfg.MaxDebugFile)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			writeErr(w, status, fmt.Sprintf("%s: %v", name, err))
			return
		}
		sp.name = name
		files = append(files, sp)
	}
	kind, app, release := fields["kind"], fields["app"], fields["release"]
	if !store.ValidKind(kind) {
		writeErr(w, http.StatusBadRequest, "kind must be one of sourcemap, dsym, r8, proguard, dart, ndk")
		return
	}
	if !validLabel(app) || !validLabel(release) {
		writeErr(w, http.StatusBadRequest, "app and release are required (≤ 200 printable chars)")
		return
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "no files")
		return
	}
	project := projectFrom(r.Context())
	ctx := r.Context()
	var stored []store.File
	var errs []uploadError
	switch kind {
	case store.KindSourcemap:
		stored, errs = s.processSourcemaps(r, project, app, release, files)
	case store.KindR8, store.KindProguard:
		stored, errs = s.processMappings(r, project, kind, app, release, files)
	case store.KindDSYM, store.KindDart, store.KindNDK:
		stored, errs = s.processDebugFiles(r, project, kind, app, release, files, tmp)
	}
	if len(stored) > 0 {
		prefix := store.Prefix(project, kind, app, release)
		unlock := s.cfg.Store.Lock(prefix)
		ix, err := s.cfg.Store.LoadIndexFresh(ctx, project, kind, app, release)
		if err == nil {
			for _, f := range stored {
				ix.Upsert(f)
			}
			if kind == store.KindSourcemap {
				linkMinified(ix)
			}
			err = s.cfg.Store.SaveIndex(ctx, ix)
		}
		unlock()
		if err != nil {
			s.cfg.Log.Error("save index", "err", err)
			writeErr(w, http.StatusBadGateway, "blob store error")
			return
		}
		if s.cfg.Cache != nil {
			s.cfg.Cache.Bump(ctx, scope(project, app, release))
		}
		s.cfg.Log.Info("artifacts uploaded", "project", project, "kind", kind, "app", app, "release", release, "files", len(stored), "errors", len(errs))
	}
	status := http.StatusCreated
	if len(stored) == 0 {
		status = http.StatusBadRequest
	}
	if stored == nil {
		stored = []store.File{}
	}
	writeJSON(w, status, map[string]any{"kind": kind, "app": app, "release": release, "files": stored, "errors": errs})
}

var errTooLarge = errors.New("file exceeds size limit")

func spool(r io.Reader, dir string, n int, limit int64) (spooled, error) {
	f, err := os.Create(filepath.Join(dir, fmt.Sprintf("part-%d", n)))
	if err != nil {
		return spooled{}, err
	}
	defer f.Close()
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err != nil {
		return spooled{}, err
	}
	if written > limit {
		return spooled{}, errTooLarge
	}
	return spooled{path: f.Name(), size: written, sha: hex.EncodeToString(h.Sum(nil))}, nil
}

// rawFileName returns the unmodified filename parameter of a Content-Disposition header.
func rawFileName(cd string) string {
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// sanitizeName normalizes a relative upload name and rejects traversal.
func sanitizeName(n string) (string, bool) {
	n = strings.ReplaceAll(n, "\\", "/")
	n = strings.TrimPrefix(n, "~/")
	n = path.Clean("/" + n)[1:]
	if n == "" || n == "." || len(n) > 512 || n == "index.json" {
		return "", false
	}
	for _, c := range n {
		if c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	return n, true
}

var sourceMappingURLRe = regexp.MustCompile(`(?m)^[ \t]*//[#@][ \t]*sourceMappingURL=(\S+)[ \t]*$`)

func (s *Server) processSourcemaps(r *http.Request, project int64, app, release string, files []spooled) ([]store.File, []uploadError) {
	var out []store.File
	var errs []uploadError
	now := time.Now().UnixMilli()
	for _, f := range files {
		if f.size > s.cfg.MaxMap {
			errs = append(errs, uploadError{f.name, fmt.Sprintf("exceeds %d MB", s.cfg.MaxMap>>20)})
			continue
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			errs = append(errs, uploadError{f.name, err.Error()})
			continue
		}
		entry := store.File{Name: f.name, Key: store.ObjectKey(project, store.KindSourcemap, app, release, f.name),
			Size: f.size, SHA256: f.sha, UploadedAt: now}
		switch {
		case strings.HasSuffix(f.name, ".map"):
			m, err := jsmap.Parse(data)
			if err != nil {
				errs = append(errs, uploadError{f.name, err.Error()})
				continue
			}
			entry.Minified = minifiedFor(f.name, m.File)
		case isJS(f.name):
			mu := sourceMappingURLRe.FindAllSubmatch(data, -1)
			if len(mu) == 0 {
				errs = append(errs, uploadError{f.name, "minified file has no //# sourceMappingURL; upload its .map instead"})
				continue
			}
			ref := string(mu[len(mu)-1][1])
			if strings.HasPrefix(ref, "data:") || strings.Contains(ref, "://") {
				errs = append(errs, uploadError{f.name, "inline/absolute sourceMappingURL is not supported; upload the .map file"})
				continue
			}
			if i := strings.IndexAny(ref, "?#"); i >= 0 {
				ref = ref[:i]
			}
			entry.SourceMap = path.Clean(path.Join(path.Dir(f.name), ref))
		default:
			errs = append(errs, uploadError{f.name, "expected *.map or minified *.js"})
			continue
		}
		if err := s.cfg.Store.PutFile(r.Context(), entry.Key, f.path, contentType(f.name)); err != nil {
			errs = append(errs, uploadError{f.name, "blob store error"})
			continue
		}
		out = append(out, entry)
	}
	return out, errs
}

func isJS(n string) bool {
	return strings.HasSuffix(n, ".js") || strings.HasSuffix(n, ".mjs") || strings.HasSuffix(n, ".cjs") || strings.HasSuffix(n, ".bundle")
}

func contentType(n string) string {
	switch {
	case strings.HasSuffix(n, ".map"), strings.HasSuffix(n, ".json"):
		return "application/json"
	case isJS(n):
		return "application/javascript"
	}
	return "application/octet-stream"
}

// minifiedFor derives the minified file a map belongs to: "x.js.map" → "x.js", else the map's "file" next to it.
func minifiedFor(mapName, file string) string {
	base := strings.TrimSuffix(mapName, ".map")
	if isJS(base) || file == "" {
		return base
	}
	return path.Clean(path.Join(path.Dir(mapName), path.Base(file)))
}

// linkMinified resolves JS entries (sourceMappingURL) to their maps: the map gets Minified/MinifiedKey.
func linkMinified(ix *store.Index) {
	byName := map[string]int{}
	for i, f := range ix.Files {
		if strings.HasSuffix(f.Name, ".map") {
			byName[f.Name] = i
		}
	}
	for _, f := range ix.Files {
		if !isJS(f.Name) || f.SourceMap == "" {
			continue
		}
		if mi, ok := byName[f.SourceMap]; ok {
			ix.Files[mi].Minified = f.Name
			ix.Files[mi].MinifiedKey = f.Key
		}
	}
}

func (s *Server) processMappings(r *http.Request, project int64, kind, app, release string, files []spooled) ([]store.File, []uploadError) {
	var out []store.File
	var errs []uploadError
	for _, f := range files {
		if f.size > s.cfg.MaxMap {
			errs = append(errs, uploadError{f.name, fmt.Sprintf("exceeds %d MB", s.cfg.MaxMap>>20)})
			continue
		}
		fh, err := os.Open(f.path)
		if err != nil {
			errs = append(errs, uploadError{f.name, err.Error()})
			continue
		}
		m, err := retrace.Parse(fh)
		fh.Close()
		if err != nil {
			errs = append(errs, uploadError{f.name, err.Error()})
			continue
		}
		entry := store.File{Name: f.name, Key: store.ObjectKey(project, kind, app, release, f.name), Size: f.size,
			SHA256: f.sha, UploadedAt: time.Now().UnixMilli(), MapID: m.ID}
		if err := s.cfg.Store.PutFile(r.Context(), entry.Key, f.path, "text/plain"); err != nil {
			errs = append(errs, uploadError{f.name, "blob store error"})
			continue
		}
		out = append(out, entry)
	}
	return out, errs
}

// processDebugFiles stores Mach-O / ELF debug files, unpacking zips (zipped .dSYM bundles).
func (s *Server) processDebugFiles(r *http.Request, project int64, kind, app, release string, files []spooled, tmp string) ([]store.File, []uploadError) {
	var out []store.File
	var errs []uploadError
	add := func(name, p string, size int64, sha string) {
		ids, err := native.Inspect(p)
		if err != nil {
			errs = append(errs, uploadError{name, err.Error()})
			return
		}
		entry := store.File{Name: ids[0].ID + "/" + path.Base(name), Size: size, SHA256: sha, UploadedAt: time.Now().UnixMilli()}
		entry.Key = store.ObjectKey(project, kind, app, release, entry.Name)
		for _, id := range ids {
			entry.Debug = append(entry.Debug, store.DebugID{ID: id.ID, Arch: id.Arch, Format: id.Format})
		}
		if err := s.cfg.Store.PutFile(r.Context(), entry.Key, p, "application/octet-stream"); err != nil {
			errs = append(errs, uploadError{name, "blob store error"})
			return
		}
		out = append(out, entry)
	}
	for _, f := range files {
		if isZip(f.path) {
			n, err := s.unzipDebugFiles(f, tmp, add)
			if err != nil {
				errs = append(errs, uploadError{f.name, err.Error()})
			} else if n == 0 {
				errs = append(errs, uploadError{f.name, "zip contains no Mach-O/ELF debug file"})
			}
			continue
		}
		add(f.name, f.path, f.size, f.sha)
	}
	return out, errs
}

func isZip(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	_, err = io.ReadFull(f, magic[:])
	return err == nil && bytes.Equal(magic[:], []byte("PK\x03\x04"))
}

// unzipDebugFiles extracts DWARF files: entries under *.dSYM/Contents/Resources/DWARF/, or — for zips that are
// not dSYM bundles (e.g. zipped Flutter symbols) — every regular file that parses as Mach-O/ELF.
func (s *Server) unzipDebugFiles(f spooled, tmp string, add func(name, p string, size int64, sha string)) (int, error) {
	zr, err := zip.OpenReader(f.path)
	if err != nil {
		return 0, fmt.Errorf("bad zip: %w", err)
	}
	defer zr.Close()
	hasDSYM := false
	for _, e := range zr.File {
		if strings.Contains(e.Name, ".dSYM/Contents/Resources/DWARF/") {
			hasDSYM = true
			break
		}
	}
	var total int64
	n := 0
	for i, e := range zr.File {
		if e.FileInfo().IsDir() || strings.Contains(e.Name, "__MACOSX/") || strings.HasPrefix(path.Base(e.Name), "._") || path.Base(e.Name) == ".DS_Store" {
			continue
		}
		if hasDSYM && !strings.Contains(e.Name, ".dSYM/Contents/Resources/DWARF/") {
			continue
		}
		if !hasDSYM && strings.HasSuffix(e.Name, ".plist") {
			continue
		}
		if e.UncompressedSize64 > uint64(s.cfg.MaxDebugFile) {
			return n, fmt.Errorf("%s: %w", e.Name, errTooLarge)
		}
		rc, err := e.Open()
		if err != nil {
			return n, err
		}
		sp, err := spool(rc, tmp, 100000+i, s.cfg.MaxDebugFile) // the limit also guards against lying zip headers
		rc.Close()
		if err != nil {
			return n, fmt.Errorf("%s: %w", e.Name, err)
		}
		total += sp.size
		if total > 4*s.cfg.MaxDebugFile {
			return n, errors.New("zip expands beyond limit")
		}
		if !hasDSYM {
			if _, err := native.Inspect(sp.path); err != nil {
				continue // not a debug file
			}
		}
		add(e.Name, sp.path, sp.size, sp.sha)
		n++
	}
	return n, nil
}
