// Package infrasim simulates what trooper-agent's host / cluster modes (contracts §20) and the AWS
// Lambda integration (contracts §21) would report for the demo world: an EKS cluster `demo-eks`
// (3 nodes) running the demo services as pods, a non-Kubernetes docker host, Warning events and
// four Lambda functions. Nothing here runs for real — it is shaped telemetry sent over OTLP to
// ingest-gw with the demo ingest key, driven by the same scenario clock as the real services
// (reco-db-slow → recommendation-api pod OOMKilled / restarting).
package infrasim

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// Cluster / cloud identity of the simulated world.
const (
	ClusterName = "demo-eks"
	Region      = "eu-central-1"
	Namespace   = "shop"
)

const (
	mib = 1 << 20
	gib = 1 << 30
)

type container struct {
	name, id, image string
	cpuReq, cpuLim  float64 // cores
	memReq, memLim  float64 // bytes
	baseCPU         float64 // cores at factor 1
	baseMem         float64 // bytes
	exe             string  // main process
	pid             int
	mem             float64 // current working set
	netIn, netOut   float64 // byte/s at factor 1
	restarts        int
}

type pod struct {
	ns, name, uid, node string
	kind, workload      string
	service             string
	phase               string
	ready               bool
	reason              string
	reasonAt            time.Time
	notReadyUntil       time.Time
	containers          []*container
	// oom: memory ramps to the limit and the container is OOM-killed (reco-db-slow scenario)
	oom bool
}

type node struct {
	name, hostID, instance string
	cores                  float64
	memTotal               float64
	allocCPU, allocMem     float64
	bootAt                 time.Time
	disk                   float64
	diskRead, diskWrite    float64 // byte/s baseline
	pods                   []*pod
}

type workload struct {
	ns, kind, name string
	pods           []*pod
	cron           bool
}

// dockerHost is a non-Kubernetes host with plain docker containers.
type dockerHost struct {
	name, hostID string
	cores        float64
	memTotal     float64
	bootAt       time.Time
	containers   []*container
}

// World is the simulated infrastructure.
type World struct {
	nodes     []*node
	pods      []*pod
	workloads []*workload
	docker    *dockerHost
	rng       *rand.Rand
}

func hash(s string, n int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	const alphabet = "bcdfghjklmnpqrstvwxz2456789"
	v := h.Sum64()
	var sb strings.Builder
	for range n {
		sb.WriteByte(alphabet[v%uint64(len(alphabet))])
		v /= uint64(len(alphabet))
	}
	return sb.String()
}

func hexID(s string, n int) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		h := fnv.New64a()
		_, _ = fmt.Fprintf(h, "%s/%d", s, i)
		fmt.Fprintf(&sb, "%016x", h.Sum64())
	}
	return sb.String()[:n]
}

func uuid(s string) string {
	x := hexID(s, 32)
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// exeFor maps the mock language of a service to its main process.
func exeFor(s world.Service) string {
	switch s.Language {
	case "Node.js":
		return "node"
	case "Python":
		return "python3"
	case "Java", "Kotlin":
		return "java"
	}
	return s.Name
}

// profile is the per-service resource shape (cores / bytes at factor 1).
var profile = map[string]struct {
	cpu, mem, cpuLim, memLim, netIn float64
}{
	"orders-api":            {0.35, 380 * mib, 1, 768 * mib, 220e3},
	"payments-api":          {0.18, 140 * mib, 0.5, 256 * mib, 90e3},
	"catalog-api":           {0.22, 210 * mib, 1, 512 * mib, 260e3},
	"auth-api":              {0.12, 160 * mib, 0.5, 384 * mib, 70e3},
	"storefront-bff":        {0.30, 290 * mib, 1, 512 * mib, 410e3},
	"recommendation-api":    {0.40, 300 * mib, 1, 512 * mib, 120e3},
	"order-events-consumer": {0.15, 200 * mib, 0.5, 512 * mib, 60e3},
	"email-worker":          {0.08, 180 * mib, 0.5, 384 * mib, 20e3},
	"inventory-api":         {0.10, 120 * mib, 0.5, 256 * mib, 40e3},
}

// NewWorld builds the cluster: 3 nodes, the demo services as Deployments in `shop`, system
// DaemonSets / Deployments, a CronJob, and one docker host.
func NewWorld(now time.Time) *World {
	w := &World{rng: rand.New(rand.NewPCG(42, 7))}
	for i, n := range []string{"ip-10-0-1-23", "ip-10-0-2-117", "ip-10-0-3-54"} {
		name := n + "." + Region + ".compute.internal"
		w.nodes = append(w.nodes, &node{
			name: name, hostID: hexID("machine-"+name, 32), instance: "i-0" + hexID(name, 16),
			cores: 4, memTotal: 16 * gib, allocCPU: 3.92, allocMem: 15.1 * gib,
			bootAt: now.Add(-time.Duration(9+i*3) * 24 * time.Hour), disk: 0.38 + 0.07*float64(i),
			diskRead: 180e3, diskWrite: 650e3,
		})
	}
	ni := 0
	nextNode := func() *node {
		n := w.nodes[ni%len(w.nodes)]
		ni++
		return n
	}
	addPod := func(ns, kind, wl, svc string, n *node, cs []*container) *pod {
		var name string
		switch kind {
		case "Deployment":
			name = fmt.Sprintf("%s-%s-%s", wl, hash(wl, 9), hash(fmt.Sprintf("%s/%d", wl, len(w.pods)), 5))
		case "DaemonSet":
			name = fmt.Sprintf("%s-%s", wl, hash(wl+n.name, 5))
		default:
			name = fmt.Sprintf("%s-%s", wl, hash(fmt.Sprintf("%s/%d", wl, len(w.pods)), 5))
		}
		p := &pod{ns: ns, name: name, uid: uuid("pod/" + name), node: n.name, kind: kind, workload: wl, service: svc,
			phase: "Running", ready: true, containers: cs}
		for _, c := range cs {
			c.id = hexID("ctr/"+name+"/"+c.name, 64)
			c.pid = 1000 + int(fnvN(name+c.name)%60000)
			c.mem = c.baseMem
		}
		n.pods = append(n.pods, p)
		w.pods = append(w.pods, p)
		return p
	}
	wlOf := func(ns, kind, name string) *workload {
		for _, x := range w.workloads {
			if x.ns == ns && x.kind == kind && x.name == name {
				return x
			}
		}
		x := &workload{ns: ns, kind: kind, name: name}
		w.workloads = append(w.workloads, x)
		return x
	}

	services := append([]world.Service(nil), world.Services...)
	services = append(services, world.Service{Name: "inventory-api", Version: "v1.4.2", Language: "Go", Instances: 1})
	for _, s := range services {
		pr := profile[s.Name]
		replicas := max(s.Instances, 2) // HA in the cluster even where the demo runs one process
		if s.Name == "recommendation-api" {
			replicas = 2
		}
		for r := 0; r < replicas; r++ {
			c := &container{name: "app", image: fmt.Sprintf("123456789012.dkr.ecr.%s.amazonaws.com/%s:%s", Region, s.Name, s.Version),
				cpuReq: pr.cpuLim / 4, cpuLim: pr.cpuLim, memReq: pr.memLim / 2, memLim: pr.memLim,
				baseCPU: pr.cpu / float64(replicas), baseMem: pr.mem, exe: exeFor(s), netIn: pr.netIn / float64(replicas), netOut: pr.netIn * 1.6 / float64(replicas)}
			p := addPod(Namespace, "Deployment", s.Name, s.Name, nextNode(), []*container{c})
			wlOf(Namespace, "Deployment", s.Name).pods = append(wlOf(Namespace, "Deployment", s.Name).pods, p)
		}
	}
	// the OOM pod: first recommendation-api replica
	for _, p := range w.pods {
		if p.workload == "recommendation-api" {
			p.oom = true
			break
		}
	}
	for _, n := range w.nodes {
		for _, ds := range []struct{ ns, name, image, exe string }{
			{"kube-system", "aws-node", "602401143452.dkr.ecr.eu-central-1.amazonaws.com/amazon-k8s-cni:v1.19.2", "aws-k8s-agent"},
			{"kube-system", "kube-proxy", "602401143452.dkr.ecr.eu-central-1.amazonaws.com/eks/kube-proxy:v1.31.3", "kube-proxy"},
			{"tasktrooper", "trooper-agent-host", "ghcr.io/tasktrooper/trooper-agent:0.2.0", "trooper-agent"},
		} {
			c := &container{name: strings.TrimPrefix(ds.name, "trooper-agent-"), image: ds.image, cpuReq: 0.025, memReq: 64 * mib, memLim: 256 * mib,
				baseCPU: 0.012, baseMem: 48 * mib, exe: ds.exe, netIn: 3e3, netOut: 2e3}
			if ds.name == "trooper-agent-host" {
				c.name = "trooper-agent"
			}
			p := addPod(ds.ns, "DaemonSet", ds.name, "", n, []*container{c})
			if ds.name == "trooper-agent-host" {
				p.service = "trooper-agent"
			}
			wlOf(ds.ns, "DaemonSet", ds.name).pods = append(wlOf(ds.ns, "DaemonSet", ds.name).pods, p)
		}
	}
	for r := 0; r < 2; r++ {
		c := &container{name: "coredns", image: "602401143452.dkr.ecr.eu-central-1.amazonaws.com/eks/coredns:v1.11.4", cpuReq: 0.1, memReq: 70 * mib,
			memLim: 170 * mib, baseCPU: 0.006, baseMem: 22 * mib, exe: "coredns", netIn: 8e3, netOut: 9e3}
		p := addPod("kube-system", "Deployment", "coredns", "", nextNode(), []*container{c}) // k8s-app=kube-dns: no service label
		wlOf("kube-system", "Deployment", "coredns").pods = append(wlOf("kube-system", "Deployment", "coredns").pods, p)
	}
	{
		c := &container{name: "trooper-agent", image: "ghcr.io/tasktrooper/trooper-agent:0.2.0", cpuReq: 0.02, memReq: 64 * mib, memLim: 256 * mib,
			baseCPU: 0.01, baseMem: 60 * mib, exe: "trooper-agent", netIn: 6e3, netOut: 4e3}
		p := addPod("tasktrooper", "Deployment", "trooper-agent-cluster", "trooper-agent", nextNode(), []*container{c})
		wlOf("tasktrooper", "Deployment", "trooper-agent-cluster").pods = append(wlOf("tasktrooper", "Deployment", "trooper-agent-cluster").pods, p)
	}
	w.workloads = append(w.workloads, &workload{ns: Namespace, kind: "CronJob", name: "nightly-report", cron: true})

	w.docker = &dockerHost{name: "ci-runner-1", hostID: hexID("machine-ci-runner-1", 32), cores: 8, memTotal: 32 * gib, bootAt: now.Add(-41 * 24 * time.Hour)}
	for _, c := range []*container{
		{name: "gitlab-runner", image: "gitlab/gitlab-runner:v17.5.3", baseCPU: 0.05, baseMem: 90 * mib, exe: "gitlab-runner", netIn: 15e3, netOut: 8e3},
		{name: "buildkitd", image: "moby/buildkit:v0.18.2", baseCPU: 0.9, baseMem: 1.4 * gib, cpuLim: 4, memLim: 8 * gib, exe: "buildkitd", netIn: 900e3, netOut: 120e3},
	} {
		c.id = hexID("docker/"+c.name, 64)
		c.pid = 2000 + int(fnvN(c.name)%30000)
		c.mem = c.baseMem
		w.docker.containers = append(w.docker.containers, c)
	}
	return w
}

func fnvN(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// noise is a multiplicative jitter around 1.
func (w *World) noise(spread float64) float64 { return 1 + (w.rng.Float64()*2-1)*spread }

// event is a Warning event to log (uid + count dedupe, like the agent).
type event struct {
	at                  time.Time
	reason, msg         string
	kind, name, ns, uid string
	count               int
}

// step advances the simulation by dt: OOM ramp / kill / restart for the oom pod while the
// scenario is on, readiness flaps. It returns the Warning events that happened.
func (w *World) step(now time.Time, dt time.Duration, oomOn bool) []event {
	var evs []event
	for _, p := range w.pods {
		if !p.ready && now.After(p.notReadyUntil) && p.phase == "Running" {
			p.ready = true
		}
		if p.reason != "" && now.Sub(p.reasonAt) > 15*time.Minute {
			p.reason = ""
		}
		for _, c := range p.containers {
			target := c.baseMem * w.noise(0.03)
			if p.oom && oomOn {
				// leak: +~1 MiB/s until the limit (≈ 3–4 min), then the kernel OOM-kills it
				c.mem += (0.9 + w.rng.Float64()*0.4) * mib * dt.Seconds()
				if c.mem >= c.memLim*0.995 {
					c.restarts++
					c.mem = c.baseMem * 0.5
					p.reason, p.reasonAt = "OOMKilled", now
					p.ready, p.notReadyUntil = false, now.Add(40*time.Second)
					evs = append(evs,
						event{at: now, reason: "BackOff", msg: "Back-off restarting failed container " + c.name + " in pod " + p.name + "_" + p.ns + "(" + p.uid + ")",
							kind: "Pod", name: p.name, ns: p.ns, uid: uuid("ev/backoff/" + p.uid), count: c.restarts},
						event{at: now, reason: "OOMKilling", msg: fmt.Sprintf("Memory cgroup out of memory: Killed process %d (%s) total-vm:1843200kB, anon-rss:%dkB", c.pid, c.exe, int(c.memLim/1024)),
							kind: "Node", name: p.node, uid: uuid("ev/oom/" + p.node), count: c.restarts},
					)
				}
				continue
			}
			c.mem += (target - c.mem) * 0.3
		}
	}
	return evs
}

func (p *pod) restarts() int {
	n := 0
	for _, c := range p.containers {
		n += c.restarts
	}
	return n
}

// factor is the traffic multiplier (daily curve like sim.Traffic, peak ~15:00).
func factor(now time.Time) float64 {
	h := float64(now.Hour()) + float64(now.Minute())/60
	return 1 + 0.25*math.Sin((h-9)/24*2*math.Pi)
}
