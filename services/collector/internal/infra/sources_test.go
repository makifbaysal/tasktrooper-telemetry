package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// find returns the value of metric whose attributes include match.
func find(t *testing.T, r emit.Result, metric string, match map[string]string) (float64, bool) {
	t.Helper()
	for _, s := range r.Gauges {
		if s.Name != metric {
			continue
		}
		ok := true
		for k, v := range match {
			if got, _ := s.Attrs.Value(attrKey(k)); got.Emit() != v {
				ok = false
				break
			}
		}
		if ok {
			return s.Value, true
		}
	}
	return 0, false
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6*math.Max(1, math.Abs(b)) }

// ---------------------------------------------------------------- kubelet

const kubeletPods = `{"items":[
 {"metadata":{"name":"orders-api-7d9f8b6c5-x2k9p","namespace":"shop","uid":"u-orders",
   "labels":{"app.kubernetes.io/name":"orders-api","pod-template-hash":"7d9f8b6c5"},
   "ownerReferences":[{"kind":"ReplicaSet","name":"orders-api-7d9f8b6c5","controller":true}]},
  "spec":{"nodeName":"node-a","containers":[
    {"name":"app","image":"ghcr.io/shop/orders-api:2.41.0","resources":{"limits":{"cpu":"500m","memory":"256Mi"}}},
    {"name":"sidecar","image":"envoy:1.30","resources":{}}]},
  "status":{"phase":"Running","containerStatuses":[
    {"name":"app","containerID":"containerd://aaa111","restartCount":3,"ready":true,"state":{"running":{}}},
    {"name":"sidecar","containerID":"containerd://bbb222","restartCount":0,"ready":true,"state":{"running":{}}}]}}
]}`

func kubeletSummary(cpuNs, rx, tx float64) string {
	return fmt.Sprintf(`{"node":{"nodeName":"node-a"},"pods":[
 {"podRef":{"name":"orders-api-7d9f8b6c5-x2k9p","namespace":"shop","uid":"u-orders"},
  "containers":[
   {"name":"app","cpu":{"usageNanoCores":250000000,"usageCoreNanoSeconds":%f},"memory":{"workingSetBytes":104857600}},
   {"name":"sidecar","cpu":{"usageCoreNanoSeconds":%f},"memory":{"workingSetBytes":2097152}}],
  "network":{"rxBytes":%f,"txBytes":%f}},
 {"podRef":{"name":"gone","namespace":"shop","uid":"u-gone"},"containers":[{"name":"x"}]}
]}`, cpuNs, cpuNs, rx, tx)
}

func TestKubeletScraper(t *testing.T) {
	var call atomic.Int32
	var gotAuth atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		n := float64(call.Load())
		switch r.URL.Path {
		case "/pods":
			fmt.Fprint(w, kubeletPods)
		case "/stats/summary":
			call.Add(1)
			fmt.Fprint(w, kubeletSummary(1e9+n*1.5e9, 1000+n*15000, 500+n*30000))
		case "/metrics/cadvisor":
			fmt.Fprintf(w, "# HELP x\ncontainer_cpu_cfs_periods_total{container=\"app\",namespace=\"shop\",pod=\"orders-api-7d9f8b6c5-x2k9p\"} %f\n", 100+n*100)
			fmt.Fprintf(w, "container_cpu_cfs_throttled_periods_total{container=\"app\",namespace=\"shop\",pod=\"orders-api-7d9f8b6c5-x2k9p\"} %f\n", 10+n*25)
			fmt.Fprint(w, "container_memory_rss{container=\"app\"} 5\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	env := KubeEnv{KubeletURL: srv.URL, KubeletInsecure: true, Token: "sa-token"}
	kc, err := env.Kubelet()
	if err != nil {
		t.Fatal(err)
	}
	k := NewKubeletScraper(kc)
	t0 := time.Unix(1_800_000_000, 0)
	k.now = func() time.Time { return t0 }
	if _, err := k.Scrape(context.Background()); err != nil {
		t.Fatal(err)
	}
	k.now = func() time.Time { return t0.Add(15 * time.Second) }
	r, err := k.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth.Load() != "Bearer sa-token" {
		t.Fatalf("auth header = %v", gotAuth.Load())
	}
	app := map[string]string{
		"container.id": "aaa111", "container.name": "app", "container.image.name": "ghcr.io/shop/orders-api:2.41.0",
		"k8s.namespace.name": "shop", "k8s.pod.name": "orders-api-7d9f8b6c5-x2k9p", "k8s.pod.uid": "u-orders",
		"trooper.k8s.workload.kind": "Deployment", "trooper.k8s.workload.name": "orders-api", "trooper.service": "orders-api",
	}
	tests := []struct {
		metric string
		match  map[string]string
		want   float64
		absent bool
	}{
		{emit.ContainerCPU, app, 0.25, false}, // usageNanoCores
		{emit.ContainerMemoryBytes, app, 104857600, false},
		{emit.ContainerCPULimit, app, 0.5, false},
		{emit.ContainerMemoryLimitBytes, app, 256 << 20, false},
		{emit.ContainerRestarts, app, 3, false},
		{emit.ContainerNetIn, app, 1000, false},  // 15000 B / 15 s
		{emit.ContainerNetOut, app, 2000, false}, // 30000 B / 15 s
		{emit.ContainerThrottled, app, 0.25, false},
		{emit.ContainerCPU, map[string]string{"container.name": "sidecar"}, 0.1, false}, // Δ1.5e9 ns / 15 s
		{emit.ContainerCPULimit, map[string]string{"container.name": "sidecar"}, 0, true},
		{emit.ContainerNetIn, map[string]string{"container.name": "sidecar"}, 0, true}, // pod network only on the first container
		{emit.ContainerCPU, map[string]string{"k8s.pod.name": "gone"}, 0, true},
	}
	for _, tt := range tests {
		v, ok := find(t, r, tt.metric, tt.match)
		if tt.absent {
			if ok {
				t.Errorf("%s %v: unexpected sample %v", tt.metric, tt.match, v)
			}
			continue
		}
		if !ok || !near(v, tt.want) {
			t.Errorf("%s %v = %v (found %v), want %v", tt.metric, tt.match, v, ok, tt.want)
		}
	}
}

func TestKubeletScraperErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden (user=system:serviceaccount:tt:agent, verb=get, resource=nodes, subresource=proxy)", http.StatusForbidden)
	}))
	defer srv.Close()
	tests := []struct {
		name string
		env  KubeEnv
		want string
	}{
		{"forbidden", KubeEnv{KubeletURL: srv.URL, KubeletInsecure: true}, "403"},
		{"tls verify fails without insecure", KubeEnv{KubeletURL: srv.URL}, "certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc, _ := tt.env.Kubelet()
			_, err := NewKubeletScraper(kc).Scrape(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := (KubeEnv{}).Kubelet(); err == nil {
		t.Fatal("no NODE_IP: want error")
	}
}

// ---------------------------------------------------------------- docker

// fakeDocker serves the Docker Engine API on a unix socket.
func fakeDocker(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttdk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	var mu sync.Mutex
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("docker API must be read-only, got %s %s", r.Method, r.URL.Path)
		}
		k := float64(n.Load())
		switch r.URL.Path {
		case "/containers/json":
			n.Add(1)
			fmt.Fprint(w, `[{"Id":"c1","Names":["/shop-orders-1"],"Image":"orders:2.41","Labels":{"tasktrooper.io/service":"orders-api"}},
			                {"Id":"c2","Names":["/redis"],"Image":"redis:7","Labels":{}},
			                {"Id":"c3","Names":["/stopped"],"Image":"x","Labels":{}}]`)
		case "/containers/c1/stats", "/containers/c2/stats":
			if r.URL.Query().Get("stream") != "false" {
				t.Errorf("stats must not stream")
			}
			fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%f},"throttling_data":{"periods":%f,"throttled_periods":%f}},
			  "memory_stats":{"usage":%f,"stats":{"inactive_file":1048576}},
			  "networks":{"eth0":{"rx_bytes":%f,"tx_bytes":%f},"eth1":{"rx_bytes":%f,"tx_bytes":0}}}`,
				1e9+k*7.5e9, 50+k*100, 5+k*10, 11534336.0, 100+k*30000, 100+k*15000, k*15000)
		case "/containers/c3/stats":
			http.Error(w, "No such container", http.StatusNotFound)
		case "/containers/c1/json":
			fmt.Fprint(w, `{"RestartCount":2,"HostConfig":{"Memory":536870912,"NanoCpus":1500000000}}`)
		case "/containers/c2/json":
			fmt.Fprint(w, `{"RestartCount":0,"HostConfig":{"Memory":0,"CpuQuota":50000,"CpuPeriod":100000}}`)
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, &n
}

func TestDockerScraper(t *testing.T) {
	sock, _ := fakeDocker(t)
	if !socketExists(sock) {
		t.Fatal("socketExists = false")
	}
	d := NewDockerScraper(sock)
	defer d.Close()
	t0 := time.Unix(1_800_000_000, 0)
	d.now = func() time.Time { return t0 }
	if _, err := d.Scrape(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return t0.Add(15 * time.Second) }
	r, err := d.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c1 := map[string]string{
		"container.id": "c1", "container.name": "shop-orders-1", "container.image.name": "orders:2.41",
		"k8s.namespace.name": "", "k8s.pod.name": "", "k8s.pod.uid": "", "trooper.k8s.workload.kind": "", "trooper.k8s.workload.name": "",
		"trooper.service": "orders-api",
	}
	tests := []struct {
		metric string
		match  map[string]string
		want   float64
		absent bool
	}{
		{emit.ContainerCPU, c1, 0.5, false}, // 7.5e9 ns / 15 s
		{emit.ContainerMemoryBytes, c1, 10 << 20, false},
		{emit.ContainerCPULimit, c1, 1.5, false},
		{emit.ContainerMemoryLimitBytes, c1, 512 << 20, false},
		{emit.ContainerRestarts, c1, 2, false},
		{emit.ContainerNetIn, c1, 3000, false}, // (30000 + 15000) / 15
		{emit.ContainerNetOut, c1, 1000, false},
		{emit.ContainerThrottled, c1, 0.1, false},
		{emit.ContainerCPULimit, map[string]string{"container.id": "c2"}, 0.5, false}, // quota / period
		{emit.ContainerMemoryLimitBytes, map[string]string{"container.id": "c2"}, 0, true},
		{emit.ContainerCPU, map[string]string{"container.id": "c2", "trooper.service": "redis"}, 0.5, false}, // service = container name
		{emit.ContainerCPU, map[string]string{"container.id": "c3"}, 0, true},
	}
	for _, tt := range tests {
		v, ok := find(t, r, tt.metric, tt.match)
		if tt.absent {
			if ok {
				t.Errorf("%s %v: unexpected sample %v", tt.metric, tt.match, v)
			}
			continue
		}
		if !ok || !near(v, tt.want) {
			t.Errorf("%s %v = %v (found %v), want %v", tt.metric, tt.match, v, ok, tt.want)
		}
	}
}

func TestDockerScraperNoSocket(t *testing.T) {
	d := NewDockerScraper(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := d.Scrape(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if socketExists(filepath.Join(t.TempDir(), "missing.sock")) {
		t.Fatal("socketExists on a missing path")
	}
}

// ---------------------------------------------------------------- cluster (API server)
// fakeAPI serves paged lists (with metadata.resourceVersion) and watch streams: a watch request
// (watch=1) blocks and writes every line pushed with push(path, …); "CLOSE" ends the stream.
type fakeAPI struct {
	mu          sync.Mutex
	objects     map[string]string // path → items JSON array
	paths       []string
	rv          string
	lists       map[string]int
	watchRVs    map[string][]string
	watchStatus map[string]int // next watch request on path answers this status
	streams     map[string]chan string
}

func (f *fakeAPI) set(path, items string) {
	f.mu.Lock()
	f.objects[path] = items
	f.mu.Unlock()
}

func (f *fakeAPI) stream(path string) chan string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.streams == nil {
		f.streams = map[string]chan string{}
	}
	if f.streams[path] == nil {
		f.streams[path] = make(chan string, 64)
	}
	return f.streams[path]
}

// push sends one watch event line on path's stream.
func (f *fakeAPI) push(path, line string) { f.stream(path) <- line }

func (f *fakeAPI) listCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists[path]
}

func (f *fakeAPI) watchRV(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.watchRVs[path]...)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	if f.lists == nil {
		f.lists, f.watchRVs, f.watchStatus = map[string]int{}, map[string][]string{}, map[string]int{}
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		f.mu.Unlock()
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	items, ok := f.objects[r.URL.Path]
	if !ok {
		f.mu.Unlock()
		http.Error(w, `{"kind":"Status","code":403}`, http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api/v1/events" && r.URL.Query().Get("fieldSelector") != "type=Warning" {
		f.mu.Unlock()
		http.Error(w, "want fieldSelector", http.StatusBadRequest)
		return
	}
	rv := f.rv
	if rv == "" {
		rv = "100"
	}
	if r.URL.Query().Get("watch") == "1" {
		if r.URL.Query().Get("allowWatchBookmarks") != "true" {
			f.mu.Unlock()
			http.Error(w, "want bookmarks", http.StatusBadRequest)
			return
		}
		f.watchRVs[r.URL.Path] = append(f.watchRVs[r.URL.Path], r.URL.Query().Get("resourceVersion"))
		if st := f.watchStatus[r.URL.Path]; st != 0 {
			delete(f.watchStatus, r.URL.Path)
			f.mu.Unlock()
			http.Error(w, `{"kind":"Status","code":`+fmt.Sprint(st)+`}`, st)
			return
		}
		f.mu.Unlock()
		ch := f.stream(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-ch:
				if line == "CLOSE" {
					return
				}
				fmt.Fprintln(w, line)
				w.(http.Flusher).Flush()
			}
		}
	}
	if r.URL.Query().Get("continue") == "" {
		f.lists[r.URL.Path]++
	}
	f.mu.Unlock()
	// two pages for pods to exercise continue
	if r.URL.Path == "/api/v1/pods" {
		var all []json.RawMessage
		_ = json.Unmarshal([]byte(items), &all)
		if r.URL.Query().Get("continue") == "" && len(all) > 1 {
			b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"continue": "p2", "resourceVersion": rv}, "items": all[:1]})
			w.Write(b)
			return
		}
		if r.URL.Query().Get("continue") == "p2" {
			all = all[1:]
		}
		b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": rv}, "items": all})
		w.Write(b)
		return
	}
	fmt.Fprintf(w, `{"metadata":{"resourceVersion":%q},"items":%s}`, rv, items)
}

type logRec struct {
	mu   sync.Mutex
	recs []emit.LogRecord
}

func (l *logRec) Warn(_ context.Context, r emit.LogRecord) {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
}

func (l *logRec) reset() {
	l.mu.Lock()
	l.recs = nil
	l.mu.Unlock()
}

func (l *logRec) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recs)
}

func (l *logRec) has(body string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.Body == body {
			return true
		}
	}
	return false
}

func newFakeCluster(now time.Time) *fakeAPI {
	recent := now.Add(-time.Minute).Format(time.RFC3339)
	old := now.Add(-2 * time.Hour).Format(time.RFC3339)
	ancient := now.Add(-48 * time.Hour).Format(time.RFC3339)
	f := &fakeAPI{objects: map[string]string{}}
	f.set("/api/v1/nodes", `[
	 {"metadata":{"name":"node-a","labels":{"topology.kubernetes.io/region":"eu-central-1"}},"spec":{"providerID":"aws:///eu-central-1a/i-1"},
	  "status":{"allocatable":{"cpu":"3920m","memory":"15Gi"},"conditions":[{"type":"Ready","status":"True"}]}},
	 {"metadata":{"name":"node-b"},"status":{"allocatable":{"cpu":"4","memory":"16000000Ki"},"conditions":[{"type":"Ready","status":"False"}]}}]`)
	f.set("/api/v1/pods", `[
	 {"metadata":{"name":"payments-api-6b8d9c7f4-abcde","namespace":"shop","uid":"u-pay","labels":{"tasktrooper.io/service":"payments-api","app":"pay"},
	   "ownerReferences":[{"kind":"ReplicaSet","name":"payments-api-6b8d9c7f4","controller":true}]},
	  "spec":{"nodeName":"node-a","containers":[{"name":"app","resources":{"requests":{"cpu":"250m","memory":"128Mi"},"limits":{"cpu":"1","memory":"512Mi"}}}]},
	  "status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[
	    {"name":"app","restartCount":4,"state":{"waiting":{"reason":"CrashLoopBackOff"}},
	     "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"finishedAt":"`+recent+`"}}}]}},
	 {"metadata":{"name":"invoice-batch-29123456-xyz","namespace":"shop","uid":"u-job","labels":{"app":"invoice"},
	   "ownerReferences":[{"kind":"Job","name":"invoice-batch-29123456","controller":true}]},
	  "spec":{"nodeName":"node-b","containers":[{"name":"job"}]},
	  "status":{"phase":"Succeeded","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"name":"job","restartCount":0}]}}]`)
	f.set("/apis/apps/v1/replicasets", `[{"metadata":{"name":"payments-api-6b8d9c7f4","namespace":"shop","ownerReferences":[{"kind":"Deployment","name":"payments-api","controller":true}]}}]`)
	f.set("/apis/apps/v1/deployments", `[{"metadata":{"name":"payments-api","namespace":"shop"},"spec":{"replicas":3},"status":{"readyReplicas":2,"availableReplicas":2,"updatedReplicas":3}}]`)
	f.set("/apis/apps/v1/statefulsets", `[{"metadata":{"name":"kafka","namespace":"data"},"spec":{"replicas":3},"status":{"readyReplicas":3,"updatedReplicas":3}}]`)
	f.set("/apis/apps/v1/daemonsets", `[{"metadata":{"name":"trooper-agent-host","namespace":"tt"},"status":{"desiredNumberScheduled":2,"numberReady":2,"numberAvailable":2,"updatedNumberScheduled":2}}]`)
	f.set("/apis/batch/v1/jobs", `[
	 {"metadata":{"name":"invoice-batch-29123456","namespace":"shop","ownerReferences":[{"kind":"CronJob","name":"invoice-batch","controller":true}]},
	  "status":{"failed":1,"startTime":"`+recent+`","conditions":[{"type":"Failed","status":"True"}]}},
	 {"metadata":{"name":"migrate","namespace":"shop"},"spec":{"completions":1},"status":{"active":1,"ready":1,"startTime":"`+recent+`"}},
	 {"metadata":{"name":"ancient","namespace":"shop"},"status":{"succeeded":1,"startTime":"`+ancient+`","completionTime":"`+ancient+`","conditions":[{"type":"Complete","status":"True"}]}}]`)
	// no cronjobs entry → 403 → that kind is skipped, the rest still reports
	f.set("/api/v1/events", `[
	 {"metadata":{"uid":"e1","namespace":"shop","name":"payments.1"},"involvedObject":{"kind":"Pod","name":"payments-api-6b8d9c7f4-abcde","namespace":"shop"},
	  "reason":"BackOff","message":"Back-off restarting failed container app","type":"Warning","count":4,"lastTimestamp":"`+recent+`"},
	 {"metadata":{"uid":"e-old","namespace":"shop","name":"old.1"},"involvedObject":{"kind":"Pod","name":"x","namespace":"shop"},
	  "reason":"FailedMount","message":"old","type":"Warning","count":1,"lastTimestamp":"`+old+`"},
	 {"metadata":{"uid":"e3","namespace":"default","name":"node-b.1"},"involvedObject":{"kind":"Node","name":"node-b"},
	  "reason":"NodeNotReady","message":"Node node-b status is now: NodeNotReady","type":"Warning","series":{"count":2,"lastObservedTime":"`+recent+`"}}]`)
	return f
}

func TestClusterScraper(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	f := newFakeCluster(now)
	srv := httptest.NewServer(f)
	defer srv.Close()
	kc, err := KubeEnv{APIURL: srv.URL, Token: "test-token"}.API()
	if err != nil {
		t.Fatal(err)
	}
	logs := &logRec{}
	s := NewClusterScraper(kc, logs)
	s.now = func() time.Time { return now }
	s.MinBackoff, s.MaxBackoff = 5*time.Millisecond, 20*time.Millisecond
	defer s.Close()
	r, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pay := map[string]string{
		"k8s.namespace.name": "shop", "k8s.pod.name": "payments-api-6b8d9c7f4-abcde", "k8s.pod.uid": "u-pay", "k8s.node.name": "node-a",
		"k8s.pod.phase": "Running", "trooper.k8s.pod.reason": "OOMKilled", "trooper.k8s.workload.kind": "Deployment",
		"trooper.k8s.workload.name": "payments-api", "trooper.service": "payments-api",
	}
	job := map[string]string{"k8s.pod.uid": "u-job", "trooper.k8s.workload.kind": "CronJob", "trooper.k8s.workload.name": "invoice-batch",
		"k8s.pod.phase": "Succeeded", "trooper.service": "invoice", "trooper.k8s.pod.reason": ""}
	wl := func(ns, kind, name string) map[string]string {
		return map[string]string{"k8s.namespace.name": ns, "trooper.k8s.workload.kind": kind, "trooper.k8s.workload.name": name}
	}
	tests := []struct {
		metric string
		match  map[string]string
		want   float64
		absent bool
	}{
		{emit.K8sNodeReady, map[string]string{"k8s.node.name": "node-a"}, 1, false},
		{emit.K8sNodeReady, map[string]string{"k8s.node.name": "node-b"}, 0, false},
		{emit.K8sNodeAllocatableCPU, map[string]string{"k8s.node.name": "node-a"}, 3.92, false},
		{emit.K8sNodeAllocatableMemory, map[string]string{"k8s.node.name": "node-a"}, 15 << 30, false},
		{emit.K8sNodeAllocatableMemory, map[string]string{"k8s.node.name": "node-b"}, 16000000 * 1024, false},
		{emit.K8sNodePods, map[string]string{"k8s.node.name": "node-a"}, 1, false},
		{emit.K8sNodePods, map[string]string{"k8s.node.name": "node-b"}, 0, false}, // Succeeded pod not counted
		{emit.K8sPodReady, pay, 0, false},
		{emit.K8sPodRestarts, pay, 4, false},
		{emit.K8sPodCPURequest, pay, 0.25, false},
		{emit.K8sPodCPULimit, pay, 1, false},
		{emit.K8sPodMemoryRequest, pay, 128 << 20, false},
		{emit.K8sPodMemoryLimit, pay, 512 << 20, false},
		{emit.K8sPodRestarts, job, 0, false},
		{emit.K8sPodCPULimit, job, 0, true},
		{emit.K8sWorkloadDesired, wl("shop", "Deployment", "payments-api"), 3, false},
		{emit.K8sWorkloadReady, wl("shop", "Deployment", "payments-api"), 2, false},
		{emit.K8sWorkloadAvailable, wl("shop", "Deployment", "payments-api"), 2, false},
		{emit.K8sWorkloadUpdated, wl("shop", "Deployment", "payments-api"), 3, false},
		{emit.K8sWorkloadAvailable, wl("data", "StatefulSet", "kafka"), 3, false}, // availableReplicas missing → ready
		{emit.K8sWorkloadDesired, wl("tt", "DaemonSet", "trooper-agent-host"), 2, false},
		{emit.K8sWorkloadDesired, wl("shop", "Job", "migrate"), 1, false},
		{emit.K8sWorkloadReady, wl("shop", "Job", "migrate"), 1, false},
		{emit.K8sWorkloadAvailable, wl("shop", "Job", "migrate"), 0, false},
		{emit.K8sWorkloadDesired, wl("shop", "Job", "ancient"), 0, true},                // finished > 24 h ago
		{emit.K8sWorkloadDesired, wl("shop", "Job", "invoice-batch-29123456"), 0, true}, // CronJob child
	}
	for _, tt := range tests {
		v, ok := find(t, r, tt.metric, tt.match)
		if tt.absent {
			if ok {
				t.Errorf("%s %v: unexpected sample %v", tt.metric, tt.match, v)
			}
			continue
		}
		if !ok || !near(v, tt.want) {
			t.Errorf("%s %v = %v (found %v), want %v", tt.metric, tt.match, v, ok, tt.want)
		}
	}
	f.mu.Lock()
	paths := append([]string(nil), f.paths...)
	f.mu.Unlock()
	for _, p := range paths {
		if !strings.HasPrefix(p, "GET ") {
			t.Errorf("cluster mode must be read-only: %s", p)
		}
	}

	// events: first scrape → recent ones only
	if len(logs.recs) != 2 {
		t.Fatalf("first scrape events = %d, want 2 (old one skipped): %+v", len(logs.recs), logs.recs)
	}
	e := logs.recs[0]
	wantAttrs := map[string]string{"k8s.event.reason": "BackOff", "k8s.event.type": "Warning", "k8s.object.kind": "Pod",
		"k8s.object.name": "payments-api-6b8d9c7f4-abcde", "k8s.namespace.name": "shop", "k8s.event.count": "4"}
	got := map[string]string{}
	for _, kv := range e.Attrs {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	for k, v := range wantAttrs {
		if got[k] != v {
			t.Errorf("event attr %s = %q want %q", k, got[k], v)
		}
	}
	if e.Body != "Back-off restarting failed container app" {
		t.Errorf("body = %q", e.Body)
	}
	if got2 := logs.recs[1].Attrs; got2[4].Value.Emit() != "" || got2[5].Value.Emit() != "2" {
		t.Errorf("node event: ns=%q count=%q (series.count)", got2[4].Value.Emit(), got2[5].Value.Emit())
	}

	// watched events: same uid+count → nothing; count bumped → once more; the old event bumped →
	// sent now; a non-Warning event is ignored
	ev := func(typ, uid, msg string, count int) string {
		return fmt.Sprintf(`{"type":%q,"object":{"metadata":{"uid":%q,"namespace":"shop","name":%q,"resourceVersion":"%d"},
		 "involvedObject":{"kind":"Pod","name":"x","namespace":"shop"},"reason":"BackOff","message":%q,"type":"Warning","count":%d,
		 "lastTimestamp":%q}}`, typ, uid, uid, 200+count, msg, count, now.Format(time.RFC3339))
	}
	steps := []struct {
		name string
		line string
		want int
	}{
		{"unchanged", ev("MODIFIED", "e1", "Back-off restarting failed container app", 4), 0},
		{"count bump", ev("MODIFIED", "e1", "Back-off restarting failed container app", 5), 1},
		{"old event recurs", ev("MODIFIED", "e-old", "old", 2), 1},
		{"new event", ev("ADDED", "e9", "Liveness probe failed", 1), 1},
		{"normal event", `{"type":"ADDED","object":{"metadata":{"uid":"n1","resourceVersion":"300"},"type":"Normal","reason":"Pulled","count":1}}`, 0},
	}
	for _, st := range steps {
		logs.reset()
		f.push("/api/v1/events", st.line)
		// a marker event proves the line above was processed
		f.push("/api/v1/events", ev("ADDED", "marker-"+st.name, "marker", 1))
		waitFor(t, func() bool { return logs.has("marker") })
		if n := logs.count() - 1; n != st.want {
			t.Errorf("%s: events = %d want %d", st.name, n, st.want)
		}
	}

	// watched pod change shows up in the next snapshot
	f.push("/api/v1/pods", `{"type":"DELETED","object":{"metadata":{"name":"payments-api-6b8d9c7f4-abcde","namespace":"shop","uid":"u-pay","resourceVersion":"400"}}}`)
	waitFor(t, func() bool { return s.pods.ResourceVersion() == "400" })
	r, err = s.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := find(t, r, emit.K8sPodReady, pay); ok {
		t.Error("deleted pod still reported")
	}
	if v, _ := find(t, r, emit.K8sNodePods, map[string]string{"k8s.node.name": "node-a"}); v != 0 {
		t.Errorf("node-a pods after delete = %v", v)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClusterScraperAuthError(t *testing.T) {
	srv := httptest.NewServer(&fakeAPI{objects: map[string]string{}})
	defer srv.Close()
	kc, _ := KubeEnv{APIURL: srv.URL, Token: "wrong"}.API()
	s := NewClusterScraper(kc, nil)
	defer s.Close()
	_, err := s.Scrape(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if _, err := (KubeEnv{}).API(); err == nil {
		t.Fatal("no API URL: want error")
	}
}

func TestCloudAttrsFromNode(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(newFakeCluster(now))
	defer srv.Close()
	f := newFakeCluster(now)
	f.set("/api/v1/nodes/node-a", "")
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/nodes/node-a" {
			fmt.Fprint(w, `{"metadata":{"name":"node-a","labels":{"topology.kubernetes.io/region":"eu-central-1"}},"spec":{"providerID":"aws:///eu-central-1a/i-1"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv2.Close()
	tests := []struct {
		name         string
		c            Config
		prov, region string
	}{
		{"env wins", Config{CloudProvider: "gcp", CloudRegion: "europe-west1", Kube: KubeEnv{APIURL: srv2.URL, Token: "x", NodeName: "node-a"}}, "gcp", "europe-west1"},
		{"from own node", Config{Kube: KubeEnv{APIURL: srv2.URL, Token: "x", NodeName: "node-a"}}, "aws", "eu-central-1"},
		{"from first node", Config{Kube: KubeEnv{APIURL: srv.URL, Token: "test-token"}}, "aws", "eu-central-1"},
		{"outside k8s", Config{}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[string]string{}
			for _, kv := range cloudAttrs(context.Background(), tt.c) {
				got[string(kv.Key)] = kv.Value.Emit()
			}
			if got["cloud.provider"] != tt.prov || got["cloud.region"] != tt.region {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func attrKey(k string) attribute.Key { return attribute.Key(k) }
