package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetries(t *testing.T) {
	tests := []struct {
		name     string
		codes    []int
		wantErr  int // 0 = success
		wantHits int32
	}{
		{"429 then ok", []int{429, 200}, 0, 2},
		{"503 twice then ok", []int{503, 503, 200}, 0, 3},
		{"404 no retry", []int{404}, 404, 1},
		{"always 500", []int{500, 500, 500}, 500, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := hits.Add(1) - 1
				if r.Header.Get("Authorization") != "Bearer tok" {
					t.Error("missing auth")
				}
				code := tt.codes[min(int(i), len(tt.codes)-1)]
				if code == 429 {
					w.Header().Set("Retry-After", "0")
				}
				w.WriteHeader(code)
				if code == 200 {
					_, _ = w.Write([]byte(`{"ok":true}`))
				}
			}))
			defer srv.Close()
			c := &Client{Base: srv.URL, Auth: Bearer("tok"), MaxAttempts: 3, BaseDelay: time.Millisecond}
			var out struct{ OK bool }
			err := c.Get(context.Background(), "/x", nil, &out)
			if tt.wantErr == 0 && (err != nil || !out.OK) {
				t.Fatalf("err %v out %v", err, out)
			}
			if tt.wantErr != 0 && !IsStatus(err, tt.wantErr) {
				t.Fatalf("want %d, got %v", tt.wantErr, err)
			}
			if hits.Load() != tt.wantHits {
				t.Fatalf("hits %d", hits.Load())
			}
			if err != nil && strings.Contains(err.Error(), "tok") {
				t.Fatal("token leaked into error")
			}
		})
	}
}

func TestContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (&Client{Base: srv.URL}).Get(ctx, "/", nil, nil)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}
