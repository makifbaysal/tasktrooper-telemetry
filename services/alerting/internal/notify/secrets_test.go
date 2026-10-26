package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tasktrooper/telemetry/services/pkg/secrets"

	"github.com/tasktrooper/telemetry/services/alerting/internal/store"
)

type fakeSecrets struct {
	rows  map[[2]int64][2][]byte
	calls int
	err   error
}

func (f *fakeSecrets) LoadSecret(_ context.Context, project, id int64) ([]byte, []byte, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	r, ok := f.rows[[2]int64{project, id}]
	if !ok {
		return nil, nil, pgx.ErrNoRows
	}
	return r[0], r[1], nil
}

func TestSecretResolver(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	fs := &fakeSecrets{rows: map[[2]int64][2][]byte{}}
	seal := func(project, id int64, v string) {
		n, c, err := secrets.Seal(key, project, []byte(v))
		if err != nil {
			t.Fatal(err)
		}
		fs.rows[[2]int64{project, id}] = [2][]byte{n, c}
	}
	seal(1, 10, "https://hooks.slack.com/T/B/x")
	seal(1, 11, "Bearer tok")
	seal(1, 12, "oncall@x.io")
	seal(2, 20, "other project")
	// row 21 of project 2 sealed with project 1's AAD: moved between projects → must not decrypt
	n, c, _ := secrets.Seal(key, 1, []byte("stolen"))
	fs.rows[[2]int64{2, 21}] = [2][]byte{n, c}

	r := NewSecretResolver(fs, key)
	now := time.Unix(1_000_000, 0)
	r.Now = func() time.Time { return now }
	ctx := context.Background()

	tests := []struct {
		name      string
		project   int64
		cfg       map[string]any
		want      map[string]any
		permanent bool
	}{
		{"plain values untouched", 1, map[string]any{"webhookUrl": "https://x", "url": "env:HOOK", "n": 3.0},
			map[string]any{"webhookUrl": "https://x", "url": "env:HOOK", "n": 3.0}, false},
		{"top level, nested header, list", 1,
			map[string]any{"webhookUrl": "secret:10", "headers": map[string]any{"Authorization": "secret:11"}, "to": []any{"a@x.io", "secret:12"}},
			map[string]any{"webhookUrl": "https://hooks.slack.com/T/B/x", "headers": map[string]any{"Authorization": "Bearer tok"}, "to": []any{"a@x.io", "oncall@x.io"}}, false},
		{"other project's id", 1, map[string]any{"url": "secret:20"}, nil, true},
		{"project-bound AAD", 2, map[string]any{"url": "secret:21"}, nil, true},
		{"bad ref", 1, map[string]any{"url": "secret:abc"}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ResolveConfig(ctx, tc.project, tc.cfg)
			if tc.want == nil {
				if err == nil || IsPermanent(err) != tc.permanent {
					t.Fatalf("err %v permanent=%v", err, IsPermanent(err))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !equalJSON(got, tc.want) {
				t.Fatalf("got %v", got)
			}
		})
	}

	// cache: 60 s per (project, ref)
	fs.calls = 0
	for range 3 {
		if _, err := r.ResolveConfig(ctx, 1, map[string]any{"u": "secret:10"}); err != nil {
			t.Fatal(err)
		}
	}
	if fs.calls != 0 {
		t.Fatalf("cached value reloaded %d×", fs.calls)
	}
	now = now.Add(61 * time.Second)
	_, _ = r.ResolveConfig(ctx, 1, map[string]any{"u": "secret:10"})
	if fs.calls != 1 {
		t.Fatalf("expired value not reloaded (%d)", fs.calls)
	}

	// database outage is retryable, not permanent
	fs.err = &store.RetryableError{Err: errors.New("connection refused")}
	_, err := r.ResolveConfig(ctx, 1, map[string]any{"u": "secret:11", "v": "secret:99"})
	if err == nil || IsPermanent(err) {
		t.Fatalf("db error should retry: %v", err)
	}
}

func TestDeliverWithSealedChannel(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var c captured
	srv := newCapture(t, &c)
	n, ct, _ := secrets.Seal(key, 7, []byte(srv+"/teams"))
	fs := &fakeSecrets{rows: map[[2]int64][2][]byte{{7, 1}: {n, ct}}}
	cfg, err := NewSecretResolver(fs, key).ResolveConfig(context.Background(), 7, map[string]any{"webhookUrl": "secret:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeliverer(SMTPConfig{}).Deliver(context.Background(), Channel{Name: "t", Type: "teams", Config: cfg}, "firing", firing); err != nil {
		t.Fatal(err)
	}
	if c.path != "/teams" {
		t.Fatalf("delivered to %q", c.path)
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func newCapture(t *testing.T, c *captured) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.path = r.URL.Path
		c.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
