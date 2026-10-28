package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/blob"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/llmprice"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

type obsFake struct {
	fakeStore
	diffT, diffB store.ProfileFilter
	price        model.AIPriceInput
	deleted      string
}

func (f *obsFake) GetService(_ context.Context, _ store.Q, id string) (*model.Service, error) {
	return &model.Service{ID: id, Name: id}, nil
}

func (f *obsFake) ServiceHasAI(_ context.Context, _ store.Q, id string) bool { return id == "chat" }

func (f *obsFake) ReplaySegmentKey(_ context.Context, session string, seg int) (string, error) {
	if session != "s1" || seg != 0 {
		return "", store.ErrNotFound
	}
	return "7/42/2026-10-27/s1/000000.json.gz", nil
}

func (f *obsFake) FlameDiff(_ context.Context, t, b store.ProfileFilter) (*model.FlameDiff, error) {
	f.diffT, f.diffB = t, b
	return &model.FlameDiff{}, nil
}

func (f *obsFake) SetLLMPrice(_ context.Context, in model.AIPriceInput) (*llmprice.Price, error) {
	f.price = in
	return &llmprice.Price{Provider: in.Provider, Model: in.Model, Source: "org"}, nil
}

func (f *obsFake) DeleteLLMPrice(_ context.Context, p, m string) error {
	f.deleted = p + "/" + m
	return nil
}

type memBlobs map[string][]byte

func (m memBlobs) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := m[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func obsServer(t *testing.T, f *obsFake, blobs blob.Reader) http.Handler {
	f.t = t
	s := &Server{Store: f, Auth: auth.Static{"good": {OrgID: 7, ProjectID: 42}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return testNow }, Replays: blobs}
	return s.Handler()
}

func gz(t *testing.T, s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestReplaySegment(t *testing.T) {
	body := gz(t, `{"session_id":"s1","segment":0,"events":[]}`)
	tests := []struct {
		name  string
		blobs blob.Reader
		path  string
		code  int
	}{
		{"not configured", nil, "/api/v1/replays/s1/segments/0", 503},
		{"ok", memBlobs{"7/42/2026-10-27/s1/000000.json.gz": body}, "/api/v1/replays/s1/segments/0", 200},
		{"unknown segment", memBlobs{}, "/api/v1/replays/s1/segments/3", 404},
		{"bad segment", memBlobs{}, "/api/v1/replays/s1/segments/x", 404},
		{"blob expired", memBlobs{}, "/api/v1/replays/s1/segments/0", 404},
	}
	for _, tc := range tests {
		w := do(obsServer(t, &obsFake{}, tc.blobs), "GET", tc.path, "good", "", "Accept-Encoding", "gzip")
		if w.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
			continue
		}
		if tc.code == 200 {
			if w.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(w.Body.Bytes(), body) {
				t.Errorf("%s: body re-encoded (%q, %d bytes)", tc.name, w.Header().Get("Content-Encoding"), w.Body.Len())
			}
		}
	}
}

func TestServiceHasAi(t *testing.T) {
	h := obsServer(t, &obsFake{}, nil)
	for svc, want := range map[string]string{"chat": `"hasAi":true`, "api": `"hasAi":false`} {
		w := do(h, "GET", "/api/v1/services/"+svc, "good", "")
		if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(want)) || !bytes.Contains(w.Body.Bytes(), []byte(`"id":"`+svc+`"`)) {
			t.Errorf("%s: %d %s", svc, w.Code, w.Body)
		}
	}
}

func TestProfileDiffParams(t *testing.T) {
	tests := []struct {
		name, query string
		code        int
		check       func(t, b store.ProfileFilter) bool
	}{
		{"nothing", "", 400, nil},
		{"one version", "&version=v2", 400, nil},
		{"versions", "&baseVersion=v1&version=v2&route=/x&method=get", 200, func(t, b store.ProfileFilter) bool {
			return t.Version == "v2" && b.Version == "v1" && t.From.Equal(b.From) && t.To.Sub(t.From) >= 24*time.Hour && b.Method == "GET" && b.Route == "/x"
		}},
		{"windows", "&baseFrom=1000&baseTo=2000&from=3000&to=4000&version=ignored", 200, func(t, b store.ProfileFilter) bool {
			return t.From.UnixMilli() == 3000 && t.To.UnixMilli() == 4000 && b.From.UnixMilli() == 1000 && b.To.UnixMilli() == 2000 && t.Version == ""
		}},
		{"bad window", "&baseFrom=1000&baseTo=x&from=3000&to=4000", 400, nil},
	}
	for _, tc := range tests {
		f := &obsFake{}
		w := do(obsServer(t, f, nil), "GET", "/api/v1/profiles/diff?service=api&type=cpu"+tc.query, "good", "")
		if w.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
			continue
		}
		if tc.check != nil && !tc.check(f.diffT, f.diffB) {
			t.Errorf("%s: filters %+v / %+v", tc.name, f.diffT, f.diffB)
		}
	}
}

func TestAIPricesRoles(t *testing.T) {
	// api key = admin: allowed, named audit entry
	f := &obsFake{}
	h := obsServer(t, f, nil)
	w := do(h, "PUT", "/api/v1/ai/prices", "good", `{"provider":"openai","model":"gpt-x","inputPerMTok":1,"outputPerMTok":2}`)
	if w.Code != 200 || f.price.Model != "gpt-x" {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if len(f.audit) != 1 || f.audit[0].Action != "ai_prices.update" || f.audit[0].Target != "openai/gpt-x" {
		t.Errorf("audit: %+v", f.audit)
	}
	if w := do(h, "DELETE", "/api/v1/ai/prices?provider=openai&model=GPT-X", "good", ""); w.Code != 204 || f.deleted != "openai/GPT-X" {
		t.Errorf("delete: %d %q", w.Code, f.deleted)
	}

	// member: forbidden (role check in the handler; viewers are already stopped by guard)
	for _, role := range []string{"member", "viewer"} {
		mf := &obsFake{}
		mf.t = t
		s := &Server{Store: mf, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return testNow }}
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := tenant.With(req.Context(), tenant.Tenant{OrgID: 7, ProjectID: 42})
				next.ServeHTTP(w, req.WithContext(withPrincipal(ctx, &principal{UserID: 1, OrgID: 7, ProjectID: 42, Role: role})))
			})
		})
		s.obsRoutes(r)
		for _, m := range []string{"PUT", "DELETE"} {
			w := do(r, m, "/ai/prices?provider=openai&model=x", "", `{"provider":"openai","model":"x","inputPerMTok":1,"outputPerMTok":1}`)
			if w.Code != 403 || mf.price.Model != "" || mf.deleted != "" {
				t.Errorf("%s %s: %d", role, m, w.Code)
			}
		}
	}
}
