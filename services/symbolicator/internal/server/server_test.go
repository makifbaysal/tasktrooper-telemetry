package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/auth"
	"github.com/tasktrooper/telemetry/services/symbolicator/internal/store"
)

const td = "../../testdata/"

type env struct {
	srv   *httptest.Server
	mem   *store.Mem
	cache *MemCache
}

func newEnv(t *testing.T, maxDebug, maxMap int64) *env {
	t.Helper()
	good := sha256.Sum256([]byte("tt_dev_api_key"))
	a := auth.New(func(_ context.Context, h []byte) (int64, error) {
		if bytes.Equal(h, good[:]) {
			return 1, nil
		}
		return 0, auth.ErrUnauthorized
	}, time.Minute)
	mem := store.NewMem()
	cache := NewMemCache()
	s, err := New(Config{Store: store.NewStore(mem), Auth: a, Cache: cache, CacheDir: t.TempDir(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxDebugFile: maxDebug, MaxMap: maxMap,
		Demangle: func([]string) map[string]string { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, mem: mem, cache: cache}
}

type part struct {
	name string // relative file name
	data []byte
}

func (e *env) upload(t *testing.T, key, kind, app, release string, files ...part) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("kind", kind)
	mw.WriteField("app", app)
	mw.WriteField("release", release)
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, f.name))
		w, _ := mw.CreatePart(h)
		w.Write(f.data)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/artifacts", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) symbolicate(t *testing.T, app, release, platform, stack string) (int, Response, string) {
	t.Helper()
	body, _ := json.Marshal(Request{App: app, Release: release, Platform: platform, Stack: stack})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/symbolicate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tt_dev_api_key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out Response
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header.Get("X-Cache")
}

func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(td + p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEndToEndInMemory(t *testing.T) {
	e := newEnv(t, 0, 0)
	const app, rel = "shop", "1.2.3"

	// before any upload: raw stack back, symbolicated=false
	stack := string(read(t, "js/stack-chrome.txt"))
	code, res, _ := e.symbolicate(t, app, rel, "web", stack)
	if code != 200 || res.Symbolicated || res.Stack != stack {
		t.Fatalf("pre-upload: %d %+v", code, res)
	}

	for _, tc := range []struct {
		kind  string
		files []part
	}{
		{"sourcemap", []part{{"assets/app.min.js.map", read(t, "js/dist/assets/app.min.js.map")}, {"assets/app.min.js", read(t, "js/dist/assets/app.min.js")}}},
		{"r8", []part{{"mapping.txt", read(t, "android/mapping.txt")}}},
		{"dsym", []part{{"ShopApp.dSYM.zip", read(t, "ios/ShopApp.dSYM.zip")}}},
		{"dart", []part{{"app.android-arm64.symbols", read(t, "flutter/shop-linux.symbols")}, {"app.ios-arm64.symbols", read(t, "flutter/shop.symbols")}}},
	} {
		code, out := e.upload(t, "tt_dev_api_key", tc.kind, app, rel, tc.files...)
		if code != 201 || out["errors"] != nil {
			t.Fatalf("upload %s: %d %v", tc.kind, code, out)
		}
	}

	for _, tc := range []struct {
		name, platform, stack string
		want                  []string // substrings of the symbolicated stack
	}{
		{"js chrome", "javascript", stack, []string{"at validatePrice (../../src/cart.ts:11:", "at computeTotal (../../src/cart.ts:4:", "at checkout (../../src/app.ts:4:"}},
		{"js firefox auto", "", string(read(t, "js/stack-firefox.txt")), []string{"at validatePrice (../../src/cart.ts:11:"}},
		{"android", "android", string(read(t, "android/stack.txt")), []string{"at com.example.shop.PriceValidator.fail(PriceValidator.java:12)", "at com.example.shop.Main.main(Main.java:7)"}},
		{"ios auto", "", string(read(t, "ios/crash.txt")), []string{"validate_price (in ShopApp) (helper.c:6)", "main (in ShopApp) (main.swift:12)"}},
		{"flutter android", "flutter", string(read(t, "flutter/trace-android.txt")), []string{"PriceValidator.validate (file://", "main.dart:4:"}},
		{"flutter macos", "flutter", string(read(t, "flutter/trace-macos.txt")), []string{"CartService.computeTotal ("}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, res, _ := e.symbolicate(t, app, rel, tc.platform, tc.stack)
			if code != 200 || !res.Symbolicated {
				t.Fatalf("%d %+v", code, res)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Stack, w) {
					t.Errorf("missing %q in:\n%s", w, res.Stack)
				}
			}
		})
	}

	// cache: second call hits; an upload bumps the generation → miss
	_, _, c1 := e.symbolicate(t, app, rel, "android", string(read(t, "android/stack.txt")))
	if c1 != "hit" {
		t.Errorf("expected cache hit, got %q", c1)
	}
	e.upload(t, "tt_dev_api_key", "proguard", app, rel, part{"mapping.txt", read(t, "android/mapping.txt")})
	if _, _, c := e.symbolicate(t, app, rel, "android", string(read(t, "android/stack.txt"))); c != "miss" {
		t.Errorf("expected miss after upload, got %q", c)
	}

	// list
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/artifacts?app=shop&release=1.2.3", nil)
	req.Header.Set("Authorization", "Bearer tt_dev_api_key")
	code, out := do(t, req)
	if arts, _ := out["artifacts"].([]any); code != 200 || len(arts) != 7 {
		t.Errorf("list: %d %v", code, out)
	}

	// delete everything of the release
	req, _ = http.NewRequest("DELETE", e.srv.URL+"/v1/artifacts?app=shop&release=1.2.3", nil)
	req.Header.Set("Authorization", "Bearer tt_dev_api_key")
	if code, out := do(t, req); code != 200 || out["deleted"].(float64) < 10 {
		t.Errorf("delete: %d %v", code, out)
	}
	if k := e.mem.Keys(); len(k) != 0 {
		t.Errorf("left over keys: %v", k)
	}
}

func TestUploadRejects(t *testing.T) {
	e := newEnv(t, 64<<10, 1<<10) // tiny limits
	for _, tc := range []struct {
		name, key, kind, app string
		files                []part
		code                 int
	}{
		{"no key", "", "r8", "a", []part{{"m.txt", []byte("x")}}, 401},
		{"bad key", "nope", "r8", "a", []part{{"m.txt", []byte("x")}}, 401},
		{"bad kind", "tt_dev_api_key", "zip", "a", []part{{"m.txt", []byte("x")}}, 400},
		{"no app", "tt_dev_api_key", "r8", "", []part{{"m.txt", []byte("x")}}, 400},
		{"no files", "tt_dev_api_key", "r8", "a", nil, 400},
		{"traversal is cleaned", "tt_dev_api_key", "r8", "a", []part{{"../../etc/mapping.txt", []byte("a.B -> a:\n")}}, 201},
		{"garbage mapping", "tt_dev_api_key", "r8", "a", []part{{"m.txt", []byte("hello")}}, 400},
		{"map too large", "tt_dev_api_key", "sourcemap", "a", []part{{"x.js.map", bytes.Repeat([]byte(" "), 2<<10)}}, 400},
		{"debug too large", "tt_dev_api_key", "dsym", "a", []part{{"x", bytes.Repeat([]byte{0}, 100<<10)}}, 413},
		{"garbage dsym", "tt_dev_api_key", "dsym", "a", []part{{"x.zip", []byte("PK\x03\x04garbage")}}, 400},
		{"not debug file", "tt_dev_api_key", "dart", "a", []part{{"x.symbols", []byte("\x7fELF nope")}}, 400},
		{"js without sourceMappingURL", "tt_dev_api_key", "sourcemap", "a", []part{{"a.js", []byte("var a=1")}}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := e.upload(t, tc.key, tc.kind, tc.app, "1", tc.files...)
			if code != tc.code {
				t.Errorf("code = %d (%v), want %d", code, out, tc.code)
			}
		})
	}
	for _, k := range e.mem.Keys() {
		if strings.Contains(k, "..") || !strings.HasPrefix(k, "1/r8/a/1/") {
			t.Errorf("unexpected key %q", k)
		}
	}
}

func TestSymbolicateRejects(t *testing.T) {
	e := newEnv(t, 0, 0)
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{`, 400},
		{`{"app":"a","release":"1","stack":""}`, 400},
		{`{"app":"","release":"1","stack":"x"}`, 400},
		{`{"app":"a","release":"1","platform":"cobol","stack":"x"}`, 400},
		{`{"app":"a","release":"1","platform":"ios","stack":"Binary Images:\n0xzz"}`, 200},
	} {
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/symbolicate", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer tt_dev_api_key")
		if code, out := do(t, req); code != tc.code {
			t.Errorf("%s → %d %v", tc.body, code, out)
		}
	}
}

func TestMatchSourcemap(t *testing.T) {
	ix := &store.Index{Files: []store.File{
		{Name: "assets/app.min.js.map", Minified: "assets/app.min.js"},
		{Name: "static/js/main.abc.js.map", Minified: "static/js/main.abc.js"},
		{Name: "vendor.js.map", Minified: "vendor.js"},
		{Name: "a/dup.js.map", Minified: "a/dup.js"},
		{Name: "b/dup.js.map", Minified: "b/dup.js"},
	}}
	for url, want := range map[string]string{
		"https://shop.example.com/assets/app.min.js":       "assets/app.min.js.map",
		"https://cdn.example.com/v2/assets/app.min.js?x=1": "assets/app.min.js.map",
		"~/static/js/main.abc.js":                          "static/js/main.abc.js.map",
		"http://localhost:3000/vendor.js#frag":             "vendor.js.map",
		"https://x.com/other/vendor.js":                    "vendor.js.map",
		"app:///assets/app.min.js":                         "assets/app.min.js.map",
		"https://x.com/c/dup.js":                           "", // ambiguous basename
		"https://x.com/nothing.js":                         "",
		"<anonymous>":                                      "",
	} {
		got := ""
		if f := MatchSourcemap(ix, url); f != nil {
			got = f.Name
		}
		if got != want {
			t.Errorf("MatchSourcemap(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestNormalizePlatform(t *testing.T) {
	for _, tc := range []struct{ p, stack, want string }{
		{"web", "", "javascript"},
		{"React-Native", "", "javascript"},
		{"jvm", "", "android"},
		{"apple", "", "ios"},
		{"dart", "", "flutter"},
		{"", "x\n\tat a.b.c(SourceFile:1)", "android"},
		{"", "Thread 0 Crashed:\n\nBinary Images:\n", "ios"},
		{"", "*** *** ***\nbuild_id: 'ab'\n", "flutter"},
		{"", "Error\n    at x (https://a/b.js:1:2)", "javascript"},
		{"cobol", "", ""},
	} {
		if got := NormalizePlatform(tc.p, tc.stack); got != tc.want {
			t.Errorf("NormalizePlatform(%q) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// SDK conventions: iOS release "<short> (<build>)", all-thread Apple reports, Android NDK tombstones, and the
// SDK upload tools' multipart part name "file".
func TestSDKFormats(t *testing.T) {
	e := newEnv(t, 0, 0)
	const rel = "1.4.0 (212)"
	if code, out := e.upload(t, "tt_dev_api_key", "dsym", "shop-ios", rel, part{"ShopApp.dSYM.zip", read(t, "ios/ShopApp.dSYM.zip")}); code != 201 {
		t.Fatalf("dsym upload: %d %v", code, out)
	}
	if code, out := e.upload(t, "tt_dev_api_key", "ndk", "shop-android", rel, part{"arm64-v8a/libshop.so", read(t, "ndk/libshop.so")}); code != 201 {
		t.Fatalf("ndk upload: %d %v", code, out)
	}
	for _, tc := range []struct {
		name, app, platform, fixture string
		want                         []string
	}{
		{"ios all threads", "shop-ios", "ios", "ios/crash-threads.txt", []string{"Thread 1 Crashed:\n0   libsystem_kernel", "validate_price (in ShopApp) (helper.c:6)", "Thread 0:\n0   libsystem_kernel.dylib         0x000000018fc285e8 0x18fc0d000 + 112104\n1   ShopApp                        0x0000000104a1c8b0 main (in ShopApp) (main.swift:12)"}},
		{"ndk tombstone", "shop-android", "android", "ndk/tombstone.txt", []string{"libshop.so (validate_price) (shop.c:5) [inlined]", "libshop.so (compute_total) (shop.c:13)", "(abort+164)"}},
		{"ndk tombstone auto", "shop-android", "", "ndk/tombstone.txt", []string{"libshop.so (compute_total) (shop.c:13)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, res, _ := e.symbolicate(t, tc.app, rel, tc.platform, string(read(t, tc.fixture)))
			if code != 200 || !res.Symbolicated || len(res.Missing) != 0 {
				t.Fatalf("%d %+v", code, res)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Stack, w) {
					t.Errorf("missing %q in:\n%s", w, res.Stack)
				}
			}
		})
	}
	for _, k := range e.mem.Keys() {
		if !strings.HasPrefix(k, "1/dsym/shop-ios/1.4.0%20%28212%29/") && !strings.HasPrefix(k, "1/ndk/shop-android/1.4.0%20%28212%29/") {
			t.Errorf("unexpected key %q", k)
		}
	}
}
