package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/frame"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/jsmap"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/native"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/retrace"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/store"
)

// Request is the POST /v1/symbolicate body.
type Request struct {
	App      string `json:"app"`
	Release  string `json:"release"`
	Platform string `json:"platform"` // javascript|web|browser|react-native · android|jvm · ios|apple · flutter|dart · "" (auto)
	Stack    string `json:"stack"`
}

// Response is the POST /v1/symbolicate answer (contracts.md §12).
type Response struct {
	Stack        string        `json:"stack"`
	Frames       []frame.Frame `json:"frames"`
	Symbolicated bool          `json:"symbolicated"`
	Platform     string        `json:"platform"`
	// Missing lists debug ids (dSYM UUID / Dart build id) of in-app images that have no uploaded debug file.
	Missing []string `json:"missing,omitempty"`
}

func (s *Server) symbolicate(w http.ResponseWriter, r *http.Request) {
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStack+64<<10))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body {app, release, platform, stack}")
		return
	}
	if req.Stack == "" || len(req.Stack) > maxStack {
		writeErr(w, http.StatusBadRequest, "stack is required (≤ 1 MB)")
		return
	}
	if !validLabel(req.App) || !validLabel(req.Release) {
		writeErr(w, http.StatusBadRequest, "app and release are required")
		return
	}
	platform := NormalizePlatform(req.Platform, req.Stack)
	if platform == "" {
		writeErr(w, http.StatusBadRequest, "unknown platform")
		return
	}
	project := projectFrom(r.Context())
	ctx := r.Context()

	var cacheKey string
	if s.cfg.Cache != nil {
		gen := s.cfg.Cache.Gen(ctx, scope(project, req.App, req.Release))
		h := sha256.New()
		for _, p := range []string{"v1", itoa(project), req.App, req.Release, platform, gen, req.Stack} {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
		cacheKey = "ttsym:res:" + hex.EncodeToString(h.Sum(nil))
		if v, ok := s.cfg.Cache.Get(ctx, cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "hit")
			w.Write(v)
			return
		}
	}
	resp := s.run(ctx, project, req, platform)
	body, err := json.Marshal(resp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode")
		return
	}
	body = append(body, '\n')
	if cacheKey != "" && ctx.Err() == nil {
		s.cfg.Cache.Set(ctx, cacheKey, body, resultTTL)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "miss")
	w.Write(body)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

var jvmFrameRe = regexp.MustCompile(`(?m)^\s*at\s+[\w$.]+\.[\w$<>-]+\([^)]*\)\s*$`)

// NormalizePlatform maps platform aliases (and "" via stack sniffing) to javascript|android|ios|flutter.
func NormalizePlatform(p, stack string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "javascript", "js", "web", "browser", "react-native", "reactnative", "node":
		return "javascript"
	case "android", "jvm", "java", "kotlin":
		return "android"
	case "ios", "apple", "macos", "swift", "cocoa", "tvos", "watchos":
		return "ios"
	case "flutter", "dart":
		return "flutter"
	case "":
		switch {
		case native.IsDartTrace(stack):
			return "flutter"
		case native.IsTombstone(stack):
			return "android"
		case native.IsAppleReport(stack):
			return "ios"
		case jvmFrameRe.MatchString(stack) && !strings.Contains(stack, ".js:"):
			return "android"
		default:
			return "javascript"
		}
	}
	return ""
}

func (s *Server) run(ctx context.Context, project int64, req Request, platform string) Response {
	resp := Response{Stack: req.Stack, Frames: []frame.Frame{}, Platform: platform}
	switch platform {
	case "javascript":
		ix, err := s.cfg.Store.LoadIndex(ctx, project, store.KindSourcemap, req.App, req.Release)
		if err != nil {
			s.cfg.Log.Warn("load sourcemap index", "err", err)
			ix = &store.Index{}
		}
		res := jsmap.Symbolicate(req.Stack, func(u string) (*jsmap.Bundle, bool) {
			f := MatchSourcemap(ix, u)
			if f == nil {
				return nil, false
			}
			b, err := s.bundle(ctx, f)
			if err != nil {
				s.cfg.Log.Warn("load sourcemap", "key", f.Key, "err", err)
				return nil, false
			}
			return b, true
		})
		resp.Frames, resp.Symbolicated = res.Frames, res.Symbolicated
		if res.Symbolicated {
			resp.Stack = res.Stack
		}
	case "android":
		if native.IsTombstone(req.Stack) { // NDK crash: native frames by GNU build id
			res := native.SymbolicateTombstone(req.Stack, s.provider(ctx, project, req.App, req.Release))
			resp.Frames, resp.Symbolicated, resp.Missing = res.Frames, res.Symbolicated, res.MissingIDs
			sort.Strings(resp.Missing)
			if res.Symbolicated {
				resp.Stack = res.Stack
			}
			break
		}
		m := s.mapping(ctx, project, req.App, req.Release)
		res := retrace.Retrace(req.Stack, m)
		resp.Frames, resp.Symbolicated = res.Frames, res.Symbolicated
		if res.Symbolicated {
			resp.Stack = res.Stack
		}
	case "ios", "flutter":
		provide := s.provider(ctx, project, req.App, req.Release)
		var res native.Result
		if native.IsDartTrace(req.Stack) {
			res = native.SymbolicateDart(req.Stack, provide)
		} else {
			res = native.SymbolicateApple(req.Stack, provide, s.cfg.Demangle)
		}
		resp.Frames, resp.Symbolicated, resp.Missing = res.Frames, res.Symbolicated, res.MissingIDs
		sort.Strings(resp.Missing)
		if res.Symbolicated {
			resp.Stack = res.Stack
		}
	}
	if resp.Frames == nil {
		resp.Frames = []frame.Frame{}
	}
	return resp
}

// MatchSourcemap finds the map for a frame URL. Rules, best first:
//  1. the URL path equals the minified name ("~/" prefix rule: https://cdn.example.com/assets/app.js matches
//     an upload named assets/app.js(.map) — scheme and host are ignored);
//  2. the URL path ends with "/<minified name>" (longest match wins: uploads made from a sub-directory);
//  3. same file basename (unique match only).
//
// The minified name comes from the map name ("x.js.map" → "x.js"), the map's "file" field, or the
// //# sourceMappingURL of an uploaded minified file.
func MatchSourcemap(ix *store.Index, frameURL string) *store.File {
	p := urlPath(frameURL)
	if p == "" {
		return nil
	}
	var best *store.File
	bestLen := -1
	var byBase []*store.File
	for i := range ix.Files {
		f := &ix.Files[i]
		if !strings.HasSuffix(f.Name, ".map") || f.Minified == "" {
			continue
		}
		min := strings.TrimPrefix(f.Minified, "/")
		switch {
		case p == min:
			return f
		case strings.HasSuffix(p, "/"+min) && len(min) > bestLen:
			best, bestLen = f, len(min)
		case path.Base(p) == path.Base(min):
			byBase = append(byBase, f)
		}
	}
	if best != nil {
		return best
	}
	if len(byBase) == 1 {
		return byBase[0]
	}
	return nil
}

// urlPath returns the path of a frame URL without leading "/", query or fragment.
func urlPath(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if pu, err := url.Parse(u); err == nil && pu.Host != "" {
			rest = pu.Path
		} else if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[j:] // app:///index.bundle, webpack:///src/x.js
		}
		u = rest
	}
	u = strings.TrimPrefix(u, "~/")
	return strings.TrimLeft(u, "/")
}

func (s *Server) bundle(ctx context.Context, f *store.File) (*jsmap.Bundle, error) {
	key := f.Key + "@" + f.SHA256 + "@" + f.MinifiedKey
	if b, ok := s.maps.get(key); ok {
		return b, nil
	}
	data, err := s.cfg.Store.Get(ctx, f.Key, s.cfg.MaxMap)
	if err != nil {
		return nil, err
	}
	m, err := jsmap.Parse(data)
	if err != nil {
		return nil, err
	}
	b := &jsmap.Bundle{Map: m}
	if f.MinifiedKey != "" {
		if js, err := s.cfg.Store.Get(ctx, f.MinifiedKey, s.cfg.MaxMap); err == nil {
			b.Minified = string(js)
		}
	}
	return s.maps.add(key, b), nil
}

// mapping returns the newest r8/proguard mapping of a release (nil when none).
func (s *Server) mapping(ctx context.Context, project int64, app, release string) *retrace.Mapping {
	var newest *store.File
	for _, kind := range []string{store.KindR8, store.KindProguard} {
		ix, err := s.cfg.Store.LoadIndex(ctx, project, kind, app, release)
		if err != nil {
			s.cfg.Log.Warn("load mapping index", "err", err)
			continue
		}
		for i := range ix.Files {
			if newest == nil || ix.Files[i].UploadedAt > newest.UploadedAt {
				newest = &ix.Files[i]
			}
		}
	}
	if newest == nil {
		return nil
	}
	key := newest.Key + "@" + newest.SHA256
	if m, ok := s.mappings.get(key); ok {
		return m
	}
	data, err := s.cfg.Store.Get(ctx, newest.Key, s.cfg.MaxMap)
	if err != nil {
		s.cfg.Log.Warn("load mapping", "key", newest.Key, "err", err)
		return nil
	}
	m, err := retrace.Parse(bytes.NewReader(data))
	if err != nil {
		s.cfg.Log.Warn("parse mapping", "key", newest.Key, "err", err)
		return nil
	}
	return s.mappings.add(key, m)
}

// provider resolves debug ids through the dsym, dart and ndk indexes of a release; memoized per request.
func (s *Server) provider(ctx context.Context, project int64, app, release string) native.Provider {
	byID := map[string]*store.File{}
	for _, kind := range []string{store.KindDSYM, store.KindDart, store.KindNDK} {
		ix, err := s.cfg.Store.LoadIndex(ctx, project, kind, app, release)
		if err != nil {
			s.cfg.Log.Warn("load debug index", "err", err)
			continue
		}
		for i := range ix.Files {
			for _, d := range ix.Files[i].Debug {
				if cur, ok := byID[d.ID]; !ok || ix.Files[i].UploadedAt > cur.UploadedAt {
					byID[d.ID] = &ix.Files[i]
				}
			}
		}
	}
	memo := map[string]*native.Symbolizer{}
	return func(id string) (*native.Symbolizer, bool) {
		if sym, ok := memo[id]; ok {
			return sym, sym != nil
		}
		f := byID[id]
		var sym *native.Symbolizer
		if f != nil {
			var err error
			sym, err = s.symbolizer(ctx, f, id)
			if err != nil {
				s.cfg.Log.Warn("open debug file", "key", f.Key, "id", id, "err", err)
				sym = nil
			}
		}
		memo[id] = sym
		return sym, sym != nil
	}
}

func (s *Server) symbolizer(ctx context.Context, f *store.File, id string) (*native.Symbolizer, error) {
	key := f.SHA256 + "/" + id
	if sym, ok := s.syms.get(key); ok {
		return sym, nil
	}
	p, ok := s.files.get(f.SHA256)
	if !ok || !exists(p) {
		if len(f.SHA256) != 64 {
			return nil, errors.New("bad sha256 in index")
		}
		p = filepath.Join(s.cfg.CacheDir, f.SHA256)
		if !exists(p) {
			if err := s.cfg.Store.GetToFile(ctx, f.Key, p); err != nil {
				return nil, err
			}
		}
		p = s.files.add(f.SHA256, p)
	}
	sym, err := native.Open(p, id)
	if err != nil {
		return nil, err
	}
	return s.syms.add(key, sym), nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
