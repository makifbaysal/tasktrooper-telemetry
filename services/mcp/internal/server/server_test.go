package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
)

// fakeAPI is a minimal stand-in for services/api. It records every request so
// tests can assert that validation fails *before* any upstream call, and that
// query parameters (limits, filters) are wired as designed.
type fakeAPI struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string // "GET /path?query"
	route func(path string) (int, string)
}

func newFakeAPI(t *testing.T, route func(path string) (int, string)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{route: route}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		code, body := route(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) client() *apiclient.Client {
	return apiclient.New(f.URL, "tt_api_test_key", 2*time.Second)
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAPI) lastQuery(t *testing.T) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no API call recorded")
	}
	u := strings.TrimPrefix(f.calls[len(f.calls)-1], "GET ")
	i := strings.Index(u, "?")
	if i < 0 {
		return url.Values{}
	}
	q, err := url.ParseQuery(u[i+1:])
	if err != nil {
		t.Fatalf("parse recorded query %q: %v", u[i+1:], err)
	}
	return q
}

func testServer(f *fakeAPI) *Server {
	return &Server{
		api: f.client(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

// TestInputValidation is the table for ADR-025 rule 2: bad input must fail
// with a short actionable error before any upstream call.
func TestInputValidation(t *testing.T) {
	// Any call at all is a failure: validation must happen locally.
	f := newFakeAPI(t, func(string) (int, string) { return 200, `[]` })
	s := testServer(f)
	ctx := context.Background()

	tests := []struct {
		name    string
		call    func() (*mcp.CallToolResult, any, error)
		wantSub string
	}{
		{"bad range", func() (*mcp.CallToolResult, any, error) {
			return s.listServices(ctx, nil, serviceListInput{RangeInput{Range: sp("2h")}, ""})
		}, "range must be one of"},
		{"bad type", func() (*mcp.CallToolResult, any, error) {
			return s.listServices(ctx, nil, serviceListInput{RangeInput{}, "http,grpc"})
		}, "type must be one of"},
		{"bad level", func() (*mcp.CallToolResult, any, error) {
			return s.searchLogs(ctx, nil, logSearchInput{RangeInput{}, "", "fatal", "", "", "", nil, nil})
		}, "level must be one of"},
		{"bad traceId in logs", func() (*mcp.CallToolResult, any, error) {
			return s.searchLogs(ctx, nil, logSearchInput{RangeInput{}, "", "", "", "not-a-trace", "", nil, nil})
		}, "traceId must be 32 hex chars"},
		{"bad traceId in get_trace", func() (*mcp.CallToolResult, any, error) {
			return s.getTrace(ctx, nil, traceGetInput{TraceID: "zzz"})
		}, "32 hex chars"},
		{"bad state", func() (*mcp.CallToolResult, any, error) {
			return s.listIncidents(ctx, nil, incidentListInput{RangeInput{}, "closed", "", "", "", "", nil, nil})
		}, "state must be one of"},
		{"bad baseline", func() (*mcp.CallToolResult, any, error) {
			return s.endpointRCA(ctx, nil, rcaInput{RangeInput{}, "checkout", "get-orders", "30d"})
		}, "baseline must be one of"},
		{"bad sort", func() (*mcp.CallToolResult, any, error) {
			return s.searchTraces(ctx, nil, traceSearchInput{RangeInput{}, "", "", false, nil, "fastest", nil})
		}, "sort must be"},
		{"negative minMs", func() (*mcp.CallToolResult, any, error) {
			return s.searchTraces(ctx, nil, traceSearchInput{MinMs: ip(-1)})
		}, "minMs must be >= 0"},
		{"empty incident id", func() (*mcp.CallToolResult, any, error) {
			return s.getIncident(ctx, nil, incidentGetInput{ID: "   "})
		}, "id is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := f.count()
			_, _, err := tc.call()
			if err == nil {
				t.Fatal("want validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantSub)
			}
			if f.count() != before {
				t.Errorf("validation error made %d upstream call(s)", f.count()-before)
			}
		})
	}

	// A valid range must be accepted (and reach the API).
	if _, _, err := s.listServices(ctx, nil, serviceListInput{RangeInput{Range: sp("7d")}, ""}); err != nil {
		t.Errorf("valid range 7d rejected: %v", err)
	}
}

// TestQueryWiring asserts the shared knobs (range default, env, limit clamping)
// land on the wire the way contracts §8 and mcp.md §3 require.
func TestQueryWiring(t *testing.T) {
	ctx := context.Background()

	t.Run("range defaults to 1h and env is forwarded", func(t *testing.T) {
		f := newFakeAPI(t, func(string) (int, string) { return 200, `[]` })
		s := testServer(f)
		if _, _, err := s.listIncidents(ctx, nil, incidentListInput{RangeInput: RangeInput{Env: sp("staging")}}); err != nil {
			t.Fatal(err)
		}
		q := f.lastQuery(t)
		if q.Get("range") != "1h" {
			t.Errorf("range = %q, want default 1h", q.Get("range"))
		}
		if q.Get("env") != "staging" {
			t.Errorf("env = %q", q.Get("env"))
		}
	})

	t.Run("limit clamps to the documented max", func(t *testing.T) {
		f := newFakeAPI(t, func(string) (int, string) { return 200, `[]` })
		s := testServer(f)
		if _, _, err := s.searchLogs(ctx, nil, logSearchInput{Limit: ip(9999)}); err != nil {
			t.Fatal(err)
		}
		if got := f.lastQuery(t).Get("limit"); got != "200" {
			t.Errorf("limit = %q, want 200 (max)", got)
		}
	})

	t.Run("limit defaults per tool", func(t *testing.T) {
		f := newFakeAPI(t, func(string) (int, string) { return 200, `[]` })
		s := testServer(f)
		if _, _, err := s.searchTraces(ctx, nil, traceSearchInput{}); err != nil {
			t.Fatal(err)
		}
		if got := f.lastQuery(t).Get("limit"); got != "20" {
			t.Errorf("traces default limit = %q, want 20", got)
		}
		if _, _, err := s.listLoadtestRuns(ctx, nil, loadtestListInput{}); err != nil {
			t.Fatal(err)
		}
		if got := f.lastQuery(t).Get("limit"); got != "20" {
			t.Errorf("loadtest default limit = %q, want 20", got)
		}
	})

	t.Run("trace search filters are forwarded", func(t *testing.T) {
		f := newFakeAPI(t, func(string) (int, string) { return 200, `[]` })
		s := testServer(f)
		in := traceSearchInput{
			RangeInput: RangeInput{Range: sp("24h")},
			Service:    "checkout",
			Endpoint:   "post-orders",
			ErrorsOnly: true,
			MinMs:      ip(500),
			Sort:       "slowest",
		}
		if _, _, err := s.searchTraces(ctx, nil, in); err != nil {
			t.Fatal(err)
		}
		q := f.lastQuery(t)
		for k, want := range map[string]string{
			"range": "24h", "service": "checkout", "endpoint": "post-orders",
			"errors": "true", "minMs": "500", "sort": "slowest",
		} {
			if got := q.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
	})
}

// TestPIIRedaction asserts log messages and trace attrs are masked on the way
// out (mcp.md §4): the model never sees raw emails, IPs or tokens.
func TestPIIRedaction(t *testing.T) {
	ctx := context.Background()

	t.Run("log messages masked", func(t *testing.T) {
		f := newFakeAPI(t, func(string) (int, string) {
			return 200, `{"lines":[{"id":"l1","ts":1,"level":"error","service":"checkout",
				"message":"payment failed for alice@example.com from 10.0.0.8"}],"nextBefore":1}`
		})
		s := testServer(f)
		res, _, err := s.searchLogs(ctx, nil, logSearchInput{})
		if err != nil {
			t.Fatal(err)
		}
		text := resultText(t, res)
		if strings.Contains(text, "alice@example.com") || strings.Contains(text, "10.0.0.8") {
			t.Errorf("PII not masked: %s", text)
		}
		if !strings.Contains(text, "[redacted]") {
			t.Errorf("no mask marker in result: %s", text)
		}
	})

	t.Run("trace attrs and events masked", func(t *testing.T) {
		f := newFakeAPI(t, func(string) (int, string) {
			return 200, `{"id":"0af7651916cd43dd8448eb211c80319c","spans":[{"id":"s1",
				"attrs":{"user.email":"bob@corp.io","http.route":"/orders"},
				"resource":{"host.ip":"192.168.1.5"},
				"events":[{"attrs":{"note":"token Bearer abcDEF-123"}}]}]}`
		})
		s := testServer(f)
		res, _, err := s.getTrace(ctx, nil, traceGetInput{TraceID: "0af7651916cd43dd8448eb211c80319c"})
		if err != nil {
			t.Fatal(err)
		}
		text := resultText(t, res)
		for _, leak := range []string{"bob@corp.io", "192.168.1.5", "abcDEF-123"} {
			if strings.Contains(text, leak) {
				t.Errorf("PII %q not masked: %s", leak, text)
			}
		}
		// Non-PII attribute values survive.
		if !strings.Contains(text, "/orders") {
			t.Errorf("unrelated attr lost: %s", text)
		}
	})
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type %T", res.Content[0])
	}
	return tc.Text
}
