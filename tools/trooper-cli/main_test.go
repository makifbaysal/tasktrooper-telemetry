package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type captured struct {
	fields map[string]string
	files  map[string][]byte
}

// fakeServer records uploads and answers like the symbolicator.
func fakeServer(t *testing.T, reqs *[]captured) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid key"}`))
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/artifacts":
			c := captured{fields: map[string]string{}, files: map[string][]byte{}}
			mr, _ := r.MultipartReader()
			var names []string
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				_, params, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
				data, _ := io.ReadAll(p)
				if fn := params["filename"]; fn != "" {
					c.files[fn] = data
					names = append(names, fn)
				} else {
					c.fields[p.FormName()] = string(data)
				}
			}
			*reqs = append(*reqs, c)
			var files []map[string]any
			var errs []map[string]string
			for _, n := range names {
				if strings.Contains(n, "bad") {
					errs = append(errs, map[string]string{"name": n, "error": "nope"})
				} else {
					files = append(files, map[string]any{"name": n, "size": len(c.files[n])})
				}
			}
			status := 201
			if len(files) == 0 {
				status = 400
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"files": files, "errors": errs})
		case r.URL.Path == "/v1/symbolicate":
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(map[string]any{"stack": "SYM:" + req["platform"] + ":" + req["stack"], "symbolicated": true, "frames": []any{}})
		case r.URL.Path == "/v1/artifacts" && r.Method == "GET":
			w.Write([]byte(`{"artifacts":[{"kind":"r8","app":"a","release":"1","name":"mapping.txt","size":10,"uploadedAt":1700000000000}]}`))
		case r.Method == "DELETE":
			w.Write([]byte(`{"deleted":3}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func write(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCommands(t *testing.T) {
	var reqs []captured
	srv := fakeServer(t, &reqs)
	defer srv.Close()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "dist/assets/app.js"), "x()\n//# sourceMappingURL=app.js.map\n")
	write(t, filepath.Join(dir, "dist/assets/app.js.map"), `{"version":3}`)
	write(t, filepath.Join(dir, "dist/vendor.mjs.map"), `{"version":3}`)
	write(t, filepath.Join(dir, "dist/node_modules/x.js.map"), `{}`)
	write(t, filepath.Join(dir, "dist/readme.txt"), `hi`)
	write(t, filepath.Join(dir, "Shop.app.dSYM/Contents/Info.plist"), "plist")
	write(t, filepath.Join(dir, "Shop.app.dSYM/Contents/Resources/DWARF/Shop"), "dwarf")
	write(t, filepath.Join(dir, "mapping.txt"), "a -> b:\n")
	write(t, filepath.Join(dir, "symbols/app.android-arm64.symbols"), "elf")
	write(t, filepath.Join(dir, "bad/bad.symbols"), "elf")
	write(t, filepath.Join(dir, "lib/arm64-v8a/libshop.so"), "elf")
	write(t, filepath.Join(dir, "lib/arm64-v8a/notes.txt"), "x")

	common := []string{"--app", "shop", "--release", "1.0", "--url", srv.URL, "--key", "good"}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		code  int
		out   string
		check func(t *testing.T, c captured)
	}{
		{"usage", []string{}, "", exitUsage, "Usage:", nil},
		{"help", []string{"--help"}, "", exitOK, "Commands:", nil},
		{"unknown", []string{"frobnicate"}, "", exitUsage, "", nil},
		{"missing app", []string{"upload-mapping", "--key", "good", filepath.Join(dir, "mapping.txt")}, "", exitUsage, "", nil},
		{"unauthorized", append([]string{"list"}, "--url", srv.URL, "--key", "bad"), "", exitAuth, "", nil},
		{"sourcemaps", append([]string{"upload-sourcemaps", filepath.Join(dir, "dist")}, common...), "", exitOK, "uploaded assets/app.js.map",
			func(t *testing.T, c captured) {
				var names []string
				for n := range c.files {
					names = append(names, n)
				}
				sort.Strings(names)
				if strings.Join(names, ",") != "assets/app.js,assets/app.js.map,vendor.mjs.map" || c.fields["kind"] != "sourcemap" ||
					c.fields["app"] != "shop" || c.fields["release"] != "1.0" {
					t.Errorf("got %v %v", names, c.fields)
				}
			}},
		{"dsym zips bundle", append([]string{"upload-dsym", filepath.Join(dir, "Shop.app.dSYM")}, common...), "", exitOK, "",
			func(t *testing.T, c captured) {
				data := c.files["Shop.app.dSYM.zip"]
				zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				if err != nil || c.fields["kind"] != "dsym" {
					t.Fatalf("zip: %v %v", err, c.fields)
				}
				var names []string
				for _, f := range zr.File {
					names = append(names, f.Name)
				}
				sort.Strings(names)
				if strings.Join(names, ",") != "Shop.app.dSYM/Contents/Info.plist,Shop.app.dSYM/Contents/Resources/DWARF/Shop" {
					t.Errorf("zip entries %v", names)
				}
			}},
		{"dsym search dir (flags last)", []string{"upload-dsym", dir, "--app", "shop", "--release", "1.0", "--url", srv.URL, "--key", "good"}, "", exitOK, "", nil},
		{"mapping", append([]string{"upload-mapping", "--kind", "proguard", filepath.Join(dir, "mapping.txt")}, common...), "", exitOK, "",
			func(t *testing.T, c captured) {
				if c.fields["kind"] != "proguard" || string(c.files["mapping.txt"]) != "a -> b:\n" {
					t.Errorf("%v", c.fields)
				}
			}},
		{"bad mapping kind", append([]string{"upload-mapping", "--kind", "x", filepath.Join(dir, "mapping.txt")}, common...), "", exitUsage, "", nil},
		{"flutter symbols", append([]string{"upload-flutter-symbols", filepath.Join(dir, "symbols")}, common...), "", exitOK, "",
			func(t *testing.T, c captured) {
				if c.fields["kind"] != "dart" || c.files["app.android-arm64.symbols"] == nil {
					t.Errorf("%v", c.fields)
				}
			}},
		{"ndk symbols", append([]string{"upload-ndk-symbols", filepath.Join(dir, "lib")}, common...), "", exitOK, "",
			func(t *testing.T, c captured) {
				if c.fields["kind"] != "ndk" || len(c.files) != 1 || c.files["arm64-v8a/libshop.so"] == nil {
					t.Errorf("%v %v", c.fields, len(c.files))
				}
			}},
		{"all rejected", append([]string{"upload-flutter-symbols", filepath.Join(dir, "bad")}, common...), "", exitFailure, "", nil},
		{"partial", append([]string{"upload-flutter-symbols", filepath.Join(dir, "bad"), filepath.Join(dir, "symbols")}, common...), "", exitPartial, "", nil},
		{"symbolicate", append([]string{"symbolicate", "--stdin", "--platform", "android"}, common...), "at a.b(x)", exitOK, "SYM:android:at a.b(x)", nil},
		{"symbolicate needs input", append([]string{"symbolicate"}, common...), "", exitUsage, "", nil},
		{"list", append([]string{"list"}, common...), "", exitOK, "mapping.txt", nil},
		{"delete", append([]string{"delete"}, common...), "", exitOK, "deleted 3 objects", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(reqs)
			var out, errb bytes.Buffer
			code := run(tc.args, strings.NewReader(tc.stdin), &out, &errb)
			if code != tc.code {
				t.Fatalf("code = %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out.String(), errb.String())
			}
			if tc.out != "" && !strings.Contains(out.String(), tc.out) {
				t.Errorf("stdout %q lacks %q", out.String(), tc.out)
			}
			if tc.check != nil {
				if len(reqs) == before {
					t.Fatal("no upload request")
				}
				tc.check(t, reqs[len(reqs)-1])
			}
		})
	}
}
