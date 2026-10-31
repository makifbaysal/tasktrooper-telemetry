package apiclient

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testKey = "tt_api_TESTKEY0123456789abcdefghijklmnop"

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestGetErrorMapping is the table for upstream failure → short ToolError.
// Every case also asserts the key never leaks into the message.
func TestGetErrorMapping(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantSub string // substring the message must contain
	}{
		{
			name: "401 says check TT_API_KEY",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			wantSub: "check TT_API_KEY",
		},
		{
			name: "403 says check TT_API_KEY",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantSub: "check TT_API_KEY",
		},
		{
			name: "404 says list endpoints",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantSub: NotFoundHint,
		},
		{
			name: "500 says unavailable without the body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"panic: goroutine 1 [running]: secret internal detail"}`)
			},
			wantSub: "unavailable",
		},
		{
			name: "200 with a broken body maps to unreadable",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"items": [`) // truncated JSON
			},
			wantSub: "unreadable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := New(srv.URL, testKey, time.Second)

			var out any
			err := c.Get(context.Background(), "/api/v1/incidents", url.Values{}, &out)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantSub) {
				t.Errorf("message %q does not contain %q", msg, tc.wantSub)
			}
			if strings.Contains(msg, testKey) {
				t.Errorf("API key leaked into error: %q", msg)
			}
			// No raw upstream body and no stack markers.
			if strings.Contains(msg, "goroutine") || strings.Contains(msg, "panic:") {
				t.Errorf("raw upstream body leaked: %q", msg)
			}
			if len(msg) > 200 {
				t.Errorf("error too long (%d bytes): %q", len(msg), msg)
			}
		})
	}
}

func TestGetTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, testKey, 20*time.Millisecond)

	var out any
	err := c.Get(context.Background(), "/api/v1/services", url.Values{}, &out)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "unavailable") || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("timeout not mapped: %q", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("API key leaked: %q", err)
	}
}

func TestGetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Query().Get("range") != "1h" {
			t.Errorf("range = %q, want 1h", r.URL.Query().Get("range"))
		}
		_, _ = io.WriteString(w, `[{"name":"checkout"}]`)
	}))
	defer srv.Close()
	c := New(srv.URL, testKey, time.Second)

	var out []map[string]string
	err := c.Get(context.Background(), "/api/v1/services", url.Values{"range": {"1h"}}, &out)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(out) != 1 || out[0]["name"] != "checkout" {
		t.Errorf("decoded %v", out)
	}
}

// TestCheck is the startup fail-fast gate: bad key and mismatched ids.
func TestCheck(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantOrg     uint32
		wantProject uint32
		wantSub     string
	}{
		{
			name: "401 says check TT_API_KEY",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			wantSub: "check TT_API_KEY",
		},
		{
			name: "project mismatch names both ids",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"currentProjectId":"2","orgs":[{"id":"1"}]}`)
			},
			wantProject: 1,
			wantSub:     "TT_PROJECT_ID=1",
		},
		{
			name: "org mismatch names the org",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"currentProjectId":"1","orgs":[{"id":"9"}]}`)
			},
			wantOrg: 1,
			wantSub: "TT_ORG_ID=1",
		},
		{
			name: "matching ids pass",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"currentProjectId":"1","orgs":[{"id":"1"}]}`)
			},
			wantOrg:     1,
			wantProject: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := New(srv.URL, testKey, time.Second)
			err := c.Check(context.Background(), discardLogger(), tc.wantOrg, tc.wantProject)
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("message %q does not contain %q", err, tc.wantSub)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("API key leaked: %q", err)
			}
		})
	}
}

func TestCheckUnreachable(t *testing.T) {
	// A closed server: the port is free, so the connection is refused fast.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()
	c := New(base, testKey, time.Second)
	err := c.Check(context.Background(), discardLogger(), 0, 0)
	if err == nil {
		t.Fatal("want unreachable error, got nil")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("message %q does not say unreachable", err)
	}
}
