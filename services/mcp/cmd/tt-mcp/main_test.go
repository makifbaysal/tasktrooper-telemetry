// Stdout-purity test: the server speaks MCP over stdin/stdout, so every byte
// it writes to stdout must be a JSON-RPC message (ADR-025, mcp.md §2). This
// runs the real binary as a subprocess with pipes, does a real handshake, and
// asserts nothing else reached stdout.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/mcp/internal/server"
)

// The session runs twice: plain, and with self telemetry on (ADR-027) against a fake collector with SDK debug
// output enabled — the SDK must keep stdout clean too, propagate traceparent to the api and export the tool spans.
func TestStdoutIsProtocolOnly(t *testing.T) {
	bin := buildBinary(t)
	for _, selfTel := range []bool{false, true} {
		name := "plain"
		if selfTel {
			name = "self telemetry"
		}
		t.Run(name, func(t *testing.T) { stdioSession(t, bin, selfTel) })
	}
}

type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
}

func (r *recorder) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.seen, s)
}

func stdioSession(t *testing.T, bin string, selfTel bool) {
	// Fake services/api: /me at startup, /services for the tools/call.
	var apiSeen recorder
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") != "" {
			apiSeen.add("traceparent " + r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v1/me":
			_, _ = io.WriteString(w, `{"currentProjectId":"1","orgs":[{"id":"1"}]}`)
		case "/api/v1/services":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	// Fake ingest gateway: rejects metrics (400, not retried) so the SDK logs errors (TROOPER_DEBUG) — they must go to stderr.
	var collected recorder
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		collected.add(r.URL.Path)
		if r.URL.Path == "/v1/metrics" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer collector.Close()

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), noEnvFile(t),
		"TT_API_URL="+api.URL,
		"TT_API_KEY=tt_api_test_key",
		"TT_LOG_LEVEL=debug", // debug logging on: it must still not touch stdout
		"TROOPER_DSN=", "OTEL_EXPORTER_OTLP_ENDPOINT=",
	)
	if selfTel {
		cmd.Env = append(cmd.Env, "TROOPER_DSN="+strings.Replace(collector.URL, "://", "://tt_test_ingest_key@", 1)+"/2",
			"TROOPER_DEBUG=true")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	// Real MCP handshake over real pipes.
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"stdout-test","version":"0"}}}`)
	writeLine(t, stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	writeLine(t, stdin, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tt_list_services","arguments":{}}}`)

	// Every stdout line must be a JSON-RPC message. Collect them until both
	// responses arrive; a stray print would fail to parse or lack jsonrpc.
	// Errors are collected (not reported) inside the goroutine so it never
	// touches t after the test has finished on the timeout path.
	var responses []map[string]any
	var violations []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.TrimSpace(line) == "" {
				violations = append(violations, "blank line on stdout")
				continue
			}
			var msg map[string]any
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				violations = append(violations, "non-JSON line on stdout: "+line)
				continue
			}
			if msg["jsonrpc"] != "2.0" {
				violations = append(violations, "stdout line without jsonrpc envelope: "+line)
				continue
			}
			if _, isMsg := msg["id"]; isMsg {
				responses = append(responses, msg)
			}
			if len(responses) >= 3 {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for initialize + tools/list + tools/call responses")
	}
	for _, v := range violations {
		t.Error(v)
	}
	if len(responses) < 3 {
		t.Fatalf("got %d response(s), want initialize + tools/list + tools/call (stderr: %q)", len(responses), stderr.String())
	}

	// tools/list response must carry the read-only tool surface (no TT_MCP_ALLOW_WRITE).
	var toolsResp map[string]any // responses may arrive in any order
	for _, r := range responses {
		if id, _ := r["id"].(float64); id == 2 {
			toolsResp = r
		}
	}
	result, _ := toolsResp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != server.ToolCount(false) {
		t.Errorf("tools/list returned %d tools, want %d", len(tools), server.ToolCount(false))
	}

	// Reap the child before reading stderr: os/exec copies stderr through its
	// own goroutine until the process exits, so an earlier read is a data race.
	// Closing stdin ends the stdio session (EOF) and the server shuts down.
	_ = stdin.Close()
	_ = cmd.Wait()

	// The startup log must be on stderr, not stdout.
	if !strings.Contains(stderr.String(), "tt-mcp listening") {
		t.Errorf("startup log missing from stderr: %q", stderr.String())
	}
	// And the API key must never appear on either stream.
	if strings.Contains(stderr.String(), "tt_api_test_key") {
		t.Errorf("API key leaked to stderr")
	}

	// Self telemetry: traceparent on the api call of the tool, spans flushed on shutdown, nothing otherwise.
	if got := apiSeen.has("traceparent /api/v1/services"); got != selfTel {
		t.Errorf("traceparent on tools/call api request = %v, want %v", got, selfTel)
	}
	if got := collected.has("/v1/traces"); got != selfTel {
		t.Errorf("spans exported = %v, want %v (stderr: %q)", got, selfTel, stderr.String())
	}
}

// buildBinary compiles cmd/tt-mcp into a temp dir so the test can exec it.
func buildBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "tt-mcp")
	cmd := exec.Command("go", "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		t.Fatalf("write to server stdin: %v", err)
	}
}

// TestMissingKeyFailsFast: no TT_API_KEY → exit 1 with a stderr message and
// an empty stdout (a broken config must not emit half a protocol stream).
func TestMissingKeyFailsFast(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), noEnvFile(t), "TT_API_URL=http://127.0.0.1:1", "TT_API_KEY=")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		t.Fatal("want non-zero exit without TT_API_KEY")
	}
	if out.Len() != 0 {
		t.Errorf("stdout not empty on config error: %q", out.String())
	}
	if !strings.Contains(errb.String(), "TT_API_KEY") {
		t.Errorf("stderr does not name the missing variable: %q", errb.String())
	}
}

// noEnvFile points tt-mcp at an empty env file, so a developer's ~/.config/tasktrooper/mcp.env (written by the
// installer) cannot change what the test sees.
func noEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.env")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return "TT_MCP_ENV_FILE=" + p
}
