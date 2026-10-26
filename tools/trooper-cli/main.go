// Command trooper-cli uploads symbolication artifacts (source maps, dSYMs, R8/ProGuard mappings, Flutter
// symbols) to the TaskTrooper symbolicator and symbolicates stacks (contracts.md §12).
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // network / server error
	exitUsage   = 2
	exitAuth    = 3 // 401: missing or invalid TROOPER_API_KEY
	exitPartial = 4 // some files were rejected
)

const usage = `trooper-cli — upload debug artifacts to the TaskTrooper symbolicator and symbolicate stacks

Usage:
  trooper-cli <command> [flags] [args]

Commands:
  upload-sourcemaps <dir>         upload every *.js.map (+ its minified .js) under dir, names relative to dir
  upload-dsym <path>...           upload .dSYM bundles (zipped on the fly), zips, or Mach-O DWARF files;
                                  a directory that is not a .dSYM is searched for *.dSYM bundles
  upload-mapping <mapping.txt>    upload an R8/ProGuard mapping (--kind proguard for ProGuard builds)
  upload-flutter-symbols <path>...  upload Flutter --split-debug-info *.symbols files (or a directory of them)
  upload-ndk-symbols <path>...    upload unstripped Android native libraries (*.so, or a directory of them, e.g.
                                  app/build/intermediates/merged_native_libs/release/out/lib)
  list                            list uploaded artifacts (filter with --app / --release)
  symbolicate                     symbolicate a stack read from --stdin or --file
  delete                          delete all artifacts of --app/--release (optionally one --kind)

Common flags (every command):
  --app NAME        app / service name (RUM resource app, e.g. "shop-web")
  --release VER     release = RUM resource.version / service.version (e.g. "1.4.2+57")
  --url URL         symbolicator base URL     (env TROOPER_API_URL, default http://localhost:8095)
  --key KEY         API key (kind api)        (env TROOPER_API_KEY)
  --json            machine-readable output

Examples:
  trooper-cli upload-sourcemaps --app shop-web --release 1.4.2 dist/
  trooper-cli upload-dsym --app shop-ios --release 1.4.2 build/Shop.app.dSYM
  trooper-cli upload-mapping --app shop-android --release 1.4.2 app/build/outputs/mapping/release/mapping.txt
  pbpaste | trooper-cli symbolicate --stdin --app shop-web --release 1.4.2 --platform javascript

Exit codes: 0 ok · 1 network/server error · 2 usage · 3 unauthorized · 4 some files rejected
`

type opts struct {
	app, release, url, key, platform, kind, file string
	stdin, json, noMinified                      bool
	batchBytes                                   int64
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	cmd := args[0]
	o := opts{url: envOr("TROOPER_API_URL", "http://localhost:8095"), key: os.Getenv("TROOPER_API_KEY"), batchBytes: 40 << 20}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: trooper-cli %s [flags] [args]  (trooper-cli --help)\n", cmd)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.app, "app", "", "app / service name")
	fs.StringVar(&o.release, "release", "", "release (RUM resource.version)")
	fs.StringVar(&o.url, "url", o.url, "symbolicator base URL (TROOPER_API_URL)")
	fs.StringVar(&o.key, "key", o.key, "API key (TROOPER_API_KEY)")
	fs.BoolVar(&o.json, "json", false, "JSON output")
	switch cmd {
	case "symbolicate":
		fs.BoolVar(&o.stdin, "stdin", false, "read the stack from stdin")
		fs.StringVar(&o.file, "file", "", "read the stack from a file")
		fs.StringVar(&o.platform, "platform", "", "javascript | android | ios | flutter (default: auto-detect)")
	case "upload-mapping":
		fs.StringVar(&o.kind, "kind", "r8", "r8 | proguard")
	case "delete", "list":
		fs.StringVar(&o.kind, "kind", "", "sourcemap | dsym | r8 | proguard | dart")
	case "upload-sourcemaps":
		fs.BoolVar(&o.noMinified, "no-minified", false, "upload only *.map files (minified .js gives better function names)")
	case "upload-dsym", "upload-flutter-symbols", "upload-ndk-symbols":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	c := &client{base: strings.TrimRight(o.url, "/"), key: o.key, http: &http.Client{Timeout: 10 * time.Minute}}
	needKey := func() bool {
		if o.key == "" {
			fmt.Fprintln(stderr, "error: API key missing: set TROOPER_API_KEY or pass --key")
			return false
		}
		return true
	}
	needAppRelease := func() bool {
		if o.app == "" || o.release == "" {
			fmt.Fprintln(stderr, "error: --app and --release are required")
			return false
		}
		return true
	}
	var code int
	switch cmd {
	case "upload-sourcemaps":
		if len(pos) != 1 || !needAppRelease() || !needKey() {
			if len(pos) != 1 {
				fmt.Fprintln(stderr, "error: expected exactly one directory")
			}
			return exitUsage
		}
		code = uploadSourcemaps(c, o, pos[0], stdout, stderr)
	case "upload-dsym", "upload-flutter-symbols", "upload-ndk-symbols":
		if len(pos) == 0 || !needAppRelease() || !needKey() {
			if len(pos) == 0 {
				fmt.Fprintln(stderr, "error: expected at least one path")
			}
			return exitUsage
		}
		kind := map[string]string{"upload-dsym": "dsym", "upload-flutter-symbols": "dart", "upload-ndk-symbols": "ndk"}[cmd]
		code = uploadDebugFiles(c, o, kind, pos, stdout, stderr)
	case "upload-mapping":
		if len(pos) != 1 || !needAppRelease() || !needKey() {
			if len(pos) != 1 {
				fmt.Fprintln(stderr, "error: expected exactly one mapping file")
			}
			return exitUsage
		}
		if o.kind != "r8" && o.kind != "proguard" {
			fmt.Fprintln(stderr, "error: --kind must be r8 or proguard")
			return exitUsage
		}
		code = uploadFiles(c, o, o.kind, []upFile{{name: filepath.Base(pos[0]), path: pos[0]}}, stdout, stderr)
	case "list":
		if !needKey() {
			return exitUsage
		}
		code = list(c, o, stdout, stderr)
	case "delete":
		if !needAppRelease() || !needKey() {
			return exitUsage
		}
		code = del(c, o, stdout, stderr)
	case "symbolicate":
		if !needAppRelease() || !needKey() {
			return exitUsage
		}
		var src io.Reader
		switch {
		case o.stdin:
			src = stdin
		case o.file != "":
			f, err := os.Open(o.file)
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return exitUsage
			}
			defer f.Close()
			src = f
		default:
			fmt.Fprintln(stderr, "error: pass --stdin or --file")
			return exitUsage
		}
		code = symbolicate(c, o, src, stdout, stderr)
	}
	return code
}

// parseInterspersed allows flags after positional arguments ("upload-dsym a.dSYM --app x").
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- HTTP client ----

type client struct {
	base, key string
	http      *http.Client
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.msg) }

func (c *client) do(req *http.Request, out any) (int, error) {
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", "trooper-cli/0.1")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if out != nil && len(body) > 0 {
		_ = json.Unmarshal(body, out)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(body))
		}
		return resp.StatusCode, &apiError{resp.StatusCode, e.Error}
	}
	return resp.StatusCode, nil
}

func errCode(err error, stderr io.Writer) int {
	fmt.Fprintln(stderr, "error:", err)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == http.StatusUnauthorized {
		return exitAuth
	}
	return exitFailure
}

// ---- uploads ----

type upFile struct {
	name string // relative name sent to the server
	path string
	data []byte // used instead of path when set (zipped .dSYM)
}

type uploadResp struct {
	Files []struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		Debug []struct {
			ID   string `json:"id"`
			Arch string `json:"arch"`
		} `json:"debug"`
		Minified string `json:"minified"`
		MapID    string `json:"mapId"`
	} `json:"files"`
	Errors []struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	} `json:"errors"`
}

func (f upFile) size() int64 {
	if f.data != nil {
		return int64(len(f.data))
	}
	st, err := os.Stat(f.path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// uploadFiles sends files in batches of at most o.batchBytes (a single larger file goes alone).
func uploadFiles(c *client, o opts, kind string, files []upFile, stdout, stderr io.Writer) int {
	var batches [][]upFile
	var cur []upFile
	var curSize int64
	for _, f := range files {
		s := f.size()
		if len(cur) > 0 && curSize+s > o.batchBytes {
			batches = append(batches, cur)
			cur, curSize = nil, 0
		}
		cur = append(cur, f)
		curSize += s
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	var all uploadResp
	partial := false
	for _, b := range batches {
		var res uploadResp
		status, err := c.uploadBatch(o, kind, b, &res)
		all.Files = append(all.Files, res.Files...)
		all.Errors = append(all.Errors, res.Errors...)
		if err != nil {
			if status == http.StatusBadRequest && len(res.Errors) > 0 {
				partial = true
				continue
			}
			printUpload(all, o, stdout, stderr)
			return errCode(err, stderr)
		}
	}
	printUpload(all, o, stdout, stderr)
	if partial || len(all.Errors) > 0 {
		if len(all.Files) == 0 {
			return exitFailure
		}
		return exitPartial
	}
	return exitOK
}

func printUpload(r uploadResp, o opts, stdout, stderr io.Writer) {
	if o.json {
		json.NewEncoder(stdout).Encode(r)
		return
	}
	for _, f := range r.Files {
		extra := ""
		switch {
		case len(f.Debug) > 0:
			var ids []string
			for _, d := range f.Debug {
				ids = append(ids, d.ID+" ("+d.Arch+")")
			}
			extra = "  " + strings.Join(ids, ", ")
		case f.Minified != "" && strings.HasSuffix(f.Name, ".map"):
			extra = "  → " + f.Minified
		case f.MapID != "":
			extra = "  map id " + f.MapID
		}
		fmt.Fprintf(stdout, "uploaded %s (%s)%s\n", f.Name, human(f.Size), extra)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(stderr, "rejected %s: %s\n", e.Name, e.Error)
	}
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (c *client) uploadBatch(o opts, kind string, files []upFile, out *uploadResp) (int, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for k, v := range map[string]string{"kind": kind, "app": o.app, "release": o.release} {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			for _, f := range files {
				h := textproto.MIMEHeader{}
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.ToSlash(f.name)))
				h.Set("Content-Type", "application/octet-stream")
				w, err := mw.CreatePart(h)
				if err != nil {
					return err
				}
				if f.data != nil {
					_, err = w.Write(f.data)
				} else {
					var fh *os.File
					fh, err = os.Open(f.path)
					if err == nil {
						_, err = io.Copy(w, fh)
						fh.Close()
					}
				}
				if err != nil {
					return err
				}
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequest("POST", c.base+"/v1/artifacts", pr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.do(req, out)
}

func uploadSourcemaps(c *client, o opts, dir string, stdout, stderr io.Writer) int {
	var files []upFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" && p != dir {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".js.map") && !strings.HasSuffix(name, ".mjs.map") && !strings.HasSuffix(name, ".cjs.map") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, upFile{name: filepath.ToSlash(rel), path: p})
		if !o.noMinified {
			js := strings.TrimSuffix(p, ".map")
			if st, err := os.Stat(js); err == nil && st.Mode().IsRegular() && hasSourceMappingURL(js) {
				files = append(files, upFile{name: filepath.ToSlash(strings.TrimSuffix(rel, ".map")), path: js})
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitUsage
	}
	if len(files) == 0 {
		fmt.Fprintf(stderr, "error: no *.js.map files under %s\n", dir)
		return exitUsage
	}
	return uploadFiles(c, o, "sourcemap", files, stdout, stderr)
}

func hasSourceMappingURL(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	st, _ := f.Stat()
	off := st.Size() - 4096
	if off < 0 {
		off = 0
	}
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, off)
	return bytes.Contains(buf[:n], []byte("sourceMappingURL="))
}

// uploadDebugFiles uploads each .dSYM bundle (zipped in memory), zip, Mach-O/ELF file or directory thereof.
func uploadDebugFiles(c *client, o opts, kind string, paths []string, stdout, stderr io.Writer) int {
	var files []upFile
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return exitUsage
		}
		switch {
		case st.IsDir() && strings.HasSuffix(strings.TrimRight(p, "/"), ".dSYM"):
			f, err := zipBundle(p)
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return exitFailure
			}
			files = append(files, f)
		case st.IsDir():
			found := 0
			err := filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() && strings.HasSuffix(d.Name(), ".dSYM") && kind == "dsym" {
					f, err := zipBundle(q)
					if err != nil {
						return err
					}
					files = append(files, f)
					found++
					return filepath.SkipDir
				}
				if !d.IsDir() && (kind == "dart" && strings.HasSuffix(d.Name(), ".symbols") || kind == "ndk" && strings.HasSuffix(d.Name(), ".so")) {
					rel, _ := filepath.Rel(p, q) // keeps the ABI directory: arm64-v8a/libshop.so
					files = append(files, upFile{name: filepath.ToSlash(rel), path: q})
					found++
				}
				return nil
			})
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return exitFailure
			}
			if found == 0 {
				fmt.Fprintf(stderr, "error: nothing to upload under %s\n", p)
				return exitUsage
			}
		default:
			files = append(files, upFile{name: filepath.Base(p), path: p})
		}
	}
	o.batchBytes = 1 // one debug file per request: each may be up to 200 MB
	return uploadFiles(c, o, kind, files, stdout, stderr)
}

// zipBundle zips a .dSYM directory keeping the bundle name as the top-level folder.
func zipBundle(dir string) (upFile, error) {
	dir = strings.TrimRight(dir, "/")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parent := filepath.Dir(dir)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(parent, p)
		if err != nil {
			return err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: filepath.ToSlash(rel), Method: zip.Deflate})
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		return upFile{}, fmt.Errorf("zip %s: %w", dir, err)
	}
	return upFile{name: filepath.Base(dir) + ".zip", data: buf.Bytes()}, nil
}

// ---- list / delete / symbolicate ----

func list(c *client, o opts, stdout, stderr io.Writer) int {
	q := []string{}
	for k, v := range map[string]string{"app": o.app, "release": o.release, "kind": o.kind} {
		if v != "" {
			q = append(q, k+"="+urlEscape(v))
		}
	}
	sort.Strings(q)
	req, _ := http.NewRequest("GET", c.base+"/v1/artifacts?"+strings.Join(q, "&"), nil)
	var out struct {
		Artifacts []struct {
			Kind       string `json:"kind"`
			App        string `json:"app"`
			Release    string `json:"release"`
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			UploadedAt int64  `json:"uploadedAt"`
		} `json:"artifacts"`
	}
	if _, err := c.do(req, &out); err != nil {
		return errCode(err, stderr)
	}
	if o.json {
		json.NewEncoder(stdout).Encode(out)
		return exitOK
	}
	if len(out.Artifacts) == 0 {
		fmt.Fprintln(stdout, "no artifacts")
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tRELEASE\tKIND\tNAME\tSIZE\tUPLOADED")
	for _, a := range out.Artifacts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.App, a.Release, a.Kind, a.Name, human(a.Size),
			time.UnixMilli(a.UploadedAt).Format("2006-01-02 15:04"))
	}
	tw.Flush()
	return exitOK
}

func del(c *client, o opts, stdout, stderr io.Writer) int {
	u := c.base + "/v1/artifacts?app=" + urlEscape(o.app) + "&release=" + urlEscape(o.release)
	if o.kind != "" {
		u += "&kind=" + urlEscape(o.kind)
	}
	req, _ := http.NewRequest("DELETE", u, nil)
	var out struct {
		Deleted int `json:"deleted"`
	}
	if _, err := c.do(req, &out); err != nil {
		return errCode(err, stderr)
	}
	if o.json {
		json.NewEncoder(stdout).Encode(out)
	} else {
		fmt.Fprintf(stdout, "deleted %d objects\n", out.Deleted)
	}
	return exitOK
}

func symbolicate(c *client, o opts, src io.Reader, stdout, stderr io.Writer) int {
	stack, err := io.ReadAll(io.LimitReader(src, 1<<20+1))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitFailure
	}
	if len(stack) > 1<<20 {
		fmt.Fprintln(stderr, "error: stack larger than 1 MB")
		return exitUsage
	}
	if len(bytes.TrimSpace(stack)) == 0 {
		fmt.Fprintln(stderr, "error: empty stack")
		return exitUsage
	}
	body, _ := json.Marshal(map[string]string{"app": o.app, "release": o.release, "platform": o.platform, "stack": string(stack)})
	req, _ := http.NewRequest("POST", c.base+"/v1/symbolicate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var out map[string]any
	if _, err := c.do(req, &out); err != nil {
		return errCode(err, stderr)
	}
	if o.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return exitOK
	}
	s, _ := out["stack"].(string)
	fmt.Fprintln(stdout, strings.TrimRight(s, "\n"))
	if ok, _ := out["symbolicated"].(bool); !ok {
		fmt.Fprintln(stderr, "note: nothing could be symbolicated (no matching artifacts for this app/release)")
	}
	if m, _ := out["missing"].([]any); len(m) > 0 {
		fmt.Fprintf(stderr, "note: no debug file uploaded for: %v\n", m)
	}
	return exitOK
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
