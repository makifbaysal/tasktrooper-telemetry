// Package e2e runs the symbolicator binary against the live dev stack (SeaweedFS S3, Postgres, Redis from
// deploy/docker-compose.yml) and drives it with the trooper-cli binary.
//
//	TT_E2E=1 GOWORK=off go test ./e2e/ -v
package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const apiKey = "tt_dev_api_key" // deploy/postgres/002_seed.sql (project 1)

func build(t *testing.T, dir, out string) string {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", dir, err, b)
	}
	return out
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type cli struct {
	bin, url string
}

func (c cli) run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = append(os.Environ(), "TROOPER_API_URL="+c.url, "TROOPER_API_KEY="+apiKey)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func unzip(t *testing.T, src, dst string) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(filepath.Base(f.Name), "._") {
			continue
		}
		p := filepath.Join(dst, f.Name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		rc, _ := f.Open()
		w, _ := os.Create(p)
		io.Copy(w, rc)
		w.Close()
		rc.Close()
	}
}

func TestLiveStack(t *testing.T) {
	if os.Getenv("TT_E2E") != "1" {
		t.Skip("set TT_E2E=1 (needs docker compose infra: S3 :9002, Postgres :5432, Redis :6379)")
	}
	tmp := t.TempDir()
	svc := build(t, "../cmd/symbolicator", filepath.Join(tmp, "symbolicator"))
	c := cli{bin: build(t, "../../../tools/trooper-cli", filepath.Join(tmp, "trooper-cli"))}

	addr := freePort(t)
	c.url = "http://" + addr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proc := exec.CommandContext(ctx, svc)
	proc.Env = append(os.Environ(), "TT_SYMBOLICATOR_ADDR="+addr, "TT_SYMBOLICATOR_CACHE_DIR="+filepath.Join(tmp, "cache"))
	var logs bytes.Buffer
	proc.Stdout, proc.Stderr = &logs, &logs
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			t.Logf("symbolicator logs:\n%s", logs.String())
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(c.url + "/healthz")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("symbolicator not healthy: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	app, rel := fmt.Sprintf("e2e-shop-%d", time.Now().UnixNano()), "1.4.0 (212)" // iOS SDK release format
	ar := []string{"--app", app, "--release", rel}
	cleanup := func() {
		c.run(t, "", append([]string{"delete"}, ar...)...)
		opt, _ := redis.ParseURL("redis://localhost:6379/0")
		rc := redis.NewClient(opt)
		rc.Del(context.Background(), "ttsym:gen:1:"+app+":"+rel)
		rc.Close()
	}
	defer cleanup()

	// auth
	if code, _, _ := (cli{bin: c.bin, url: c.url}).runWithKey(t, "wrong", "list"); code != 3 {
		t.Errorf("bad key exit = %d, want 3", code)
	}

	// uploads through the CLI
	dsymDir := filepath.Join(tmp, "dsym")
	unzip(t, "../testdata/ios/ShopApp.dSYM.zip", dsymDir)
	for _, args := range [][]string{
		{"upload-sourcemaps", "../testdata/js/dist"},
		{"upload-dsym", filepath.Join(dsymDir, "ShopApp.dSYM")},
		{"upload-mapping", "../testdata/android/mapping.txt"},
		{"upload-flutter-symbols", "../testdata/flutter/shop-linux.symbols", "../testdata/flutter/shop.symbols"},
		{"upload-ndk-symbols", "../testdata/ndk/libshop.so"},
	} {
		code, out, errOut := c.run(t, "", append(args, ar...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
		t.Logf("%s", strings.TrimSpace(out))
	}
	code, out, _ := c.run(t, "", append([]string{"list"}, ar...)...)
	if code != 0 || strings.Count(out, app) != 7 {
		t.Errorf("list (exit %d):\n%s", code, out)
	}

	for _, tc := range []struct {
		name, platform, fixture string
		want                    []string
	}{
		{"javascript", "javascript", "js/stack-chrome.txt", []string{"at validatePrice (../../src/cart.ts:11:", "at computeTotal (../../src/cart.ts:4:", "at checkout (../../src/app.ts:4:"}},
		{"android", "android", "android/stack.txt", []string{"at com.example.shop.PriceValidator.fail(PriceValidator.java:12)", "at com.example.shop.PriceValidator.validate(PriceValidator.java:6)", "at com.example.shop.Main.main(Main.java:7)"}},
		{"ios", "ios", "ios/crash.txt", []string{"validate_price (in ShopApp) (helper.c:6)", "compute_total (in ShopApp) (helper.c:14)", "main (in ShopApp) (main.swift:12)"}},
		{"ios all threads", "ios", "ios/crash-threads.txt", []string{"Thread 1 Crashed:", "validate_price (in ShopApp) (helper.c:6)"}},
		{"android ndk", "android", "ndk/tombstone.txt", []string{"libshop.so (validate_price) (shop.c:5) [inlined]", "libshop.so (compute_total) (shop.c:13)"}},
		{"flutter", "flutter", "flutter/trace-android.txt", []string{"PriceValidator.validate (", "main.dart:4:7", "CartService.computeTotal ("}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, _ := os.ReadFile("../testdata/" + tc.fixture)
			code, out, errOut := c.run(t, string(stack), append([]string{"symbolicate", "--stdin", "--json", "--platform", tc.platform}, ar...)...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			var res struct {
				Stack        string `json:"stack"`
				Symbolicated bool   `json:"symbolicated"`
				Frames       []struct {
					Fn    string `json:"fn"`
					InApp bool   `json:"inApp"`
				} `json:"frames"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Symbolicated {
				t.Fatalf("response: %v\n%s", err, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Stack, w) {
					t.Errorf("missing %q in\n%s", w, res.Stack)
				}
			}
		})
	}

	// Swift demangling when swift-demangle is available on this host
	stack, _ := os.ReadFile("../testdata/ios/crash.txt")
	_, out, _ = c.run(t, string(stack), append([]string{"symbolicate", "--stdin"}, ar...)...)
	if _, err := exec.LookPath("xcrun"); err == nil && !strings.Contains(out, "closure #1 in CheckoutService.checkout(_:)") {
		t.Errorf("swift names not demangled:\n%s", out)
	}

	// Redis result cache: the same request again is a hit
	body, _ := json.Marshal(map[string]string{"app": app, "release": rel, "platform": "android", "stack": "java.lang.Error\n\tat a.a.a(SourceFile:1)"})
	for i, want := range []string{"miss", "hit"} {
		req, _ := http.NewRequest("POST", c.url+"/v1/symbolicate", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Cache"); got != want {
			t.Errorf("request %d: X-Cache = %q, want %q", i, got, want)
		}
	}

	// cleanup leaves nothing behind
	cleanup()
	code, out, _ = c.run(t, "", append([]string{"list", "--json"}, ar...)...)
	if code != 0 || !strings.Contains(out, `"artifacts":[]`) {
		t.Errorf("after delete (exit %d): %s", code, out)
	}
}

func (c cli) runWithKey(t *testing.T, key string, args ...string) (int, string, string) {
	return cli{bin: c.bin, url: c.url}.run(t, "", append(args, "--key", key)...)
}
