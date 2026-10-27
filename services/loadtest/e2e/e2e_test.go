//go:build integration

// End-to-end: loadtest-ctl (in process, local executor) spawns real loadtest-runner subprocesses against an
// httptest target and a fake ingest-gw. Needs the compose Postgres (make infra && make migrate). Project 9501.
//
//	cd services/loadtest && GOWORK=off go test -tags integration -count=1 -v ./e2e/
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/ctl"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/obs"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

const project = 9501

type target struct {
	srv     *httptest.Server
	orders  atomic.Int64
	gets    atomic.Int64
	badPrev atomic.Int64
	badHdr  atomic.Int64
	failing atomic.Bool
	lastRun atomic.Value
}

func newTarget() *target {
	tg := &target{}
	mux := http.NewServeMux()
	hdr := func(r *http.Request) {
		run := r.Header.Get("x-trooper-loadtest-run")
		if !strings.HasPrefix(r.Header.Get("baggage"), "tt.svc=loadtest:") || run == "" || r.Header.Get("traceparent") == "" {
			tg.badHdr.Add(1)
		}
		tg.lastRun.Store(run)
	}
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		hdr(r)
		if tg.failing.Load() {
			w.WriteHeader(500)
			return
		}
		id := 1000 + tg.orders.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id": %d, "total": 12.5}`, id)
	})
	mux.HandleFunc("GET /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		hdr(r)
		tg.gets.Add(1)
		if tg.failing.Load() {
			w.WriteHeader(500)
			return
		}
		if n, err := strconv.Atoi(r.PathValue("id")); err != nil || n <= 1000 || int64(n) > 1000+tg.orders.Load() {
			tg.badPrev.Add(1)
		}
		time.Sleep(2 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok": true}`)
	})
	tg.srv = httptest.NewServer(mux)
	return tg
}

type ingest struct {
	srv  *httptest.Server
	hits atomic.Int64
	bad  atomic.Int64
}

func newIngest() *ingest {
	in := &ingest{}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" || r.Header.Get("x-trooper-internal") != "it-token" || r.Header.Get("x-trooper-project") != fmt.Sprint(project) {
			in.bad.Add(1)
			w.WriteHeader(401)
			return
		}
		in.hits.Add(1)
	}))
	return in
}

func buildRunner(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "loadtest-runner")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/loadtest-runner")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}
	return bin
}

func setup(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, config.Load().PostgresURL)
	if err != nil {
		t.Skipf("postgres not reachable (make infra && make migrate): %v", err)
	}
	pg := st.Pool()
	clean := func() { _, _ = pg.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, project) }
	clean()
	if _, err := pg.Exec(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'loadtest-e2e')`, project); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, $1, 'loadtest-e2e', 'loadtest-e2e')`, project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clean(); st.Close() })
	return st
}

// uiSpec is shaped like apps/web saveScenario(): levels + derived stages, "METHOD /route" steps, fractional
// weights, request params/headers/body as text, string thresholds.
func uiSpec(base string, holdS int, extra string) string {
	return `{
	  "services": ["orders-api"], "target": "` + base + `", "regions": ["eu-central", "us-east"], "mode": "vus",
	  "levels": [{"name": "Warm-up", "ramp": 1, "hold": ` + fmt.Sprint(holdS) + `, "target": 4}, {"name": "Cool-down", "ramp": 1, "hold": 0, "target": 0}],
	  "stages": [{"duration": 1, "target": 4}],
	  "steps": ["POST /v1/orders", "GET /v1/orders/:id"], "flow": "sequential",
	  "weights": {"POST /v1/orders": 0.5, "GET /v1/orders/:id": 0.5},
	  "requests": {
	    "POST /v1/orders": {"params": "", "headers": "Content-Type: application/json\nX-Client: {{weighted web:54 ios:46}}",
	      "body": "{\n  \"qty\": \"{{int 1-3}}\",\n  \"sku\": \"{{pool.skus}}\"\n}", "pools": [{"name": "skus", "detail": "x", "values": ["A", "B"]}]},
	    "GET /v1/orders/:id": {"params": "id: {{prev \"POST /v1/orders\" .id}}", "headers": "", "body": "", "pools": []}
	  },
	  "thinkTime": 0.05, "progression": "auto", "gate": {"latencyX": 3, "cpu": 0.85` + extra + `},
	  "thresholds": ["http_req_failed rate < 1%", "http_req_duration p(95) < 500 ms"]
	}`
}

type runRow struct {
	State     string
	Error     *string
	Live      []byte
	Summary   []byte
	Passed    *bool
	VUMinutes *float64
}

func getRun(t *testing.T, st *store.Store, id int64) runRow {
	t.Helper()
	var r runRow
	if err := st.Pool().QueryRow(context.Background(), `SELECT state, error, live, summary, thresholds_passed, vu_minutes::float8 FROM loadtest_runs WHERE id = $1`, id).
		Scan(&r.State, &r.Error, &r.Live, &r.Summary, &r.Passed, &r.VUMinutes); err != nil {
		t.Fatal(err)
	}
	return r
}

func queue(t *testing.T, st *store.Store, spec string) int64 {
	t.Helper()
	ctx := context.Background()
	var sid, rid int64
	if err := st.Pool().QueryRow(ctx, `INSERT INTO loadtest_scenarios (project_id, name, spec, source) VALUES ($1, 'e2e', $2::jsonb, 'manual') RETURNING id`, project, spec).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().QueryRow(ctx, `INSERT INTO loadtest_runs (scenario_id, state, started_at, summary) VALUES ($1, 'queued', now(), '{"progress": 0}'::jsonb) RETURNING id`, sid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return rid
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type summary struct {
	Progress    float64          `json:"progress"`
	Rps         float64          `json:"rps"`
	P95         float64          `json:"p95"`
	ErrorRate   float64          `json:"errorRate"`
	Vus         float64          `json:"vus"`
	Requests    int64            `json:"requests"`
	StatusCodes map[string]int64 `json:"statusCodes"`
	Steps       []struct {
		Key      string `json:"key"`
		Requests int64  `json:"count"`
	} `json:"steps"`
	Thresholds []struct {
		Name   string  `json:"name"`
		Passed bool    `json:"passed"`
		Value  float64 `json:"value"`
	} `json:"thresholds"`
	Regions     []struct{ Region, State string } `json:"regions"`
	AbortReason string                           `json:"abortReason"`
	VUMinutes   float64                          `json:"vuMinutes"`
	Warnings    []string                         `json:"warnings"`
}

func TestE2E(t *testing.T) {
	st := setup(t)
	bin := buildRunner(t)
	tg, in := newTarget(), newIngest()
	defer tg.srv.Close()
	defer in.srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := &ctl.LocalExecutor{Bin: bin, Env: map[string]string{
		"TT_POSTGRES_URL": config.Load().PostgresURL, "TT_LOADTEST_TICK": "1s", "TT_LOG_LEVEL": "warn",
		"TT_LOADTEST_INGEST_URL": in.srv.URL, "TT_INTERNAL_TOKEN": "it-token",
	}}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c := ctl.New(ctl.Config{Instance: "e2e", Projects: []int64{project}, Poll: 200 * time.Millisecond, LiveEvery: 500 * time.Millisecond,
		Limits: ctl.Limits{MaxVUs: 100, MaxDuration: 10 * time.Minute, MonthlyMinutes: 1000, AllowLocal: true}}, st, ex, nil, log, obs.New())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = c.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()

	t.Run("passed", func(t *testing.T) {
		id := queue(t, st, uiSpec(tg.srv.URL, 4, ""))
		sawLive := false
		waitFor(t, 60*time.Second, "run to finish", func() bool {
			r := getRun(t, st, id)
			if r.State == "running" && len(r.Live) > 0 {
				var l struct{ Vus, Progress float64 }
				_ = json.Unmarshal(r.Live, &l)
				sawLive = sawLive || (l.Vus > 0 && l.Progress < 1)
			}
			return r.State != "queued" && r.State != "running"
		})
		r := getRun(t, st, id)
		if r.State != "passed" || r.Passed == nil || !*r.Passed {
			t.Fatalf("state %s passed=%v error=%v summary=%s", r.State, r.Passed, r.Error, r.Summary)
		}
		if !sawLive {
			t.Fatal("live was never filled while running")
		}
		var s summary
		if err := json.Unmarshal(r.Summary, &s); err != nil {
			t.Fatal(err)
		}
		if s.Requests < 50 || s.StatusCodes["200"] != s.Requests || len(s.Steps) != 2 || len(s.Regions) != 2 || s.Vus < 4 ||
			len(s.Thresholds) != 2 || !s.Thresholds[0].Passed || s.P95 <= 0 || s.Rps <= 0 || s.Progress != 1 || s.VUMinutes <= 0 {
			t.Fatalf("summary: %s", r.Summary)
		}
		var live struct{ Progress float64 }
		_ = json.Unmarshal(r.Live, &live)
		if live.Progress != 1 || r.VUMinutes == nil || *r.VUMinutes <= 0 {
			t.Fatalf("live %s vu_minutes %v", r.Live, r.VUMinutes)
		}
		if tg.badPrev.Load() != 0 || tg.badHdr.Load() != 0 || tg.lastRun.Load() != fmt.Sprint(id) {
			t.Fatalf("target saw bad prev=%d hdr=%d run=%v", tg.badPrev.Load(), tg.badHdr.Load(), tg.lastRun.Load())
		}
		if in.hits.Load() == 0 || in.bad.Load() != 0 {
			t.Fatalf("ingest hits=%d bad=%d", in.hits.Load(), in.bad.Load())
		}
		var shards int
		_ = st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM loadtest_run_shards WHERE run_id = $1 AND state = 'done'`, id).Scan(&shards)
		if shards != 2 {
			t.Fatalf("done shards = %d", shards)
		}
		used, _ := st.UsageThisMonth(context.Background(), project)
		if used <= 0 || used > 1 { // actual ≈ 4 VUs × ~5 s
			t.Fatalf("usage = %v", used)
		}
	})

	t.Run("user abort", func(t *testing.T) {
		id := queue(t, st, uiSpec(tg.srv.URL, 120, ""))
		waitFor(t, 30*time.Second, "live", func() bool { r := getRun(t, st, id); return r.State == "running" && len(r.Live) > 0 })
		start := time.Now()
		// what the api's POST /loadtests/runs/{id}/stop does (contracts §16)
		if _, err := st.Pool().Exec(context.Background(), `UPDATE loadtest_runs SET abort_requested_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 20*time.Second, "abort", func() bool { return getRun(t, st, id).State != "running" })
		r := getRun(t, st, id)
		var s summary
		_ = json.Unmarshal(r.Summary, &s)
		if r.State != "aborted" || s.AbortReason != "stopped by user" || s.Requests == 0 || s.Progress >= 1 {
			t.Fatalf("state %s summary %s", r.State, r.Summary)
		}
		if time.Since(start) > 15*time.Second {
			t.Fatalf("abort took %v", time.Since(start))
		}
	})

	t.Run("gate auto-abort", func(t *testing.T) {
		tg.failing.Store(true)
		defer tg.failing.Store(false)
		id := queue(t, st, uiSpec(tg.srv.URL, 120, `, "abortErrorRate": 0.5, "minRequests": 5`))
		waitFor(t, 30*time.Second, "gate abort", func() bool { s := getRun(t, st, id).State; return s != "queued" && s != "running" })
		r := getRun(t, st, id)
		var s summary
		_ = json.Unmarshal(r.Summary, &s)
		if r.State != "aborted" || !strings.HasPrefix(s.AbortReason, "gate: error rate") || r.Passed == nil || *r.Passed {
			t.Fatalf("state %s passed %v summary %s", r.State, r.Passed, r.Summary)
		}
	})

	t.Run("rejected target", func(t *testing.T) {
		id := queue(t, st, uiSpec("https://not-verified.example", 2, ""))
		waitFor(t, 10*time.Second, "reject", func() bool { return getRun(t, st, id).State != "queued" })
		r := getRun(t, st, id)
		if r.State != "error" || r.Error == nil || !strings.Contains(*r.Error, "not a verified domain") {
			t.Fatalf("state %s error %v", r.State, r.Error)
		}
	})
}
