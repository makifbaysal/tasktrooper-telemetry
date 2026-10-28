package store

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

func TestInfraStatusRules(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	fresh, old := now.Add(-time.Minute), now.Add(-6*time.Minute)
	hosts := []struct {
		cpu, mem, disk float64
		notReady       bool
		last           time.Time
		want           model.InfraStatus
	}{
		{0.5, 0.5, 0.5, false, fresh, model.InfraOK},
		{0.9, 0.5, 0.5, false, fresh, model.InfraWarning},
		{0.5, 0.96, 0.5, false, fresh, model.InfraCritical},
		{0.5, 0.5, 0.99, false, fresh, model.InfraCritical},
		{0.1, 0.1, 0.1, true, fresh, model.InfraCritical},
		{0.99, 0.1, 0.1, false, old, model.InfraStale},
	}
	for i, c := range hosts {
		if got := HostStatus(c.cpu, c.mem, c.disk, c.notReady, c.last, now); got != c.want {
			t.Errorf("host %d: %s want %s", i, got, c.want)
		}
	}
	pods := []struct {
		phase, reason string
		ready         bool
		inRange       float64
		want          model.InfraStatus
	}{
		{"Running", "", true, 0, model.InfraOK},
		{"Running", "", false, 0, model.InfraCritical},
		{"Failed", "", false, 0, model.InfraCritical},
		{"Running", "CrashLoopBackOff", true, 0, model.InfraCritical},
		{"Pending", "ImagePullBackOff", false, 0, model.InfraCritical},
		{"Running", "", true, 2, model.InfraWarning},
		{"Pending", "", false, 0, model.InfraWarning},
		{"Succeeded", "", false, 0, model.InfraOK},
	}
	for i, c := range pods {
		if got := PodStatus(c.phase, c.reason, c.ready, c.inRange, fresh, now); got != c.want {
			t.Errorf("pod %d: %s want %s", i, got, c.want)
		}
	}
	if PodStatus("Running", "", true, 0, old, now) != model.InfraStale {
		t.Error("pod stale")
	}
	for i, c := range []struct {
		desired, avail float64
		want           model.InfraStatus
	}{{3, 3, model.InfraOK}, {3, 2, model.InfraWarning}, {3, 0, model.InfraCritical}, {0, 0, model.InfraOK}} {
		if got := WorkloadStatus(c.desired, c.avail, fresh, now); got != c.want {
			t.Errorf("workload %d: %s want %s", i, got, c.want)
		}
	}
	c := model.Container{MemoryBytes: 96, MemoryLimit: 100}
	if ContainerStatus(c, 0, fresh, now) != model.InfraCritical {
		t.Error("container oom risk")
	}
	if ContainerStatus(model.Container{CPU: 0.1}, 1, fresh, now) != model.InfraWarning {
		t.Error("container restarted")
	}
	if ContainerStatus(model.Container{CPU: 0.1}, 0, fresh, now) != model.InfraOK {
		t.Error("container ok (no limits)")
	}
}

func TestRestartIncrease(t *testing.T) {
	got := RestartIncrease([]float64{0, 2, 2, 5, 0, 7}, []float64{0, 1, 2, 3, 0, 6}, nil)
	want := []float64{0, 1, 0, 3, 0, 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestFunctionStatusAndCost(t *testing.T) {
	to := int64(10)
	for i, c := range []struct {
		missing           bool
		inv, er, thr, p95 float64
		want              string
	}{
		{true, 100, 0, 0, 0, "missing"},
		{false, 0, 0, 0, 0, "idle"},
		{false, 100, 0.06, 0, 0, "critical"},
		{false, 100, 0, 1, 0, "critical"},
		{false, 100, 0.02, 0, 0, "warning"},
		{false, 100, 0, 0, 8500, "warning"},
		{false, 100, 0.001, 0, 100, "ok"},
	} {
		if got := FunctionStatus(c.missing, c.inv, c.er, c.thr, c.p95, &to); got != c.want {
			t.Errorf("status %d: %s want %s", i, got, c.want)
		}
	}
	mem := int64(1024)
	p := DefaultFaasPrices
	// 1M invocations × 100 ms at 1 GB = 100 000 GB-s
	got := FaasCost(p, "aws", "x86_64", &mem, faasTotals{inv: 1e6, invMs: 1e6 * 100})
	if want := 0.20 + 100000*0.0000166667; math.Abs(got-want) > 1e-6 {
		t.Errorf("lambda x86: %v want %v", got, want)
	}
	if got := FaasCost(p, "aws", "arm64", &mem, faasTotals{inv: 1e6, invMs: 1e6 * 100}); math.Abs(got-(0.20+100000*0.0000133334)) > 1e-6 {
		t.Errorf("lambda arm: %v", got)
	}
	// Azure: 1024 MB·s × 1000 = 1 GB-s per 1 024 000 units
	if got := FaasCost(p, "azure", "", nil, faasTotals{inv: 1e6, units: 1024 * 1000 * 1000}); math.Abs(got-(1000*0.000016+0.20)) > 1e-9 {
		t.Errorf("azure: %v", got)
	}
	t.Setenv("TT_FAAS_LAMBDA_REQUEST_PRICE", "0.4")
	if FaasPricesFromEnv().LambdaRequest != 0.4 || FaasPricesFromEnv().AzureGBs != p.AzureGBs {
		t.Error("env override")
	}
}

func TestMonitorQueryValidation(t *testing.T) {
	ok := func(kind, q string) MonitorKindCase { return MonitorKindCase{kind, q, true} }
	bad := func(kind, q string) MonitorKindCase { return MonitorKindCase{kind, q, false} }
	for _, c := range []MonitorKindCase{
		ok("infra", `{"entity":"host","metric":"cpu","agg":"avg","filter":{"cluster":"prod"}}`),
		ok("infra", `{"entity":"pod","metric":"not_ready"}`),
		ok("infra", `{"entity":"workload","metric":"unavailable","filter":{"namespace":"shop","workload":"api"}}`),
		bad("infra", `{"entity":"host","metric":"restarts"}`),
		bad("infra", `{"entity":"vm","metric":"cpu"}`),
		bad("infra", `{"entity":"host","metric":"cpu","agg":"p95"}`),
		bad("infra", `{"entity":"host","metric":"cpu","filter":{"pod":"x"}}`),
		bad("infra", `{"entity":"host","metric":"cpu","window":"5m"}`),
		ok("faas", `{"functionId":"aws-eu-west-1-orders","metric":"error_rate"}`),
		ok("faas", `{"filter":{"provider":"azure","service":"billing"},"metric":"cold_starts"}`),
		ok("faas", `{"metric":"duration_p95","relativeToTimeout":true}`),
		bad("faas", `{"metric":"error_rate","relativeToTimeout":true}`),
		bad("faas", `{"metric":"memory"}`),
		bad("faas", `{"functionId":"x","filter":{"provider":"aws"},"metric":"errors"}`),
		bad("faas", `{"filter":{"provider":"gcp"},"metric":"errors"}`),
		ok("llm", `{"metric":"cost_usd_per_hour","filter":{}}`),
		ok("llm", `{"metric":"error_rate","filter":{"service":"bff","provider":"openai","model":"gpt-5","route":"/chat","method":"POST","operation":"chat"}}`),
		ok("llm", `{"metric":"ttft_p95_ms"}`),
		bad("llm", `{"metric":"gb_seconds"}`),
		bad("llm", `{}`),
		bad("llm", `{"metric":"tokens","filter":{"region":"eu"}}`),
		bad("llm", `{"metric":"tokens","filter":{"model":5}}`),
		bad("llm", `{"metric":"tokens","service":"bff"}`),
	} {
		in := model.MonitorInput{Name: "m", Kind: c.kind, Severity: "warning", Query: json.RawMessage(c.q)}
		_, _, _, _, err := normalizeMonitor(&in)
		if (err == nil) != c.ok {
			t.Errorf("%s %s: err=%v", c.kind, c.q, err)
		}
	}
	for _, tp := range MonitorTemplates {
		q, _ := json.Marshal(tp.Query)
		in := model.MonitorInput{Name: tp.Name, Kind: tp.Kind, Severity: tp.Severity, Query: q, Source: "template"}
		if _, _, _, _, err := normalizeMonitor(&in); err != nil {
			t.Errorf("template %s invalid: %v", tp.ID, err)
		}
	}
}

type MonitorKindCase struct {
	kind, q string
	ok      bool
}

// TestMonitorTemplatesInSync compares the ids with alerting's eval.Templates (source file, no module dependency).
func TestMonitorTemplatesInSync(t *testing.T) {
	src, err := os.ReadFile("../../../alerting/internal/eval/templates.go")
	if err != nil {
		t.Skipf("alerting source not available: %v", err)
	}
	var want []string
	for _, m := range regexp.MustCompile(`\{ID: "([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		want = append(want, m[1])
	}
	var got []string
	for _, tp := range MonitorTemplates {
		got = append(got, tp.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("api templates %v, alerting %v", got, want)
	}
}

func TestRestartIncreaseZeroBuckets(t *testing.T) {
	// counter at 0 for two buckets, then 2: the jump shows up in the bucket where it happened
	got := RestartIncrease([]float64{0, 0, 0, 2, 2}, []float64{0, 0, 0, 2, 2}, []float64{0, 1, 1, 1, 1})
	if want := []float64{0, 0, 0, 2, 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestPodTraceAndLogFilters(t *testing.T) {
	w, args := TraceFilter{Pod: "api-7f", Host: "h1"}.resourceConds()
	if w != " AND resource_attrs[?] = ? AND resource_attrs[?] = ?" || len(args) != 4 || args[0] != "k8s.pod.name" || args[1] != "api-7f" ||
		args[2] != "host.name" || args[3] != "h1" {
		t.Errorf("trace conds: %q %v", w, args)
	}
	if w, _ := (TraceFilter{}).resourceConds(); w != "" {
		t.Errorf("empty trace conds: %q", w)
	}
	lw, la, err := logWhere(LogFilter{Pod: "api-7f", Namespace: "shop"}, true)
	if err != nil || lw != " AND attrs['k8s.pod.name'] = ? AND attrs['k8s.namespace.name'] = ?" || len(la) != 2 {
		t.Errorf("log where: %q %v %v", lw, la, err)
	}
}

func TestServerlessSummaryRows(t *testing.T) {
	fns := []model.ServerlessFunction{
		{ID: "1", Provider: "aws"},
		{ID: "2", Provider: "azure"},                        // app with children
		{ID: "3", Provider: "azure", ParentID: "2"},         // child
		{ID: "4", Provider: "azure", ParentID: "2"},         // child
		{ID: "5", Provider: "azure"},                        // app without per-function rows
		{ID: "6", Provider: "azure", ParentID: "99"},        // parent filtered out: a function, metrics excluded
		{ID: "7", Provider: "azure", ParentSlug: "azure-x"}, // parent row unresolved: still excluded from metrics
	}
	countFn, add := SummaryRows(fns)
	wantCount := []bool{true, false, true, true, true, true, true}
	wantAdd := []bool{true, true, false, false, true, false, false}
	for i := range fns {
		if countFn[i] != wantCount[i] || add[i] != wantAdd[i] {
			t.Errorf("row %s: count %v add %v", fns[i].ID, countFn[i], add[i])
		}
	}
}

func TestHostGPUs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	hosts := []model.Host{{ID: "gpu-1", LastSeen: now.UnixMilli()}, {ID: "cpu-only", LastSeen: now.UnixMilli()}}
	AttachGPUs(hosts, map[string][]gpuRow{"gpu-1": {
		{g: model.HostGPU{Index: 1, Model: "A100", Utilization: 0.5}, last: now},
		{g: model.HostGPU{Index: 0, Model: "A100", Utilization: 0.9}, last: now.Add(-time.Minute)},
		{g: model.HostGPU{Index: 2, Model: "T4"}, last: now.Add(-10 * time.Minute)}, // gone
	}})
	if len(hosts[0].GPUs) != 2 || hosts[0].GPUs[0].Index != 0 || hosts[0].GPUs[1].Index != 1 {
		t.Errorf("gpus: %+v", hosts[0].GPUs)
	}
	if hosts[1].GPUs == nil || len(hosts[1].GPUs) != 0 {
		t.Errorf("no gpus must be [], got %#v", hosts[1].GPUs)
	}
	w := timerange.Custom(now, time.Minute, 3)
	s := GPUSet(w, hosts[0].GPUs, map[string][][]float64{"1": {{0.1, 0.2, 0.3}}})
	if len(s.Series) != 2 || s.Series[0].ID != "gpu0" || s.Series[1].ID != "gpu1" || s.Series[1].Label != "GPU 1 · A100" ||
		s.Series[1].Data[2] != 0.3 || s.Series[0].Data[0] != 0 || len(s.Timestamps) != 3 {
		t.Errorf("gpu set: %+v", s)
	}
}

// Regression: renotifyMin and stream used to be dropped on create because the condition struct lacked them.
func TestMonitorConditionKeepsReminderAndStream(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{"reminder", `{"op":">","threshold":0.1,"forMin":5,"renotifyMin":30}`, map[string]any{"renotifyMin": 30.0}},
		{"stream", `{"op":">","threshold":0,"forMin":0,"stream":true}`, map[string]any{"stream": true}},
		{"neither", `{"op":">","threshold":1,"forMin":0}`, map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := model.MonitorInput{Name: "m", Kind: "log_match", Severity: "warning", Source: "manual", Query: []byte(`{}`)}
			if err := json.Unmarshal([]byte(tc.in), &in.Condition); err != nil {
				t.Fatal(err)
			}
			_, _, cond, _, err := normalizeMonitor(&in)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			_ = json.Unmarshal(cond, &got)
			for _, k := range []string{"renotifyMin", "stream"} {
				w, ok := tc.want[k]
				if g, has := got[k]; has != ok || (ok && g != w) {
					t.Fatalf("%s: got %v (present %v), want %v (present %v); cond=%s", k, g, has, w, ok, cond)
				}
			}
		})
	}
}
