//go:build integration

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

// TestIntegrationOnboarding: a fresh dev-login user has no orgs, creates one with POST /orgs (ADR-028, contracts §11)
// and can then use the project; the minted ingest key is a real, active key of that project.
func TestIntegrationOnboarding(t *testing.T) {
	pool, ch := itEnv(t)
	t.Cleanup(func() { _ = ch.Close() })
	t.Cleanup(pool.Close) // runs after the fixture cleanups registered below
	ctx := context.Background()
	email := "it-onboard-" + strings.ToLower(store.RandomToken(6)) + "@example.com"
	var orgIDs []int64
	t.Cleanup(func() {
		for _, id := range orgIDs {
			_, _ = pool.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, id)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE email = $1`, email)
	})
	srv := &Server{Store: store.New(ch, pool), Auth: auth.NewPG(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Origins: []string{"http://localhost:5173"}, DevLogin: true, MaxOrgsPerUser: 2, PublicIngestURL: "https://ingest.example.com"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	sc := &http.Client{Jar: jar}
	sess := func(method, path string, body any, hdr ...string) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+"/api/v1"+path, rd)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := sc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	if code, b := sess("POST", "/auth/dev-login", map[string]any{"email": email}); code != 200 || !strings.Contains(string(b), `"currentProjectId":null`) {
		t.Fatalf("dev login: %d %s", code, b)
	}
	if code, b := sess("GET", "/me", nil); code != 200 || !strings.Contains(string(b), `"orgs":[]`) || !strings.Contains(string(b), `"currentProjectId":null`) {
		t.Fatalf("me without orgs: %d %s", code, b)
	}
	if code, b := sess("GET", "/me", nil, "X-Trooper-Project", "999999999"); code != 200 {
		t.Fatalf("me with stale header: %d %s", code, b)
	}
	if code, _ := sess("GET", "/services", nil); code != 403 {
		t.Errorf("services without project: %d", code)
	}

	tests := []struct {
		name string
		body any
		want int
	}{
		{"empty name", map[string]any{"name": " ", "project": map[string]any{"name": "Shop"}}, 400},
		{"missing project", map[string]any{"name": "Acme"}, 400},
		{"too long", map[string]any{"name": strings.Repeat("a", 101), "project": map[string]any{"name": "Shop"}}, 400},
		{"first org", map[string]any{"name": "  Acme  ", "project": map[string]any{"name": "My Shop"}}, 201},
		{"second org", map[string]any{"name": "Acme", "project": map[string]any{"name": "日本"}}, 201},
		{"limit", map[string]any{"name": "Third", "project": map[string]any{"name": "x"}}, 409},
	}
	var first model.OrgCreated
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, b := sess("POST", "/orgs", tc.body)
			if code != tc.want {
				t.Fatalf("status %d, want %d: %s", code, tc.want, b)
			}
			if code != 201 {
				return
			}
			var v model.OrgCreated
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatal(err)
			}
			id, _ := strconv.ParseInt(v.Org.ID, 10, 64)
			orgIDs = append(orgIDs, id)
			if v.Org.Name != "Acme" || v.Org.Role != "owner" || v.IngestKey.Key == "" || !strings.HasPrefix(v.IngestKey.Key, v.IngestKey.Prefix) ||
				v.DSN != "https://"+v.IngestKey.Key+"@ingest.example.com/"+v.Project.ID {
				t.Errorf("created: %s", b)
			}
			if first.Org.ID == "" {
				first = v
			}
			h := sha256.Sum256([]byte(v.IngestKey.Key))
			var kind string
			var pid int64
			if err := pool.QueryRow(ctx, `SELECT kind, project_id FROM api_keys WHERE hash = $1 AND revoked_at IS NULL`, h[:]).Scan(&kind, &pid); err != nil ||
				kind != "ingest" || strconv.FormatInt(pid, 10) != v.Project.ID {
				t.Errorf("ingest key row: %v %s %d", err, kind, pid)
			}
			var audits int
			_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE org_id = $1 AND action = 'org.create'`, id).Scan(&audits)
			if audits != 1 {
				t.Errorf("org.create audit rows: %d", audits)
			}
		})
	}
	if first.Project.Slug != "my-shop" {
		t.Errorf("slug: %+v", first.Project)
	}
	var me model.Me
	code, b := sess("GET", "/me", nil)
	_ = json.Unmarshal(b, &me)
	if code != 200 || len(me.Orgs) != 2 || me.CurrentProjectID == nil || *me.CurrentProjectID != first.Project.ID || me.Role != "owner" {
		t.Fatalf("me after onboarding: %d %s", code, b)
	}
	if code, b := sess("GET", "/services?range=15m", nil, "X-Trooper-Project", first.Project.ID); code != 200 {
		t.Errorf("project read: %d %s", code, b)
	}
	fx := newFixture(t, pool, "onboard-apikey")
	if code, _ := (itClient{t: t, base: ts.URL, key: fx.APIKey}).do("POST", "/orgs", map[string]any{"name": "x", "project": map[string]any{"name": "y"}}); code != 403 {
		t.Errorf("api key POST /orgs: %d", code)
	}
}
