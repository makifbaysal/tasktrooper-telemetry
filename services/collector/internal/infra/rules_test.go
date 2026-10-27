package infra

import (
	"math"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/process"
)

func TestParseQuantity(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"100m", 0.1, true},
		{"2", 2, true},
		{"1.5", 1.5, true},
		{"250u", 0.00025, true},
		{"128Mi", 128 << 20, true},
		{"1Gi", 1 << 30, true},
		{"64Ki", 64 << 10, true},
		{"1G", 1e9, true},
		{"500M", 5e8, true},
		{"1e3", 1000, true},
		{"2E", 2e18, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := ParseQuantity(tt.in)
			if ok != tt.ok || math.Abs(got-tt.want) > 1e-9*math.Max(1, tt.want) {
				t.Fatalf("ParseQuantity(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func boolp(b bool) *bool { return &b }

func TestOwnersWorkload(t *testing.T) {
	ctrl := func(kind, name string) []OwnerRef {
		return []OwnerRef{{Kind: kind, Name: name, Controller: boolp(true)}}
	}
	api := Owners{
		ReplicaSets: map[string]string{"shop/orders-api-7d9f8b6c5": "orders-api", "shop/bare-rs": ""},
		Jobs:        map[string]string{"shop/nightly-29123456": "nightly", "shop/migrate": ""},
	}
	tests := []struct {
		name     string
		owners   Owners
		meta     ObjectMeta
		kind, wl string
	}{
		{"rs via api", api, ObjectMeta{Namespace: "shop", OwnerReferences: ctrl("ReplicaSet", "orders-api-7d9f8b6c5")}, "Deployment", "orders-api"},
		{"bare rs via api", api, ObjectMeta{Namespace: "shop", OwnerReferences: ctrl("ReplicaSet", "bare-rs")}, "ReplicaSet", "bare-rs"},
		{"rs heuristic with hash label", Owners{}, ObjectMeta{Labels: map[string]string{"pod-template-hash": "5c6f7"}, OwnerReferences: ctrl("ReplicaSet", "web-v2-5c6f7")}, "Deployment", "web-v2"},
		{"rs heuristic without label", Owners{}, ObjectMeta{OwnerReferences: ctrl("ReplicaSet", "payments-api-6b8d9c7f4")}, "Deployment", "payments-api"},
		{"job of cronjob via api", api, ObjectMeta{Namespace: "shop", OwnerReferences: ctrl("Job", "nightly-29123456")}, "CronJob", "nightly"},
		{"standalone job via api", api, ObjectMeta{Namespace: "shop", OwnerReferences: ctrl("Job", "migrate")}, "Job", "migrate"},
		{"job heuristic cronjob", Owners{}, ObjectMeta{OwnerReferences: ctrl("Job", "invoice-batch-29123456")}, "CronJob", "invoice-batch"},
		{"job heuristic plain", Owners{}, ObjectMeta{OwnerReferences: ctrl("Job", "db-migrate")}, "Job", "db-migrate"},
		{"statefulset", Owners{}, ObjectMeta{OwnerReferences: ctrl("StatefulSet", "kafka")}, "StatefulSet", "kafka"},
		{"daemonset", Owners{}, ObjectMeta{OwnerReferences: ctrl("DaemonSet", "trooper-agent-host")}, "DaemonSet", "trooper-agent-host"},
		{"static pod", Owners{}, ObjectMeta{OwnerReferences: ctrl("Node", "node-1")}, "", ""},
		{"bare pod", Owners{}, ObjectMeta{}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, w := tt.owners.Workload(tt.meta)
			if k != tt.kind || w != tt.wl {
				t.Fatalf("got %s/%s want %s/%s", k, w, tt.kind, tt.wl)
			}
		})
	}
}

func TestServiceOf(t *testing.T) {
	tests := []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"tasktrooper.io/service": "orders", "app.kubernetes.io/name": "x", "app": "y"}, "orders"},
		{map[string]string{"app.kubernetes.io/name": "x", "app": "y"}, "x"},
		{map[string]string{"app": "y"}, "y"},
		{map[string]string{"team": "z"}, ""},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := ServiceOf(tt.labels); got != tt.want {
			t.Errorf("ServiceOf(%v) = %q want %q", tt.labels, got, tt.want)
		}
	}
}

func TestPodReason(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	mk := func(f func(p *Pod)) *Pod {
		p := &Pod{}
		p.Status.Phase = "Running"
		p.Status.ContainerStatuses = []ContainerStatus{{Name: "app"}}
		f(p)
		return p
	}
	waiting := func(r string) ContainerState {
		var s ContainerState
		s.Waiting = &struct {
			Reason string `json:"reason"`
		}{r}
		return s
	}
	terminated := func(r string, at time.Time) ContainerState {
		var s ContainerState
		s.Terminated = &struct {
			Reason     string    `json:"reason"`
			ExitCode   int       `json:"exitCode"`
			FinishedAt time.Time `json:"finishedAt"`
		}{Reason: r, ExitCode: 137, FinishedAt: at}
		return s
	}
	tests := []struct {
		name string
		pod  *Pod
		want string
	}{
		{"healthy", mk(func(p *Pod) {}), ""},
		{"evicted", mk(func(p *Pod) { p.Status.Phase, p.Status.Reason = "Failed", "Evicted" }), "Evicted"},
		{"crashloop", mk(func(p *Pod) { p.Status.ContainerStatuses[0].State = waiting("CrashLoopBackOff") }), "CrashLoopBackOff"},
		{"crashloop after oom", mk(func(p *Pod) {
			p.Status.ContainerStatuses[0].State = waiting("CrashLoopBackOff")
			p.Status.ContainerStatuses[0].LastState = terminated("OOMKilled", now.Add(-time.Minute))
		}), "OOMKilled"},
		{"image pull", mk(func(p *Pod) { p.Status.ContainerStatuses[0].State = waiting("ImagePullBackOff") }), "ImagePullBackOff"},
		{"creating is normal", mk(func(p *Pod) { p.Status.ContainerStatuses[0].State = waiting("ContainerCreating") }), ""},
		{"recent oom restart", mk(func(p *Pod) {
			p.Status.ContainerStatuses[0].LastState = terminated("OOMKilled", now.Add(-5*time.Minute))
		}), "OOMKilled"},
		{"old oom restart", mk(func(p *Pod) { p.Status.ContainerStatuses[0].LastState = terminated("OOMKilled", now.Add(-time.Hour)) }), ""},
		{"completed is normal", mk(func(p *Pod) { p.Status.ContainerStatuses[0].State = terminated("Completed", now) }), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PodReason(tt.pod, now); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestSumResource(t *testing.T) {
	c := func(req, lim map[string]string) ContainerSpec {
		return ContainerSpec{Resources: Resources{Requests: req, Limits: lim}}
	}
	tests := []struct {
		name   string
		cs     []ContainerSpec
		limits bool
		want   float64
		ok     bool
	}{
		{"requests sum", []ContainerSpec{c(map[string]string{"cpu": "100m"}, nil), c(map[string]string{"cpu": "250m"}, nil)}, false, 0.35, true},
		{"request defaults to limit", []ContainerSpec{c(nil, map[string]string{"cpu": "1"})}, false, 1, true},
		{"limits all set", []ContainerSpec{c(nil, map[string]string{"cpu": "1"}), c(nil, map[string]string{"cpu": "500m"})}, true, 1.5, true},
		{"one unlimited container", []ContainerSpec{c(nil, map[string]string{"cpu": "1"}), c(nil, nil)}, true, 0, false},
		{"none", []ContainerSpec{c(nil, nil)}, false, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sumResource(tt.cs, tt.limits, "cpu")
			if ok != tt.ok || (ok && math.Abs(got-tt.want) > 1e-9) {
				t.Fatalf("got %v,%v want %v,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestFirstToken(t *testing.T) {
	long := ""
	for range 200 {
		long += "a"
	}
	tests := []struct{ in, want string }{
		{"/usr/bin/python3", "/usr/bin/python3"},
		{"postgres: checkpointer", "postgres"},
		{"nginx: worker process", "nginx"},
		{"java -Dpassword=secret -jar app.jar", "java"},
		{"  node server.js --token=abc", "node"},
		{long, long[:MaxCommand]},
		{"", ""},
	}
	for _, tt := range tests {
		if got := FirstToken(tt.in); got != tt.want {
			t.Errorf("FirstToken(%q) = %q want %q", tt.in, got, tt.want)
		}
	}
}

func TestContainerIDFromCgroup(t *testing.T) {
	id := "9ed4cad0f01bd4414bd41561a3ede20d244f997a4c2c3f751a5bbad619729389"
	tests := []struct{ name, in, want string }{
		{"cgroup v2 docker", "0::/system.slice/docker-" + id + ".scope\n", id},
		{"cgroup v1 kubepods", "12:memory:/kubepods/burstable/pod1234/" + id + "\n", id},
		{"containerd cri", "0::/kubepods.slice/kubepods-burstable.slice/cri-containerd-" + id + ".scope\n", id},
		{"host process", "0::/init.scope\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainerIDFromCgroup(tt.in); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestParseModes(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", []string{"db"}, false},
		{"db", []string{"db"}, false},
		{"host,cluster", []string{"host", "cluster"}, false},
		{" Host , host ,db", []string{"host", "db"}, false},
		{"ebpf", nil, true},
	}
	for _, tt := range tests {
		got, err := ParseModes(tt.in)
		if (err != nil) != tt.wantErr {
			t.Fatalf("ParseModes(%q) err = %v", tt.in, err)
		}
		if !tt.wantErr && (len(got) != len(tt.want) || func() bool {
			for i := range got {
				if got[i] != tt.want[i] {
					return true
				}
			}
			return false
		}()) {
			t.Fatalf("ParseModes(%q) = %v want %v", tt.in, got, tt.want)
		}
	}
}

func TestProviderFromID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"aws:///eu-central-1a/i-0abc", "aws"},
		{"gce://proj/europe-west1-b/node-1", "gcp"},
		{"azure:///subscriptions/x/resourceGroups/y", "azure"},
		{"kind://docker/kind/kind-control-plane", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ProviderFromID(tt.in); got != tt.want {
			t.Errorf("ProviderFromID(%q) = %q want %q", tt.in, got, tt.want)
		}
	}
}

func TestCPUBusy(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur cpu.TimesStat
		want      float64
		ok        bool
	}{
		{"half busy", cpu.TimesStat{User: 10, Idle: 10}, cpu.TimesStat{User: 15, Idle: 15}, 0.5, true},
		{"iowait counts as idle", cpu.TimesStat{User: 0, Idle: 0}, cpu.TimesStat{User: 2, Idle: 4, Iowait: 4}, 0.2, true},
		{"no time passed", cpu.TimesStat{User: 1}, cpu.TimesStat{User: 1}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cpuBusy(tt.prev, tt.cur)
			if ok != tt.ok || math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("got %v,%v want %v,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestSkipDiskAndIface(t *testing.T) {
	disks := map[string]bool{"sda": false, "sda1": true, "nvme0n1": false, "nvme0n1p2": true, "loop3": true, "dm-0": true, "xvda": false, "disk0": false, "vdb1": true}
	for n, want := range disks {
		if got := skipDisk(n); got != want {
			t.Errorf("skipDisk(%q) = %v want %v", n, got, want)
		}
	}
	ifaces := map[string]bool{"eth0": false, "ens5": false, "en0": false, "lo": true, "lo0": true, "veth12ab": true, "docker0": true, "cni0": true, "cali123": true, "flannel.1": true, "br-1234": true, "utun3": true}
	for n, want := range ifaces {
		if got := skipIface(n); got != want {
			t.Errorf("skipIface(%q) = %v want %v", n, got, want)
		}
	}
}

func TestPickTop(t *testing.T) {
	mk := func(pid int32, cpu, rss float64) procSample {
		return procSample{p: &process.Process{Pid: pid}, cpu: cpu, rss: rss, hasCPU: true}
	}
	samples := []procSample{
		mk(1, 0, 100), mk(2, 2.0, 10), mk(3, 0.5, 20), mk(4, 0, 5000), mk(5, 0.1, 4000), mk(6, 0, 3000),
	}
	got := pickTop(samples, 4)
	pids := []int32{}
	for _, s := range got {
		pids = append(pids, s.p.Pid)
	}
	// ceil(4/2)=2 by cpu: 2, 3; then by memory: 4, 5
	want := []int32{2, 3, 4, 5}
	if len(pids) != len(want) {
		t.Fatalf("got %v want %v", pids, want)
	}
	for i := range want {
		if pids[i] != want[i] {
			t.Fatalf("got %v want %v", pids, want)
		}
	}
	if len(pickTop(samples, 0)) != 0 {
		t.Fatal("n=0 must pick nothing")
	}
}

func TestParsePromLine(t *testing.T) {
	tests := []struct {
		line   string
		labels map[string]string
		val    float64
		ok     bool
	}{
		{`container_cpu_cfs_periods_total{container="app",id="/kubepods/x",namespace="shop",pod="orders-1"} 120 1730000000000`,
			map[string]string{"container": "app", "namespace": "shop", "pod": "orders-1", "id": "/kubepods/x"}, 120, true},
		{`m{a="with \"quote\"",b="2"} 3.5`, map[string]string{"a": `with "quote"`, "b": "2"}, 3.5, true},
		{`m 3`, nil, 0, false},
	}
	for _, tt := range tests {
		labels, v, ok := parsePromLine(tt.line)
		if ok != tt.ok || v != tt.val {
			t.Fatalf("%s: got %v,%v", tt.line, v, ok)
		}
		for k, want := range tt.labels {
			if labels[k] != want {
				t.Fatalf("%s: label %s = %q want %q", tt.line, k, labels[k], want)
			}
		}
	}
}
