package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/engine"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
	"github.com/tasktrooper/telemetry/services/loadtest/internal/store"
)

func specJSON(target string, peak, holdS int, extra string) []byte {
	return []byte(`{"target":"` + target + `","mode":"vus","levels":[{"ramp":0,"hold":` + itoa(holdS) + `,"target":` + itoa(peak) + `}],
	  "steps":["GET /a"],"regions":["eu-central","us-east"]` + extra + `}`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestAdmit(t *testing.T) {
	verified := []store.Domain{
		{Host: "shop.com", Method: "dns", Verified: true},
		{Host: "api.other.io", Method: "http", Verified: true},
	}
	lim := Limits{MaxVUs: 100, MaxDuration: 10 * time.Minute, MonthlyMinutes: 1000}
	tests := []struct {
		name   string
		spec   []byte
		lim    Limits
		used   float64
		reject string // substring; "" = admitted
	}{
		{"verified exact", specJSON("https://shop.com", 10, 60, ""), lim, 0, ""},
		{"subdomain of dns-verified", specJSON("https://staging.shop.com", 10, 60, ""), lim, 0, ""},
		{"subdomain of http-verified is not enough", specJSON("https://x.api.other.io", 10, 60, ""), lim, 0, "not a verified domain"},
		{"unverified host", specJSON("https://evil.com", 10, 60, ""), lim, 0, "not a verified domain"},
		{"suffix trick", specJSON("https://notshop.com", 10, 60, ""), lim, 0, "not a verified domain"},
		{"localhost without allow", specJSON("http://localhost:8080", 10, 60, ""), lim, 0, "not a verified domain"},
		{"localhost with allow", specJSON("http://localhost:8080", 10, 60, ""), Limits{AllowLocal: true}, 0, ""},
		{"127.0.0.1 with allow", specJSON("http://127.0.0.1:9", 10, 60, ""), Limits{AllowLocal: true}, 0, ""},
		{"x.localhost with allow", specJSON("http://app.localhost", 10, 60, ""), Limits{AllowLocal: true}, 0, ""},
		{"too many vus", specJSON("https://shop.com", 101, 60, ""), lim, 0, "exceeds the plan limit of 100"},
		{"rps estimate within limit", []byte(`{"target":"https://shop.com","mode":"rps","thinkTime":0,"levels":[{"ramp":0,"hold":60,"target":400}],"steps":["GET /a"]}`), lim, 0, ""},
		{"too long", specJSON("https://shop.com", 10, 3600, ""), lim, 0, "duration 1h0m0s exceeds"},
		{"monthly minutes", specJSON("https://shop.com", 60, 600, ""), lim, 950, "monthly VU-minutes exceeded"},
		{"invalid spec", []byte(`{"target":"https://shop.com"}`), lim, 0, "invalid scenario"},
		{"script needs target", []byte(`{"script":"export default () => {}"}`), lim, 0, "no target"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := Admit(tc.spec, tc.lim, AdmitInput{Domains: verified, UsedMin: tc.used})
			if tc.reject == "" && a.Reject != "" || tc.reject != "" && !strings.Contains(a.Reject, tc.reject) {
				t.Fatalf("reject = %q, want %q", a.Reject, tc.reject)
			}
		})
	}
	a, plan := Admit(specJSON("https://shop.com", 10, 60, ""), lim, AdmitInput{Domains: verified})
	if len(a.Shards) != 2 || a.Shards[0].Share != 0.5 || a.VUMinutes != 10 || plan == nil {
		t.Fatalf("shards/minutes: %+v", a)
	}
	a, _ = Admit([]byte(`{"target":"https://shop.com","stages":[{"duration":60,"target":1}],"steps":["GET /a"]}`), lim, AdmitInput{Domains: verified, Regions: []string{"eu-central"}})
	if len(a.Shards) != 1 || a.Shards[0].Region != "eu-central" {
		t.Fatalf("default regions: %+v", a.Shards)
	}
	a, _ = Admit([]byte(`{"target":"https://shop.com","stages":[{"duration":60,"target":1}],"steps":["GET /a"]}`), lim, AdmitInput{Domains: verified})
	if len(a.Shards) != 1 || a.Shards[0].Region != "local" {
		t.Fatalf("no regions → local: %+v", a.Shards)
	}
}

type fakeDNS map[string][]string

func (f fakeDNS) LookupTXT(_ context.Context, host string) ([]string, error) {
	if v, ok := f[host]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}

func TestVerifier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/trooper-verify" {
			http.NotFound(w, r)
			return
		}
		switch r.Host {
		case "web.example":
			_, _ = io.WriteString(w, "tok-web\n")
		default:
			_, _ = io.WriteString(w, "other")
		}
	}))
	defer srv.Close()
	v := &Verifier{
		DNS:  fakeDNS{"dns.example": {"v=spf1 -all", "trooper-verify=tok-dns"}},
		HTTP: &http.Client{Timeout: 2 * time.Second, Transport: hostRewrite(srv.URL)},
	}
	tests := []struct{ host, token, method string }{
		{"dns.example", "tok-dns", "dns"},
		{"DNS.example.", "tok-dns", "dns"},
		{"web.example", "tok-web", "http"},
		{"dns.example", "wrong", ""},
		{"web.example", "wrong", ""},
		{"nothing.example", "x", ""},
	}
	for _, tc := range tests {
		m, err := v.Check(context.Background(), tc.host, tc.token)
		if m != tc.method || (tc.method == "" && err == nil) {
			t.Errorf("%s/%s → %q %v", tc.host, tc.token, m, err)
		}
	}
}

// hostRewrite sends every request to base while keeping the Host header (so the server can tell domains apart).
type hostRewrite string

func (h hostRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Host = r.URL.Host
	r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(string(h), "http://")
	return http.DefaultTransport.RoundTrip(r2)
}

func shard(t *testing.T, region, state string, st *engine.Stats, live *engine.Live, abort string) store.Shard {
	t.Helper()
	sh := store.Shard{Region: region, State: state, AbortReason: abort}
	if st != nil {
		sh.Stats, _ = json.Marshal(st)
	}
	if live != nil {
		sh.Live, _ = json.Marshal(live)
	}
	return sh
}

func stats(n, errs int, ms float64) *engine.Stats {
	s := engine.NewStats()
	s.Requests, s.Errors = int64(n), int64(errs)
	s.Steps["GET /a"] = &engine.Counts{Requests: int64(n), Errors: int64(errs)}
	for range n {
		s.Hist.Add(ms)
		s.Steps["GET /a"].Hist.Add(ms)
	}
	s.Status["200"] = int64(n - errs)
	s.ElapsedS, s.VUSeconds, s.PeakVUs = 10, 600, 5
	return s
}

func TestMerge(t *testing.T) {
	plan, err := spec.Parse(specJSON("https://shop.com", 10, 10, `,"thresholds":["http_req_failed rate < 1%","http_req_duration p(95) < 500 ms"]`))
	if err != nil {
		t.Fatal(err)
	}
	live := MergeLive([]store.Shard{
		shard(t, "eu", "running", stats(100, 0, 100), &engine.Live{Vus: 5, Rps: 10, P95: 100, Progress: 0.5}, ""),
		shard(t, "us", "running", stats(100, 10, 300), &engine.Live{Vus: 5, Rps: 30, P95: 300, ErrorRate: 0.1, Progress: 0.4}, ""),
	}, plan)
	if live.Live.Vus != 10 || live.Live.Rps != 40 || live.Live.P95 != 300 || live.Live.Progress != 0.4 || live.Live.ErrorRate != 0.075 {
		t.Fatalf("live: %+v", live.Live)
	}
	ths := live.Summary["thresholds"].([]engine.ThresholdResult)
	if len(ths) != 2 || ths[0].Passed {
		t.Fatalf("running thresholds: %+v", ths)
	}

	tests := []struct {
		name      string
		shards    []store.Shard
		userAbort bool
		state     string
	}{
		{"passed", []store.Shard{shard(t, "eu", "done", stats(100, 0, 100), nil, ""), shard(t, "us", "done", stats(100, 0, 200), nil, "")}, false, "passed"},
		{"failed thresholds", []store.Shard{shard(t, "eu", "done", stats(100, 0, 100), nil, ""), shard(t, "us", "done", stats(100, 50, 900), nil, "")}, false, "failed"},
		{"gate abort", []store.Shard{shard(t, "eu", "aborted", stats(10, 9, 100), nil, "gate: error rate 90.0% > 20.0%"), shard(t, "us", "aborted", stats(10, 0, 100), nil, "stopped by user")}, false, "aborted"},
		{"user abort", []store.Shard{shard(t, "eu", "aborted", stats(10, 0, 100), nil, "stopped by user")}, true, "aborted"},
		{"shard error", []store.Shard{shard(t, "eu", "done", stats(10, 0, 100), nil, ""), {Region: "us", State: "error", Error: "runner exited"}}, false, "error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := MergeFinal(tc.shards, plan, tc.userAbort)
			if f.State != tc.state {
				t.Fatalf("state = %s (%+v)", f.State, f)
			}
			if tc.state == "error" && f.Error != "us: runner exited" {
				t.Fatalf("error = %q", f.Error)
			}
			if tc.name == "gate abort" && !strings.HasPrefix(f.Summary.AbortReason, "gate:") {
				t.Fatalf("abort reason %q", f.Summary.AbortReason)
			}
		})
	}
	f := MergeFinal([]store.Shard{shard(t, "eu", "done", stats(100, 0, 100), nil, ""), shard(t, "us", "done", stats(100, 0, 200), nil, "")}, plan, false)
	if f.Summary.Requests != 200 || f.Summary.Rps != 20 || len(f.Summary.Regions) != 2 || f.VUMinutes != 20 || f.Summary.Vus != 10 ||
		len(f.Summary.Steps) != 1 || f.Summary.Steps[0].Requests != 200 || f.Summary.P95 < 190 || f.Summary.P95 > 210 {
		t.Fatalf("final summary: %+v", f.Summary)
	}
	// k6 script shard: summary only
	k6sum, _ := json.Marshal(engine.Summary{Requests: 42, Thresholds: []engine.ThresholdResult{{Name: "x", Passed: false}}})
	f = MergeFinal([]store.Shard{{Region: "local", State: "done", Summary: k6sum}}, nil, false)
	if f.State != "failed" || f.Summary.Requests != 42 {
		t.Fatalf("script merge: %+v", f)
	}
}

func TestRenderJob(t *testing.T) {
	cfg := K8sConfig{Namespace: "tt", Image: "ghcr.io/tasktrooper/loadtest:1.2.3", SecretName: "tt-loadtest", RegionLabel: "topology.kubernetes.io/region",
		ServiceAccount: "loadtest-runner", Env: map[string]string{"TT_LOADTEST_INGEST_URL": "http://ingest-gw:4318"}}
	m := RenderJob(cfg, Job{RunID: 42, ProjectID: 7, Region: "EU Central", Share: 0.5, VUCap: 50, Deadline: 15 * time.Minute})
	b, _ := json.Marshal(m)
	var job struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			BackoffLimit          int `json:"backoffLimit"`
			ActiveDeadlineSeconds int `json:"activeDeadlineSeconds"`
			Template              struct {
				Spec struct {
					RestartPolicy      string            `json:"restartPolicy"`
					ServiceAccountName string            `json:"serviceAccountName"`
					NodeSelector       map[string]string `json:"nodeSelector"`
					Containers         []struct {
						Image   string                         `json:"image"`
						Env     []struct{ Name, Value string } `json:"env"`
						EnvFrom []map[string]map[string]string `json:"envFrom"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &job); err != nil {
		t.Fatal(err)
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" || job.Metadata.Name != "tt-loadtest-42-eu-central" || job.Metadata.Namespace != "tt" ||
		job.Metadata.Labels["trooper.dev/loadtest-run"] != "42" || job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds != 900 {
		t.Fatalf("job meta: %s", b)
	}
	ps := job.Spec.Template.Spec
	if ps.RestartPolicy != "Never" || ps.ServiceAccountName != "loadtest-runner" || ps.NodeSelector["topology.kubernetes.io/region"] != "EU Central" {
		t.Fatalf("pod spec: %s", b)
	}
	env := map[string]string{}
	for _, e := range ps.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["TT_LOADTEST_RUN_ID"] != "42" || env["TT_LOADTEST_SHARE"] != "0.5" || env["TT_LOADTEST_VU_CAP"] != "50" ||
		env["TT_LOADTEST_INGEST_URL"] != "http://ingest-gw:4318" || env["TT_POSTGRES_URL"] != "" {
		t.Fatalf("env: %v", env)
	}
	if ps.Containers[0].EnvFrom[0]["secretRef"]["name"] != "tt-loadtest" {
		t.Fatalf("envFrom: %s", b)
	}
	if n := JobName(123456789, strings.Repeat("very-long-region-", 6)); len(n) > 63 || strings.HasSuffix(n, "-") {
		t.Fatalf("job name %q", n)
	}
}

func TestK8sExecutorAPI(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	status := `{"status":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		st := status
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tkn" {
			w.WriteHeader(401)
			return
		}
		switch r.Method {
		case http.MethodPost:
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			if m["kind"] != "Job" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(201)
		case http.MethodGet:
			_, _ = io.WriteString(w, st)
		case http.MethodDelete:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	k := &K8sExecutor{Cfg: K8sConfig{APIServer: srv.URL, Namespace: "tt", Image: "img"}, HTTP: srv.Client(), Token: func() (string, error) { return "tkn", nil }}
	ctx := context.Background()
	h, err := k.Start(ctx, Job{RunID: 5, Region: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	if exited, _ := h.Status(ctx); exited {
		t.Fatal("running job reported exited")
	}
	mu.Lock()
	status = `{"status":{"failed":1,"conditions":[{"type":"Failed","status":"True","reason":"DeadlineExceeded","message":"too long"}]}}`
	mu.Unlock()
	if exited, err := h.Status(ctx); !exited || err == nil || !strings.Contains(err.Error(), "DeadlineExceeded") {
		t.Fatalf("failed job: %v %v", exited, err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"POST /apis/batch/v1/namespaces/tt/jobs", "GET /apis/batch/v1/namespaces/tt/jobs/tt-loadtest-5-eu",
		"GET /apis/batch/v1/namespaces/tt/jobs/tt-loadtest-5-eu", "DELETE /apis/batch/v1/namespaces/tt/jobs/tt-loadtest-5-eu?propagationPolicy=Background"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
}
