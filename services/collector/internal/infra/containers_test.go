package infra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

const (
	cgID1 = "1111111111111111111111111111111111111111111111111111111111111111"
	cgID2 = "2222222222222222222222222222222222222222222222222222222222222222"
	cgID3 = "3333333333333333333333333333333333333333333333333333333333333333"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeCgroupTree builds a cgroup v2 tree: a docker scope with limits, a nerdctl scope without
// limits, a bare containerd id with a nested libpod-style child, and unrelated slices.
func fakeCgroupTree(t *testing.T, usage1, periods, throttled float64) string {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"cgroup.controllers": "cpu memory io pids\n"})
	writeFiles(t, filepath.Join(root, "system.slice", "docker-"+cgID1+".scope"), map[string]string{
		"cpu.stat":       "usage_usec " + ftoa(usage1) + "\nuser_usec 1\nsystem_usec 1\nnr_periods " + ftoa(periods) + "\nnr_throttled " + ftoa(throttled) + "\nthrottled_usec 0\n",
		"memory.current": "209715200\n",
		"memory.stat":    "anon 1\nfile 2\ninactive_file 10485760\n",
		"memory.max":     "536870912\n",
		"cpu.max":        "150000 100000\n",
	})
	writeFiles(t, filepath.Join(root, "system.slice", "nerdctl-"+cgID2+".scope"), map[string]string{
		"cpu.stat":       "usage_usec 5000000\nnr_periods 0\nnr_throttled 0\n",
		"memory.current": "1048576\n",
		"memory.max":     "max\n",
		"cpu.max":        "max 100000\n",
	})
	writeFiles(t, filepath.Join(root, "default", cgID3), map[string]string{"cpu.stat": "usage_usec 1\n", "memory.current": "4096\n"})
	writeFiles(t, filepath.Join(root, "default", cgID3, "container"), map[string]string{"cpu.stat": "usage_usec 1\n"})
	writeFiles(t, filepath.Join(root, "user.slice", "user-1000.slice"), map[string]string{"cpu.stat": "usage_usec 99\n", "memory.current": "1\n"})
	return root
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func TestCgroupScraper(t *testing.T) {
	root := fakeCgroupTree(t, 10e6, 100, 10)
	if !CgroupV2(root) || CgroupV2(t.TempDir()) {
		t.Fatal("CgroupV2 detection")
	}
	s := NewCgroupScraper(root)
	t0 := time.Unix(1000, 0)
	s.now = func() time.Time { return t0 }
	r, err := s.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c1 := map[string]string{"container.id": cgID1, "container.name": cgID1[:12], "container.image.name": "", "trooper.service": cgID1[:12], "k8s.pod.name": ""}
	if _, ok := find(t, r, emit.ContainerCPU, c1); ok {
		t.Error("cpu needs two readings")
	}
	for _, tt := range []struct {
		metric string
		want   float64
	}{
		{emit.ContainerMemoryBytes, 200<<20 - 10<<20},
		{emit.ContainerMemoryLimitBytes, 512 << 20},
		{emit.ContainerCPULimit, 1.5},
	} {
		if v, ok := find(t, r, tt.metric, c1); !ok || !near(v, tt.want) {
			t.Errorf("%s = %v (%v), want %v", tt.metric, v, ok, tt.want)
		}
	}
	c2 := map[string]string{"container.id": cgID2}
	if _, ok := find(t, r, emit.ContainerMemoryLimitBytes, c2); ok {
		t.Error("memory.max=max must not report a limit")
	}
	if _, ok := find(t, r, emit.ContainerCPULimit, c2); ok {
		t.Error("cpu.max=max must not report a limit")
	}
	if v, ok := find(t, r, emit.ContainerMemoryBytes, map[string]string{"container.id": cgID3}); !ok || v != 4096 {
		t.Errorf("bare containerd id: %v %v", v, ok)
	}
	ids := map[string]bool{}
	for _, g := range r.Gauges {
		v, _ := g.Attrs.Value(attrKey("container.id"))
		ids[v.AsString()] = true
	}
	if len(ids) != 3 {
		t.Errorf("containers = %v, want exactly the 3 ids (nested child folded, user.slice ignored)", ids)
	}

	// second reading 10 s later: 15 core-seconds more → 1.5 cores; 100 more periods, 50 throttled
	writeFiles(t, filepath.Join(root, "system.slice", "docker-"+cgID1+".scope"), map[string]string{
		"cpu.stat": "usage_usec 25000000\nnr_periods 200\nnr_throttled 60\n",
	})
	s.now = func() time.Time { return t0.Add(10 * time.Second) }
	r, err = s.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := find(t, r, emit.ContainerCPU, c1); !ok || !near(v, 1.5) {
		t.Errorf("cpu = %v (%v), want 1.5", v, ok)
	}
	if v, ok := find(t, r, emit.ContainerThrottled, c1); !ok || !near(v, 0.5) {
		t.Errorf("throttled = %v (%v), want 0.5", v, ok)
	}

	if _, err := NewCgroupScraper(t.TempDir()).Scrape(context.Background()); err == nil {
		t.Error("cgroup v1 / missing tree: want error")
	}
}

type stubScraper struct {
	r   emit.Result
	err error
}

func (s *stubScraper) Scrape(context.Context) (emit.Result, error) { return s.r, s.err }
func (s *stubScraper) Close() error                                { return nil }

func stubResult(ids ...string) emit.Result {
	var r emit.Result
	for _, id := range ids {
		a := ContainerAttrs(id, "n-"+id, "", "", "", "", "", "", "")
		r.Gauge(emit.ContainerMemoryBytes, 1, a...)
		r.Gauge(emit.ContainerCPU, 1, a...)
	}
	return r
}

// TestContainerChain: first source wins for a container id; a failing source is skipped; all
// failing → error.
func TestContainerChain(t *testing.T) {
	ch := &ContainerChain{Sources: []ContainerSource{
		{"docker", &stubScraper{r: stubResult("a", "b")}},
		{"podman", &stubScraper{err: errors.New("permission denied")}},
		{"cgroup", &stubScraper{r: stubResult("b", "c")}},
	}}
	r, err := ch.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, g := range r.Gauges {
		v, _ := g.Attrs.Value(attrKey("container.id"))
		count[v.AsString()]++
	}
	if count["a"] != 2 || count["b"] != 2 || count["c"] != 2 || len(count) != 3 {
		t.Fatalf("samples per id = %v (b must come from docker only)", count)
	}
	all := &ContainerChain{Sources: []ContainerSource{{"docker", &stubScraper{err: errors.New("x")}}, {"cgroup", &stubScraper{err: errors.New("y")}}}}
	if _, err := all.Scrape(context.Background()); err == nil || !strings.Contains(err.Error(), "docker: x") {
		t.Fatalf("all failing: %v", err)
	}
}

func TestContainerSources(t *testing.T) {
	sock, _ := fakeDocker(t)
	podman, _ := fakeDocker(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	c := Config{DockerSocket: filepath.Join(t.TempDir(), "none.sock"), PodmanSocket: podman, FS: HostFS{Sys: t.TempDir()}}
	got := ContainerSources(c)
	if len(got) != 1 || got[0].Name != "podman" || got[0].addr != podman {
		t.Fatalf("podman only: %+v", got)
	}
	c.DockerSocket = sock
	cg := filepath.Join(c.FS.Sys, "fs", "cgroup")
	writeFiles(t, cg, map[string]string{"cgroup.controllers": "cpu\n"})
	got = ContainerSources(c)
	want := []string{"docker", "podman"}
	if runtime.GOOS == "linux" {
		want = append(want, "cgroup")
	}
	if len(got) != len(want) {
		t.Fatalf("sources = %+v want %v", got, want)
	}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("sources[%d] = %s want %s", i, got[i].Name, n)
		}
	}
	// the podman source speaks the Docker-compatible API
	r, err := got[1].new().Scrape(context.Background())
	if err != nil || len(r.Gauges) == 0 {
		t.Fatalf("podman scrape: %v %d", err, len(r.Gauges))
	}
}

func TestPodmanSockets(t *testing.T) {
	if got := PodmanSockets("/x/podman.sock", HostFS{}); len(got) != 1 || got[0] != "/x/podman.sock" {
		t.Fatalf("override: %v", got)
	}
	root := t.TempDir()
	writeFiles(t, filepath.Join(root, "run", "user", "1000", "podman"), map[string]string{"podman.sock": ""})
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/501")
	got := PodmanSockets("", HostFS{Root: root})
	want := []string{"/run/podman/podman.sock", "/run/user/501/podman/podman.sock", filepath.Join(root, "run/podman/podman.sock"),
		filepath.Join(root, "run/user/1000/podman/podman.sock")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestGPUReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake")
	}
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + args + "\ncat <<'X'\n" +
		"0, GPU-aaaa-1111, NVIDIA A100-SXM4-40GB, 87, 30000, 40960, 64, 250.50\n" +
		"1, GPU-bbbb-2222, NVIDIA A100-SXM4-40GB, [N/A], 0, 40960, 35, [Not Supported]\n" +
		"X\n"
	bin := filepath.Join(dir, "nvidia-smi")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g := NewGPUReader(bin)
	var r emit.Result
	g.Scrape(context.Background(), &r)
	b, _ := os.ReadFile(args)
	if !strings.Contains(string(b), "--query-gpu="+GPUQuery) || !strings.Contains(string(b), "--format=csv,noheader,nounits") {
		t.Fatalf("args = %s", b)
	}
	g0 := map[string]string{"gpu.index": "0", "gpu.uuid": "GPU-aaaa-1111", "gpu.model": "NVIDIA A100-SXM4-40GB"}
	for _, tt := range []struct {
		m    string
		want float64
	}{
		{emit.HostGPUUtilization, 0.87}, {emit.HostGPUMemoryBytes, 30000 << 20}, {emit.HostGPUMemory, 30000.0 / 40960},
		{emit.HostGPUTemperature, 64}, {emit.HostGPUPower, 250.5},
	} {
		if v, ok := find(t, r, tt.m, g0); !ok || !near(v, tt.want) {
			t.Errorf("%s = %v (%v), want %v", tt.m, v, ok, tt.want)
		}
	}
	g1 := map[string]string{"gpu.index": "1"}
	if _, ok := find(t, r, emit.HostGPUUtilization, g1); ok {
		t.Error("[N/A] utilization must be skipped")
	}
	if _, ok := find(t, r, emit.HostGPUPower, g1); ok {
		t.Error("[Not Supported] power must be skipped")
	}
	if v, ok := find(t, r, emit.HostGPUMemory, g1); !ok || v != 0 {
		t.Errorf("gpu1 memory = %v %v", v, ok)
	}

	// absent binary / failing binary: silently nothing
	var none *GPUReader
	none.Scrape(context.Background(), &r)
	t.Setenv("PATH", dir+"/nope")
	if NewGPUReader("") != nil {
		t.Error("no nvidia-smi on PATH → nil reader")
	}
	var r2 emit.Result
	(&GPUReader{Bin: filepath.Join(dir, "missing"), Timeout: time.Second}).Scrape(context.Background(), &r2)
	if len(r2.Gauges) != 0 {
		t.Error("failing nvidia-smi must yield nothing")
	}
}
