package infrasim

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// Sim drives the world and ships its telemetry.
type Sim struct {
	S           *Sender
	RT          *world.Runtime
	Log         *slog.Logger
	PostgresURL string // serverless_functions upsert (skipped when the table does not exist)

	w       *World
	faas    *faasWorld
	lastErr string
}

// New builds a simulation sending to ingest base URL with the ingest key.
func New(base, key string, rt *world.Runtime, postgresURL string, log *slog.Logger) *Sim {
	now := time.Now()
	return &Sim{
		S:  &Sender{Base: base, Key: key, HC: &http.Client{Timeout: 10 * time.Second}},
		RT: rt, Log: log, PostgresURL: postgresURL,
		w: NewWorld(now), faas: newFaasWorld(),
	}
}

// Run blocks until ctx ends: host / container / process metrics every 15 s, cluster objects every
// 30 s, Lambda metrics every 60 s (contracts §20, §21).
func (s *Sim) Run(ctx context.Context) {
	if s.PostgresURL != "" {
		if n, err := UpsertFunctions(ctx, s.PostgresURL, s.faas.fns); err != nil {
			s.Log.Warn("infra-sim: serverless_functions upsert failed", "err", err)
		} else if n >= 0 {
			s.Log.Info("infra-sim: serverless_functions upserted", "rows", n)
		} else {
			s.Log.Info("infra-sim: serverless_functions table not present (migration 011), skipping metadata")
		}
	}
	s.Log.Info("infra-sim running", "cluster", ClusterName, "nodes", len(s.w.nodes), "pods", len(s.w.pods), "functions", len(s.faas.fns))
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	last := time.Now()
	n := 0
	lastFaas := time.Now().Truncate(time.Minute)
	for {
		now := time.Now()
		evs := s.w.step(now, now.Sub(last), s.RT.Active("reco-db-slow"))
		last = now
		evs = append(evs, s.scenarioEvents(now, n)...)
		s.report(ctx, s.S.Metrics(ctx, s.hostBatches(now)...))
		if n%2 == 0 {
			s.report(ctx, s.S.Metrics(ctx, s.clusterBatch(now)))
		}
		s.report(ctx, s.S.Logs(ctx, []KV{{"service.name", "k8s"}, {"k8s.cluster.name", ClusterName}}, eventLogs(evs)))
		if m := now.Truncate(time.Minute); m.After(lastFaas) {
			// the provider's (previous, complete) minute, like CloudWatch
			s.report(ctx, s.S.Metrics(ctx, s.faas.batch(lastFaas, s.RT)))
			lastFaas = m
		}
		n++
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Sim) report(_ context.Context, err error) {
	if err == nil {
		if s.lastErr != "" {
			s.Log.Info("infra-sim: ingest recovered")
			s.lastErr = ""
		}
		return
	}
	if err.Error() != s.lastErr {
		s.Log.Warn("infra-sim: send failed", "err", err)
		s.lastErr = err.Error()
	}
}

func eventLogs(evs []event) []Log {
	out := make([]Log, 0, len(evs))
	for _, e := range evs {
		out = append(out, Log{Time: e.at, Body: e.msg, Attrs: []KV{
			{"k8s.event.reason", e.reason}, {"k8s.event.type", "Warning"}, {"k8s.object.kind", e.kind},
			{"k8s.object.name", e.name}, {"k8s.namespace.name", e.ns}, {"k8s.event.count", e.count},
		}})
	}
	return out
}

// scenarioEvents: readiness probe failures on payments-api while its provider errors, and a
// recurring image pull back-off on a stale CronJob image (every ~10 min).
func (s *Sim) scenarioEvents(now time.Time, n int) []event {
	var out []event
	if s.RT.Active("payments-provider-errors") && n%8 == 3 {
		for _, p := range s.w.pods {
			if p.workload == "payments-api" {
				out = append(out, event{at: now, reason: "Unhealthy", msg: "Readiness probe failed: HTTP probe failed with statuscode: 503",
					kind: "Pod", name: p.name, ns: p.ns, uid: uuid("ev/unhealthy/" + p.uid), count: n/8 + 1})
				break
			}
		}
	}
	if n%40 == 5 {
		out = append(out, event{at: now, reason: "FailedScheduling", msg: "0/3 nodes are available: 3 Insufficient memory. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod.",
			kind: "Pod", name: "nightly-report-29361440-k2x7q", ns: Namespace, uid: uuid("ev/sched/nightly"), count: n/40 + 1})
	}
	return out
}

func agentResource(hostName, hostID, node string) []KV {
	r := []KV{{"service.name", "trooper-agent"}, {"service.version", "0.2.0"}, {"host.name", hostName}, {"host.id", hostID},
		{"os.type", "linux"}, {"host.arch", "amd64"}, {"cloud.provider", "aws"}, {"cloud.region", Region}}
	if node != "" {
		r = append(r, KV{"k8s.cluster.name", ClusterName}, KV{"k8s.node.name", node})
	}
	return r
}

func containerAttrs(c *container, p *pod, svc string) []KV {
	a := []KV{{"container.id", c.id}, {"container.name", c.name}, {"container.image.name", c.image}}
	if p == nil {
		return append(a, KV{"k8s.namespace.name", ""}, KV{"k8s.pod.name", ""}, KV{"k8s.pod.uid", ""},
			KV{"trooper.k8s.workload.kind", ""}, KV{"trooper.k8s.workload.name", ""}, KV{"trooper.service", svc})
	}
	return append(a, KV{"k8s.namespace.name", p.ns}, KV{"k8s.pod.name", p.name}, KV{"k8s.pod.uid", p.uid},
		KV{"trooper.k8s.workload.kind", p.kind}, KV{"trooper.k8s.workload.name", p.workload}, KV{"trooper.service", svc})
}

type proc struct {
	pid             int
	exe, cmd, ctrID string
	cpu, mem        float64
	threads, fds    float64
}

// hostBatches: one batch per node (host + its containers + top processes) and the docker host.
func (s *Sim) hostBatches(now time.Time) []*Batch {
	w := s.w
	f := factor(now)
	var out []*Batch
	for _, n := range w.nodes {
		b := NewBatch(agentResource(n.name, n.hostID, n.name), now)
		var cpu, mem, rx, tx float64
		procs := []proc{
			{pid: 1, exe: "systemd", cmd: "/usr/lib/systemd/systemd", cpu: 0.004, mem: 14 * mib, threads: 1, fds: 120},
			{pid: 2841, exe: "kubelet", cmd: "/usr/bin/kubelet", cpu: 0.06 * w.noise(0.3), mem: 110 * mib * w.noise(0.02), threads: 18, fds: 64},
			{pid: 2790, exe: "containerd", cmd: "/usr/bin/containerd", cpu: 0.03 * w.noise(0.3), mem: 72 * mib * w.noise(0.02), threads: 21, fds: 210},
		}
		for _, p := range n.pods {
			svc := p.service
			for _, c := range p.containers {
				a := containerAttrs(c, p, svc)
				ccpu := c.baseCPU * f * w.noise(0.15)
				if p.oom && s.RT.Active("reco-db-slow") {
					ccpu *= 1.8 // seq scans + GC pressure
				}
				if c.cpuLim > 0 {
					ccpu = min(ccpu, c.cpuLim)
				}
				b.Gauge("trooper.container.cpu", ccpu, a...)
				b.Gauge("trooper.container.memory_bytes", c.mem, a...)
				if c.cpuLim > 0 {
					b.Gauge("trooper.container.cpu_limit", c.cpuLim, a...)
					throttled := 0.0
					if ccpu > c.cpuLim*0.7 {
						throttled = min(1, (ccpu/c.cpuLim-0.7)*1.5)
					}
					b.Gauge("trooper.container.throttled", throttled, a...)
				}
				if c.memLim > 0 {
					b.Gauge("trooper.container.memory_limit_bytes", c.memLim, a...)
				}
				b.Gauge("trooper.container.restarts", float64(c.restarts), a...)
				cin, cout := c.netIn*f*w.noise(0.2), c.netOut*f*w.noise(0.2)
				b.Gauge("trooper.container.net_in", cin, a...)
				b.Gauge("trooper.container.net_out", cout, a...)
				cpu, mem, rx, tx = cpu+ccpu, mem+c.mem, rx+cin, tx+cout
				procs = append(procs, proc{pid: c.pid, exe: c.exe, cmd: cmdFor(c.exe), ctrID: c.id, cpu: ccpu * 0.97, mem: c.mem * 0.92,
					threads: threadsFor(c.exe), fds: 40 + float64(fnvN(c.id)%200)})
			}
		}
		sys := 0.05 * n.cores * w.noise(0.2)
		b.Gauge("trooper.host.cpu", min(1, (cpu+sys)/n.cores))
		b.Gauge("trooper.host.cpu_cores", n.cores)
		used := mem + 2.1*gib
		b.Gauge("trooper.host.memory", used/n.memTotal)
		b.Gauge("trooper.host.memory_bytes", used)
		b.Gauge("trooper.host.disk", n.disk+float64(now.Unix()%86400)/86400*0.01)
		b.Gauge("trooper.host.disk_read", n.diskRead*f*w.noise(0.4))
		b.Gauge("trooper.host.disk_write", n.diskWrite*f*w.noise(0.4))
		b.Gauge("trooper.host.net_in", rx*1.05)
		b.Gauge("trooper.host.net_out", tx*1.05)
		b.Gauge("trooper.host.load1", (cpu+sys)*1.15)
		b.Gauge("trooper.host.uptime_s", now.Sub(n.bootAt).Seconds())
		addProcs(b, procs, 12)
		b.Gauge("trooper.collector.up", 1, KV{"trooper.collector.target", n.name}, KV{"trooper.collector.kind", "host"})
		b.Gauge("trooper.collector.up", 1, KV{"trooper.collector.target", n.name}, KV{"trooper.collector.kind", "kubelet"})
		out = append(out, b)
	}

	d := w.docker
	b := NewBatch(agentResource(d.name, d.hostID, ""), now)
	var cpu, mem, rx, tx float64
	procs := []proc{
		{pid: 1, exe: "systemd", cmd: "/sbin/init", cpu: 0.003, mem: 12 * mib, threads: 1, fds: 90},
		{pid: 812, exe: "dockerd", cmd: "/usr/bin/dockerd", cpu: 0.04 * w.noise(0.3), mem: 140 * mib, threads: 32, fds: 310},
	}
	// CI bursts: a build every ~6 minutes saturates buildkitd for ~2 minutes
	burst := now.Unix()%360 < 120
	for _, c := range d.containers {
		a := containerAttrs(c, nil, c.name)
		ccpu := c.baseCPU * w.noise(0.2)
		if c.name == "buildkitd" && burst {
			ccpu = 3.4 * w.noise(0.1)
		}
		b.Gauge("trooper.container.cpu", ccpu, a...)
		b.Gauge("trooper.container.memory_bytes", c.mem*w.noise(0.05), a...)
		if c.cpuLim > 0 {
			b.Gauge("trooper.container.cpu_limit", c.cpuLim, a...)
			b.Gauge("trooper.container.throttled", 0, a...)
		}
		if c.memLim > 0 {
			b.Gauge("trooper.container.memory_limit_bytes", c.memLim, a...)
		}
		b.Gauge("trooper.container.restarts", 0, a...)
		cin, cout := c.netIn*w.noise(0.3), c.netOut*w.noise(0.3)
		b.Gauge("trooper.container.net_in", cin, a...)
		b.Gauge("trooper.container.net_out", cout, a...)
		cpu, mem, rx, tx = cpu+ccpu, mem+c.mem, rx+cin, tx+cout
		procs = append(procs, proc{pid: c.pid, exe: c.exe, cmd: cmdFor(c.exe), ctrID: c.id, cpu: ccpu, mem: c.mem * 0.9, threads: 24, fds: 80})
	}
	b.Gauge("trooper.host.cpu", min(1, (cpu+0.2)/d.cores))
	b.Gauge("trooper.host.cpu_cores", d.cores)
	b.Gauge("trooper.host.memory", (mem+3*gib)/d.memTotal)
	b.Gauge("trooper.host.memory_bytes", mem+3*gib)
	b.Gauge("trooper.host.disk", 0.71)
	b.Gauge("trooper.host.disk_read", 2.1e6*w.noise(0.5))
	b.Gauge("trooper.host.disk_write", 6.5e6*w.noise(0.5))
	b.Gauge("trooper.host.net_in", rx)
	b.Gauge("trooper.host.net_out", tx)
	b.Gauge("trooper.host.load1", cpu*1.2+0.2)
	b.Gauge("trooper.host.uptime_s", now.Sub(d.bootAt).Seconds())
	addProcs(b, procs, 10)
	b.Gauge("trooper.collector.up", 1, KV{"trooper.collector.target", d.name}, KV{"trooper.collector.kind", "host"})
	b.Gauge("trooper.collector.up", 1, KV{"trooper.collector.target", d.name}, KV{"trooper.collector.kind", "docker"})
	return append(out, b)
}

func cmdFor(exe string) string {
	switch exe {
	case "node":
		return "/usr/local/bin/node"
	case "python3":
		return "/usr/local/bin/python3"
	case "java":
		return "/opt/java/openjdk/bin/java"
	}
	return "/usr/local/bin/" + exe
}

func threadsFor(exe string) float64 {
	switch exe {
	case "node":
		return 11
	case "python3":
		return 6
	case "java":
		return 48
	}
	return 14
}

// addProcs reports the top n processes (half by CPU, rest by memory — like the agent).
func addProcs(b *Batch, procs []proc, n int) {
	byCPU := append([]proc(nil), procs...)
	sort.SliceStable(byCPU, func(i, j int) bool { return byCPU[i].cpu > byCPU[j].cpu })
	picked := map[int]bool{}
	var out []proc
	for _, p := range byCPU {
		if len(out) >= (n+1)/2 {
			break
		}
		out, picked[p.pid] = append(out, p), true
	}
	byMem := append([]proc(nil), procs...)
	sort.SliceStable(byMem, func(i, j int) bool { return byMem[i].mem > byMem[j].mem })
	for _, p := range byMem {
		if len(out) >= n {
			break
		}
		if !picked[p.pid] {
			out, picked[p.pid] = append(out, p), true
		}
	}
	for _, p := range out {
		a := []KV{{"process.pid", p.pid}, {"process.executable.name", p.exe}, {"process.command", p.cmd}, {"container.id", p.ctrID}}
		b.Gauge("trooper.process.cpu", p.cpu, a...)
		b.Gauge("trooper.process.memory_bytes", p.mem, a...)
		b.Gauge("trooper.process.threads", p.threads, a...)
		b.Gauge("trooper.process.fds", p.fds, a...)
	}
}

// clusterBatch: what the cluster-mode Deployment reports.
func (s *Sim) clusterBatch(now time.Time) *Batch {
	w := s.w
	b := NewBatch([]KV{{"service.name", "trooper-agent"}, {"service.version", "0.2.0"}, {"k8s.cluster.name", ClusterName},
		{"cloud.provider", "aws"}, {"cloud.region", Region}}, now)
	for _, n := range w.nodes {
		a := []KV{{"k8s.node.name", n.name}}
		b.Gauge("trooper.k8s.node.ready", 1, a...)
		b.Gauge("trooper.k8s.node.allocatable_cpu", n.allocCPU, a...)
		b.Gauge("trooper.k8s.node.allocatable_memory", n.allocMem, a...)
		b.Gauge("trooper.k8s.node.pods", float64(len(n.pods)), a...)
	}
	for _, p := range w.pods {
		a := []KV{{"k8s.namespace.name", p.ns}, {"k8s.pod.name", p.name}, {"k8s.pod.uid", p.uid}, {"k8s.node.name", p.node},
			{"k8s.pod.phase", p.phase}, {"trooper.k8s.pod.reason", p.reason}, {"trooper.k8s.workload.kind", p.kind},
			{"trooper.k8s.workload.name", p.workload}, {"trooper.service", p.service}}
		ready := 0.0
		if p.ready {
			ready = 1
		}
		b.Gauge("trooper.k8s.pod.ready", ready, a...)
		b.Gauge("trooper.k8s.pod.restarts", float64(p.restarts()), a...)
		var cr, cl, mr, ml float64
		for _, c := range p.containers {
			cr, cl, mr, ml = cr+c.cpuReq, cl+c.cpuLim, mr+c.memReq, ml+c.memLim
		}
		if cr > 0 {
			b.Gauge("trooper.k8s.pod.cpu_request", cr, a...)
		}
		if cl > 0 {
			b.Gauge("trooper.k8s.pod.cpu_limit", cl, a...)
		}
		if mr > 0 {
			b.Gauge("trooper.k8s.pod.memory_request", mr, a...)
		}
		if ml > 0 {
			b.Gauge("trooper.k8s.pod.memory_limit", ml, a...)
		}
	}
	for _, wl := range w.workloads {
		a := []KV{{"k8s.namespace.name", wl.ns}, {"trooper.k8s.workload.kind", wl.kind}, {"trooper.k8s.workload.name", wl.name}}
		desired, ready := len(wl.pods), 0
		for _, p := range wl.pods {
			if p.ready {
				ready++
			}
		}
		if wl.cron {
			desired, ready = 1, 0
			if now.Unix()%3600 < 180 {
				ready = 1 // running ~3 min every hour
			}
			b.Gauge("trooper.k8s.workload.desired", 1, a...)
			b.Gauge("trooper.k8s.workload.ready", float64(ready), a...)
			b.Gauge("trooper.k8s.workload.available", 1, a...)
			b.Gauge("trooper.k8s.workload.updated", float64(ready), a...)
			continue
		}
		b.Gauge("trooper.k8s.workload.desired", float64(desired), a...)
		b.Gauge("trooper.k8s.workload.ready", float64(ready), a...)
		b.Gauge("trooper.k8s.workload.available", float64(ready), a...)
		b.Gauge("trooper.k8s.workload.updated", float64(desired), a...)
	}
	b.Gauge("trooper.collector.up", 1, KV{"trooper.collector.target", ClusterName}, KV{"trooper.collector.kind", "k8s"})
	return b
}
