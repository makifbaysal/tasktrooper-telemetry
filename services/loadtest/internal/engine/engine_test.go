package engine

import (
	"context"
	"encoding/json"
	"io"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/spec"
)

func mustPlan(t *testing.T, js string) *spec.Plan {
	t.Helper()
	p, err := spec.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStageShaping(t *testing.T) {
	var inflight atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	// ramp 0→20 over 1s, hold 20 for 1s, ramp down to 0 over 1s
	p := mustPlan(t, `{"target":"`+srv.URL+`","mode":"vus","thinkTime":0.02,
	  "levels":[{"ramp":1,"hold":1,"target":20},{"ramp":1,"hold":0,"target":0}],"steps":["GET /x"]}`)
	var mu sync.Mutex
	var samples []Live
	e := New(Options{Plan: p, RunID: "1", Tick: 100 * time.Millisecond, OnTick: func(tk Tick) {
		if !tk.Final {
			mu.Lock()
			samples = append(samples, tk.Live)
			mu.Unlock()
		}
	}})
	res := e.Run(context.Background())
	if res.Aborted || res.Stats.PeakVUs != 20 {
		t.Fatalf("aborted=%v peak=%v", res.Aborted, res.Stats.PeakVUs)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range samples {
		el := time.Duration(s.Progress * float64(3*time.Second))
		want := spec.TargetAt(p.Stages, el)
		if math.Abs(s.Vus-want) > 3 {
			t.Errorf("t=%v vus=%v want≈%v", el, s.Vus, want)
		}
	}
	// VU-seconds ≈ area under the curve: 10 + 20 + 10 = 40
	if v := res.Stats.VUSeconds; v < 34 || v > 46 {
		t.Errorf("vu seconds = %v", v)
	}
}

func TestArrivalRateAccuracy(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(5 * time.Millisecond)
	}))
	defer srv.Close()
	// 200 req/s for 2 s (ramp 0 = jump) → 400 requests
	p := mustPlan(t, `{"target":"`+srv.URL+`","mode":"rps","levels":[{"ramp":0,"hold":2,"target":200}],
	  "steps":["GET /a","GET /b"],"weights":{"GET /a":0.5,"GET /b":0.5}}`)
	res := New(Options{Plan: p, RunID: "1"}).Run(context.Background())
	got := res.Stats.Requests
	if math.Abs(float64(got)-400) > 400*0.05 {
		t.Fatalf("requests = %d, want 400±5%%", got)
	}
	if res.Stats.Dropped != 0 {
		t.Fatalf("dropped = %d", res.Stats.Dropped)
	}
	a, b := res.Stats.Steps["GET /a"].Requests, res.Stats.Steps["GET /b"].Requests
	if math.Abs(float64(a-b)) > 0.25*float64(got) {
		t.Fatalf("weights not honored: a=%d b=%d", a, b)
	}
}

func TestArrivalRateRamp(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()
	// ramp 0 → 300 req/s over 2 s → 300 requests
	p := mustPlan(t, `{"target":"`+srv.URL+`","mode":"rps","levels":[{"ramp":2,"hold":0,"target":300}],"steps":["GET /a"]}`)
	res := New(Options{Plan: p, RunID: "1"}).Run(context.Background())
	if math.Abs(float64(res.Stats.Requests)-300) > 15 {
		t.Fatalf("requests = %d, want 300±5%%", res.Stats.Requests)
	}
}

func TestDroppedWhenPoolExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer srv.Close()
	p := mustPlan(t, `{"target":"`+srv.URL+`","mode":"rps","maxVus":2,"levels":[{"ramp":0,"hold":1,"target":50}],"steps":["GET /a"]}`)
	res := New(Options{Plan: p, RunID: "1"}).Run(context.Background())
	if res.Stats.Dropped < 30 {
		t.Fatalf("dropped = %d, want most of 50", res.Stats.Dropped)
	}
}

func TestHeadersTemplatesAndPrev(t *testing.T) {
	var mu sync.Mutex
	var errs []string
	fail := func(f string) { mu.Lock(); errs = append(errs, f); mu.Unlock() }
	tp := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	seenTP := map[string]bool{}
	mux := http.NewServeMux()
	check := func(r *http.Request) {
		if !tp.MatchString(r.Header.Get("traceparent")) {
			fail("traceparent " + r.Header.Get("traceparent"))
		}
		mu.Lock()
		if seenTP[r.Header.Get("traceparent")] {
			errs = append(errs, "traceparent reused")
		}
		seenTP[r.Header.Get("traceparent")] = true
		mu.Unlock()
		if r.Header.Get("baggage") != "tt.svc=loadtest:77" || r.Header.Get("x-trooper-loadtest-run") != "77" || r.Header.Get("User-Agent") != UserAgent {
			fail("correlation headers " + r.Header.Get("baggage"))
		}
	}
	var created atomic.Int64
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if q, ok := body["qty"].(float64); !ok || q < 1 || q > 3 {
			fail("qty not a number in 1..3")
		}
		if _, ok := body["sku"].(string); !ok || !strings.HasPrefix(body["sku"].(string), "SKU-") {
			fail("sku from pool")
		}
		if r.Header.Get("Authorization") != "Bearer tok-"+r.Header.Get("X-User") {
			fail("pool row not shared within a request: " + r.Header.Get("Authorization") + " / " + r.Header.Get("X-User"))
		}
		if _, has := body["coupon"]; has {
			fail("optional 0% must be omitted")
		}
		id := 1000 + created.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "total": 9.5, "nested": map[string]any{"ref": "r-" + itoa(id)}})
	})
	mux.HandleFunc("GET /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		if !strings.HasPrefix(r.PathValue("id"), "10") || r.URL.Query().Get("expand") != "items" {
			fail("path param / query: " + r.URL.String())
		}
	})
	mux.HandleFunc("POST /v1/charges", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if id, ok := body["order_id"].(float64); !ok || id < 1001 {
			fail("prev id not numeric")
		}
		if body["amount"] != 9.5 || body["ref"] != body["ref_check"] || !strings.HasPrefix(body["ref"].(string), "r-") {
			fail("prev fields")
		}
		if u, _ := body["idem"].(string); len(u) != 36 {
			fail("uuid")
		}
		w.WriteHeader(201)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := mustPlan(t, `{"target":"`+srv.URL+`","mode":"vus","flow":"sequential","levels":[{"ramp":0,"hold":1,"target":3}],"thinkTime":0.01,
	  "steps":["POST /v1/orders","GET /v1/orders/:id","POST /v1/charges"],
	  "pools":{"skus":["SKU-1","SKU-2"],"users":[{"id":"u1","token":"tok-u1"},{"id":"u2","token":"tok-u2"}]},
	  "requests":{
	    "POST /v1/orders":{"headers":"Authorization: Bearer {{pool.users.token}}\nX-User: {{pool.users.id}}","body":"{\"qty\":\"{{int 1-3}}\",\"sku\":\"{{pool.skus}}\",\"coupon\":\"{{optional 0% pool.skus}}\"}"},
	    "GET /v1/orders/:id":{"params":"id: {{prev \"POST /v1/orders\" .id}}\nexpand: items"},
	    "POST /v1/charges":{"body":"{\"order_id\":\"{{prev \\\"POST /v1/orders\\\" .id}}\",\"amount\":\"{{prev \\\"POST /v1/orders\\\" .total}}\",\"ref\":\"{{prev \\\"POST /v1/orders\\\" .nested.ref}}\",\"ref_check\":\"{{prev \\\"POST /v1/orders\\\" .nested.ref}}\",\"idem\":\"{{uuid}}\"}",
	      "checks":["status == 201"]}}}`)
	res := New(Options{Plan: p, RunID: "77"}).Run(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(errs) > 0 {
		t.Fatalf("%d problems, first: %v", len(errs), errs[0])
	}
	if res.Stats.Errors != 0 || res.Stats.Steps["POST /v1/charges"].Requests == 0 {
		t.Fatalf("errors=%d steps=%v warnings=%v", res.Stats.Errors, res.Stats.Steps, res.Warnings)
	}
	if res.Stats.Checks["status == 201"][0] == 0 {
		t.Fatalf("checks not recorded: %v", res.Stats.Checks)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestTemplateFunctions(t *testing.T) {
	vu := &vuState{rng: mrand.New(mrand.NewPCG(1, 2)), session: map[string]any{"cart": "c-1"}, prev: map[string]any{
		"POST /x": map[string]any{"items": []any{map[string]any{"id": "i-9"}}},
	}}
	var warns []string
	r := &renderer{vu: vu, pools: map[string][]any{"p": {"a"}}, rows: map[string]any{}, warn: func(s string) { warns = append(warns, s) }}
	tests := []struct {
		in   string
		want any
	}{
		{"{{session.cart}}", "c-1"},
		{"{{pool.p}}", "a"},
		{"{{random.p}}", "a"},
		{"x-{{pool.p}}-y", "x-a-y"},
		{`{{prev "POST /x" .items.0.id}}`, "i-9"},
		{"{{weighted 7:100}}", 7.0},
		{"{{weighted TRY:100 EUR:0}}", "TRY"},
		{"{{int 5-5}}", 5.0},
		{"{{optional 100% pool.p}}", "a"},
		{`{{optional 100% prev "POST /x" .items.0.id}}`, "i-9"},
	}
	for _, tc := range tests {
		if got := r.Value(tc.in); got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	if _, ok := r.Value("{{optional 0% pool.p}}").(absent); !ok {
		t.Error("optional 0% should be absent")
	}
	if s := r.String("{{faker.words 3}}"); len(strings.Fields(s)) != 3 {
		t.Errorf("faker.words = %q", s)
	}
	if v := r.Value("{{pool.missing}}"); !strings.HasPrefix(v.(string), "missing-") || len(warns) == 0 {
		t.Errorf("missing pool = %v warns=%v", v, warns)
	}
	// weighted distribution ≈ 70/30
	cnt := map[string]int{}
	for range 10000 {
		cnt[weighted(vu.rng, []string{"a:70", "b:30"})]++
	}
	if math.Abs(float64(cnt["a"])/10000-0.7) > 0.03 {
		t.Errorf("weighted: %v", cnt)
	}
}

func TestThresholdEvaluation(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1)%10 == 0 {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	p := mustPlan(t, `{"target":"`+srv.URL+`","mode":"rps","levels":[{"ramp":0,"hold":1,"target":100}],"steps":["GET /a"],
	  "thresholds":["http_req_failed rate < 1%","http_req_duration p(95) < 500 ms"]}`)
	res := New(Options{Plan: p, RunID: "1"}).Run(context.Background())
	sum, ok := Summarize(res.Stats, p.Thresholds)
	if ok || len(sum.Thresholds) != 2 || sum.Thresholds[0].Passed || !sum.Thresholds[1].Passed {
		t.Fatalf("thresholds: %+v", sum.Thresholds)
	}
	if math.Abs(sum.Thresholds[0].Value-0.1) > 0.02 {
		t.Fatalf("error rate value = %v", sum.Thresholds[0].Value)
	}
	if sum.StatusCodes["500"] == 0 || sum.StatusCodes["200"] == 0 || len(sum.Steps) != 1 || sum.Steps[0].P95 <= 0 {
		t.Fatalf("summary: %+v", sum)
	}
}

func TestGateAutoAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	p := mustPlan(t, `{"target":"`+srv.URL+`","mode":"rps","levels":[{"ramp":0,"hold":30,"target":100}],"steps":["GET /a"],
	  "gate":{"abortErrorRate":0.2}}`)
	start := time.Now()
	res := New(Options{Plan: p, RunID: "1", Tick: 200 * time.Millisecond}).Run(context.Background())
	if !res.Aborted || !strings.HasPrefix(res.AbortReason, "gate: error rate") {
		t.Fatalf("aborted=%v reason=%q", res.Aborted, res.AbortReason)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("abort took %v", time.Since(start))
	}
}

func TestStopViaContextAndAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	p := mustPlan(t, `{"target":"`+srv.URL+`","levels":[{"ramp":0,"hold":60,"target":5}],"steps":["GET /a"],"thinkTime":0.05}`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := New(Options{Plan: p, RunID: "1"}).Run(ctx)
	if !res.Aborted || res.AbortReason != "stopped" || res.Stats.Requests == 0 {
		t.Fatalf("ctx stop: %+v", res)
	}
	e := New(Options{Plan: p, RunID: "1"})
	go func() { time.Sleep(200 * time.Millisecond); e.Abort("user") }()
	if r := e.Run(context.Background()); r.AbortReason != "user" {
		t.Fatalf("abort reason %q", r.AbortReason)
	}
}

func TestHistMerge(t *testing.T) {
	var a, b, all Hist
	for i := 1; i <= 1000; i++ {
		v := float64(i)
		all.Add(v)
		if i%2 == 0 {
			a.Add(v)
		} else {
			b.Add(v)
		}
	}
	a.Merge(b)
	for _, q := range []float64{0.5, 0.95, 0.99} {
		if a.Quantile(q) != all.Quantile(q) || math.Abs(a.Quantile(q)-q*1000)/(q*1000) > 0.02 {
			t.Errorf("q%v: merged %v all %v", q, a.Quantile(q), all.Quantile(q))
		}
	}
}
