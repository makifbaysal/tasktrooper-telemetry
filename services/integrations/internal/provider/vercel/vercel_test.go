package vercel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const projectsJSON = `{"projects":[{"id":"prj_web","name":"shop-web","framework":"nextjs"},{"id":"prj_docs","name":"docs"}],"pagination":{"count":2,"next":null}}`

const deploymentsJSON = `{"deployments":[
 {"uid":"dpl_1","name":"shop-web","url":"shop-web-abc.vercel.app","created":1761393600000,"state":"READY","readyState":"READY","target":"production","projectId":"prj_web",
  "meta":{"githubCommitSha":"a1b2c3","githubCommitMessage":"fix: checkout","githubCommitAuthorLogin":"akif"},"creator":{"username":"akif-v"}},
 {"uid":"dpl_2","name":"docs","url":"docs-xyz.vercel.app","created":1761393660000,"state":"BUILDING","target":null,"projectId":"prj_docs","meta":{},"creator":{"username":"bot"}},
 {"uid":"dpl_3","name":"docs","url":"docs-err.vercel.app","created":1761393720000,"readyState":"ERROR","target":"staging","projectId":"prj_docs","creator":{"username":"bot"}}
],"pagination":{"count":3,"next":null}}`

func TestMapDeployment(t *testing.T) {
	var r struct{ Deployments []Deployment }
	if err := json.Unmarshal([]byte(deploymentsJSON), &r); err != nil {
		t.Fatal(err)
	}
	st := &state{projects: map[string]project{"prj_web": {ID: "prj_web", Name: "shop-web"}}}
	pm := map[string]string{"shop-web": "web"}
	tests := []struct {
		i                                              int
		service, env, version, state, author, msg, url string
	}{
		{0, "web", "production", "a1b2c3", "ready", "akif", "fix: checkout", "https://shop-web-abc.vercel.app"},
		{1, "docs", "preview", "dpl_2", "building", "bot", "", "https://docs-xyz.vercel.app"},
		{2, "docs", "staging", "dpl_3", "error", "bot", "", "https://docs-err.vercel.app"},
	}
	for _, tt := range tests {
		m := MapDeployment(r.Deployments[tt.i], pm, st)
		if m.Service != tt.service || m.Env != tt.env || m.Version != tt.version || m.State != tt.state ||
			m.Author != tt.author || m.CommitMessage != tt.msg || m.URL != tt.url || m.Source != "vercel" || m.ExternalID != r.Deployments[tt.i].UID {
			t.Errorf("%d: %+v", tt.i, m)
		}
	}
}

func TestDiscoverCollectAndDrain(t *testing.T) {
	var mu sync.Mutex
	var sinces []string
	var drainBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vtok" || r.URL.Query().Get("teamId") != "team_1" {
			w.WriteHeader(403)
			return
		}
		switch {
		case r.URL.Path == "/v10/projects":
			_, _ = io.WriteString(w, projectsJSON)
		case r.URL.Path == "/v6/deployments":
			mu.Lock()
			sinces = append(sinces, r.URL.Query().Get("since"))
			mu.Unlock()
			_, _ = io.WriteString(w, deploymentsJSON)
		case r.URL.Path == "/v1/drains" && r.Method == "GET":
			_, _ = io.WriteString(w, `{"drains":[]}`)
		case r.URL.Path == "/v1/drains" && r.Method == "POST":
			b, _ := io.ReadAll(r.Body)
			drainBody = string(b)
			_, _ = io.WriteString(w, `{"id":"drn_1"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	p, err := New([]byte(`{"teamId":"team_1","createDrain":true}`), "vtok", Options{
		Base: srv.URL, IntegrationID: 42, PublicIngestURL: "https://ingest.example.com", IngestKey: "ik", DrainSecret: "s3",
	})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Count() != 2 {
		t.Fatalf("count %d", inv.Count())
	}
	var db struct {
		Secret   string
		Delivery struct{ Endpoint string }
	}
	_ = json.Unmarshal([]byte(drainBody), &db)
	if db.Delivery.Endpoint != "https://ingest.example.com/v1/drains/vercel?key=ik&integration=42" || db.Secret != "s3" {
		t.Fatalf("drain body %s", drainBody)
	}
	now := time.UnixMilli(1761393600000 + 2*3600*1000)
	res, err := p.Collect(context.Background(), inv, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deploys) != 3 || res.Deploys[0].Service != "shop-web" {
		t.Fatalf("deploys %+v", res.Deploys)
	}
	if _, err := p.Collect(context.Background(), inv, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// First window: backfill 24 h; second: 1 h before now (newest deploy is older than that).
	want0 := now.Add(-24 * time.Hour).UnixMilli()
	if sinces[0] != itoa(want0) {
		t.Fatalf("since[0] %s want %d", sinces[0], want0)
	}
	if sinces[1] != itoa(1761393720000) {
		t.Fatalf("since[1] %s", sinces[1])
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }
