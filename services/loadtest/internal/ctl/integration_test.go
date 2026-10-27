//go:build integration

// Integration tests against the compose Postgres (make infra && make migrate). Project/org 9501, cleaned up.
//
//	cd services/loadtest && GOWORK=off go test -tags integration -count=1 ./...
package ctl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/config"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/obs"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

const testProject = 9501

func openStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, config.Load().PostgresURL)
	if err != nil {
		t.Skipf("postgres not reachable (make infra && make migrate): %v", err)
	}
	pg := st.Pool()
	cleanup := func() {
		_, _ = pg.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, testProject) // cascades to project, scenarios, runs, domains, usage
	}
	cleanup()
	if _, err := pg.Exec(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'loadtest-it')`, testProject); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(ctx, `INSERT INTO projects (id, org_id, name, slug) VALUES ($1, $1, 'loadtest-it', 'loadtest-it')`, testProject); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanup(); st.Close() })
	return st
}

func newScenario(t *testing.T, st *store.Store, spec string) int64 {
	t.Helper()
	var id int64
	if err := st.Pool().QueryRow(context.Background(), `INSERT INTO loadtest_scenarios (project_id, name, spec, source)
	  VALUES ($1, 'it', $2::jsonb, 'manual') RETURNING id`, testProject, spec).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// queueRun inserts a run the way the api's CreateRun does.
func queueRun(t *testing.T, st *store.Store, scenario int64) int64 {
	t.Helper()
	var id int64
	if err := st.Pool().QueryRow(context.Background(), `INSERT INTO loadtest_runs (scenario_id, state, started_at, summary)
	  VALUES ($1, 'queued', now(), '{"progress": 0}'::jsonb) RETURNING id`, scenario).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func runRow(t *testing.T, st *store.Store, id int64) (state, errMsg string) {
	t.Helper()
	var e *string
	if err := st.Pool().QueryRow(context.Background(), `SELECT state, error FROM loadtest_runs WHERE id = $1`, id).Scan(&state, &e); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		errMsg = *e
	}
	return
}

type nopExec struct {
	mu   sync.Mutex
	jobs []Job
}

type nopHandle struct{}

func (nopHandle) Stop(context.Context) error           { return nil }
func (nopHandle) Status(context.Context) (bool, error) { return false, nil }
func (n *nopExec) Start(_ context.Context, j Job) (Handle, error) {
	n.mu.Lock()
	n.jobs = append(n.jobs, j)
	n.mu.Unlock()
	return nopHandle{}, nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestIntegrationClaimSkipLocked(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	sc := newScenario(t, st, `{"target":"http://127.0.0.1:1","levels":[{"ramp":0,"hold":5,"target":2}],"steps":["GET /"],"regions":["eu-central","us-east"]}`)
	want := map[int64]bool{}
	for range 6 {
		want[queueRun(t, st, sc)] = true
	}
	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				r, a, err := st.ClaimNext(ctx, fmt.Sprintf("it-%d", i), []int64{testProject}, func(r store.Run) store.Admission {
					time.Sleep(50 * time.Millisecond) // hold the row lock while others try
					a, _ := Admit(r.Spec, Limits{AllowLocal: true}, AdmitInput{})
					return a
				})
				if err == store.ErrNone {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				if a.Reject != "" {
					t.Errorf("rejected: %s", a.Reject)
				}
				mu.Lock()
				claimed[r.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != len(want) {
		t.Fatalf("claimed %d runs, want %d", len(claimed), len(want))
	}
	for id, n := range claimed {
		if n != 1 || !want[id] {
			t.Fatalf("run %d claimed %d times", id, n)
		}
		if s, _ := runRow(t, st, id); s != "running" {
			t.Fatalf("run %d state %s", id, s)
		}
		sh, _ := st.Shards(ctx, id)
		if len(sh) != 2 || sh[0].Share != 0.5 || sh[0].State != "pending" {
			t.Fatalf("shards: %+v", sh)
		}
	}
	// 6 runs × 2 VUs × 5 s = 1 VU-minute reserved
	if used, _ := st.UsageThisMonth(ctx, testProject); used < 0.99 || used > 1.01 {
		t.Fatalf("usage reserved = %v", used)
	}
}

func TestIntegrationAdmissionRejects(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	pg := st.Pool()
	ex := &nopExec{}
	c := New(Config{Instance: "it-rej", Projects: []int64{testProject}, Limits: Limits{MaxVUs: 50, MaxDuration: time.Hour, MonthlyMinutes: 100}}, st, ex, nil, quietLog(), obs.New())

	unverified := queueRun(t, st, newScenario(t, st, `{"target":"https://unverified.example","levels":[{"ramp":0,"hold":60,"target":5}],"steps":["GET /"]}`))
	tooBig := queueRun(t, st, newScenario(t, st, `{"target":"https://ok.example","levels":[{"ramp":0,"hold":60,"target":500}],"steps":["GET /"]}`))
	_, _ = pg.Exec(ctx, `INSERT INTO loadtest_domains (project_id, host, token, verified_at, method) VALUES ($1, 'ok.example', 't', now(), 'http')`, testProject)
	_, _ = pg.Exec(ctx, `INSERT INTO loadtest_usage (project_id, month, vu_minutes) VALUES ($1, date_trunc('month', now() AT TIME ZONE 'UTC')::date, 99)`, testProject)
	overQuota := queueRun(t, st, newScenario(t, st, `{"target":"https://ok.example","levels":[{"ramp":0,"hold":60,"target":5}],"steps":["GET /"]}`))
	ok := queueRun(t, st, newScenario(t, st, `{"target":"https://ok.example","levels":[{"ramp":0,"hold":6,"target":5}],"steps":["GET /"]}`))
	for range 5 {
		if _, err := c.ClaimOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[int64]string{unverified: "not a verified domain", tooBig: "peak of 500 VUs", overQuota: "monthly VU-minutes"} {
		s, e := runRow(t, st, id)
		if s != "error" || !strings.Contains(e, want) {
			t.Errorf("run %d: %s %q, want error %q", id, s, e, want)
		}
	}
	if s, _ := runRow(t, st, ok); s != "running" || len(ex.jobs) != 1 || ex.jobs[0].RunID != ok || ex.jobs[0].Region != "local" || ex.jobs[0].VUCap != 50 {
		t.Fatalf("admitted run: %s jobs=%+v", s, ex.jobs)
	}
	// restart: a new controller with the same instance id marks the orphan as error
	c2 := New(Config{Instance: "it-rej", Projects: []int64{testProject}}, st, ex, nil, quietLog(), nil)
	if err := c2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if s, e := runRow(t, st, ok); s != "error" || !strings.Contains(e, "restarted") {
		t.Fatalf("orphan: %s %q", s, e)
	}
	var abort bool
	_ = pg.QueryRow(ctx, `SELECT abort_requested_at IS NOT NULL FROM loadtest_runs WHERE id = $1`, ok).Scan(&abort)
	if !abort {
		t.Fatal("orphan recovery must ask runners to stop")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestIntegrationDomainVerification(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	pg := st.Pool()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "web.example" && r.URL.Path == "/.well-known/trooper-verify" {
			_, _ = io.WriteString(w, "tok-web")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ins := func(host, token string, requested bool) int64 {
		var id int64
		q := `INSERT INTO loadtest_domains (project_id, host, token, verify_requested_at) VALUES ($1, $2, $3, CASE WHEN $4 THEN now() END) RETURNING id`
		if err := pg.QueryRow(ctx, q, testProject, host, token, requested).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	dnsID, webID, badID, idleID := ins("dns.example", "tok-dns", true), ins("web.example", "tok-web", true), ins("bad.example", "tok-bad", true), ins("idle.example", "tok-idle", false)
	v := &Verifier{DNS: fakeDNS{"dns.example": {"trooper-verify=tok-dns"}, "idle.example": {"trooper-verify=tok-idle"}},
		HTTP: &http.Client{Timeout: 2 * time.Second, Transport: hostRewrite(srv.URL)}}
	c := New(Config{Instance: "it-ver", Projects: []int64{testProject}}, st, &nopExec{}, v, quietLog(), obs.New())
	c.VerifyDomains(ctx)
	check := func(id int64, method string, verified bool, hasErr bool) {
		t.Helper()
		var m, e *string
		var ver bool
		if err := pg.QueryRow(ctx, `SELECT method, last_error, verified_at IS NOT NULL FROM loadtest_domains WHERE id = $1`, id).Scan(&m, &e, &ver); err != nil {
			t.Fatal(err)
		}
		if ver != verified || (verified && (m == nil || *m != method)) || (e != nil) != hasErr {
			t.Fatalf("domain %d: verified=%v method=%v err=%v", id, ver, m, e)
		}
	}
	check(dnsID, "dns", true, false)
	check(webID, "http", true, false)
	check(badID, "", false, true)
	check(idleID, "", false, false) // no verification request → untouched
	// a checked failure is not retried until the next request (or the 1-minute retry window)
	if list, _ := st.PendingVerifications(ctx, []int64{testProject}); len(list) != 0 {
		t.Fatalf("pending after check: %+v", list)
	}
	domains, _ := st.VerifiedDomains(ctx, testProject)
	if !HostAllowed("api.dns.example", domains, false) || !HostAllowed("web.example", domains, false) || HostAllowed("bad.example", domains, false) {
		t.Fatalf("verified domains: %+v", domains)
	}
}
