package store

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// Hosts, containers, processes and Kubernetes (ADR-019, contracts §20). Values are the latest reported minute
// (looking back at least an hour so stopped hosts show as stale), series are the range buckets. An entity that
// stopped reporting more than 3 minutes before its host / cluster did (deleted pod, removed container) is gone.

const (
	staleAfter = 5 * time.Minute
	goneAfter  = 3 * time.Minute
)

// InfraFilter narrows the infra lists.
type InfraFilter struct {
	Cluster, Namespace, Host, Workload, Node, Service, Kind, Name, Q string
	Limit                                                            int
}

func snapFrom(q Q) time.Time {
	if f := q.Now.Add(-time.Hour); f.Before(q.W.From) {
		return f
	}
	return q.W.From
}

func snapTo(q Q) time.Time {
	if t := q.Now.Add(time.Minute); t.Before(q.W.To) {
		return t
	}
	return q.W.To
}

func isStale(now, last time.Time) bool { return now.Sub(last) > staleAfter }

var statusRank = map[model.InfraStatus]int{model.InfraOK: 0, model.InfraStale: 1, model.InfraWarning: 2, model.InfraCritical: 3}

func worse(a, b model.InfraStatus) model.InfraStatus {
	if statusRank[b] > statusRank[a] {
		return b
	}
	return a
}

// HostStatus: critical cpu|memory|disk > 0.95 or the node NotReady, warning > 0.85, stale last data > 5 min.
func HostStatus(cpu, mem, disk float64, notReady bool, last, now time.Time) model.InfraStatus {
	switch m := math.Max(cpu, math.Max(mem, disk)); {
	case isStale(now, last):
		return model.InfraStale
	case notReady || m > 0.95:
		return model.InfraCritical
	case m > 0.85:
		return model.InfraWarning
	}
	return model.InfraOK
}

var failingReasons = map[string]bool{"CrashLoopBackOff": true, "OOMKilled": true, "ImagePullBackOff": true, "ErrImagePull": true}

// PodStatus: critical phase Failed, a failing reason or Running and not ready; warning restarts in the range or
// Pending; stale when the pod stopped reporting > 5 min ago.
func PodStatus(phase, reason string, ready bool, restartsInRange float64, last, now time.Time) model.InfraStatus {
	switch {
	case isStale(now, last):
		return model.InfraStale
	case phase == "Failed" || failingReasons[reason] || (phase == "Running" && !ready):
		return model.InfraCritical
	case restartsInRange > 0 || phase == "Pending":
		return model.InfraWarning
	}
	return model.InfraOK
}

// WorkloadStatus: critical available = 0 with desired > 0, warning available < desired.
func WorkloadStatus(desired, available float64, last, now time.Time) model.InfraStatus {
	switch {
	case isStale(now, last):
		return model.InfraStale
	case desired > 0 && available <= 0:
		return model.InfraCritical
	case available < desired:
		return model.InfraWarning
	}
	return model.InfraOK
}

// ContainerStatus: critical memory > 0.95 of its limit; warning memory or cpu > 0.85 of the limit, restarts in the
// range or throttled > 0.25.
func ContainerStatus(c model.Container, restartsInRange float64, last, now time.Time) model.InfraStatus {
	mem, cpu := div(c.MemoryBytes, c.MemoryLimit), div(c.CPU, c.CPULimit)
	switch {
	case isStale(now, last):
		return model.InfraStale
	case mem > 0.95:
		return model.InfraCritical
	case mem > 0.85 || cpu > 0.85 || restartsInRange > 0 || c.Throttled > 0.25:
		return model.InfraWarning
	}
	return model.InfraOK
}

// ---------------------------------------------------------------- snapshot readers

type hostRow struct {
	h    model.Host
	last time.Time
}

var hostCols = []string{"cpu", "cpu_cores", "memory", "memory_bytes", "disk", "net_in", "net_out", "load1", "uptime_s"}

func (d *DB) hostRows(ctx context.Context, q Q, f InfraFilter) ([]hostRow, error) {
	where, args := andEq("", nil, "cluster", f.Cluster)
	where, args = andEq(where, args, "host", f.Host)
	rows, err := d.latest(ctx, gaugeQuery{table: "hosts_1m", keys: []string{"host"}, cols: hostCols,
		strs: []string{"host_id", "os", "arch", "cluster", "node"}, where: where, args: args}, snapFrom(q), snapTo(q))
	if err != nil {
		return nil, err
	}
	out := make([]hostRow, 0, len(rows))
	for _, r := range rows {
		h := model.Host{ID: r.keys[0], HostID: r.s[0], OS: r.s[1], Arch: r.s[2], Cluster: r.s[3], Node: r.s[4],
			CPU: r.v[0], CPUCores: r.v[1], Memory: r.v[2], MemoryBytes: r.v[3], Disk: r.v[4], NetIn: r.v[5], NetOut: r.v[6],
			Load1: r.v[7], UptimeS: r.v[8], LastSeen: r.last.UnixMilli()}
		out = append(out, hostRow{h: h, last: r.last})
	}
	return out, nil
}

type containerRow struct {
	c        model.Container
	workload string
	kind     string
	last     time.Time
}

var containerCols = []string{"cpu", "cpu_limit", "memory_bytes", "memory_limit", "restarts", "net_in", "net_out", "throttled"}

// containerRows returns the containers still running (seen within goneAfter of their host's newest row).
func (d *DB) containerRows(ctx context.Context, q Q, f InfraFilter) ([]containerRow, error) {
	where, args := andEq("", nil, "cluster", f.Cluster)
	where, args = andEq(where, args, "host", f.Host)
	where, args = andEq(where, args, "namespace", f.Namespace)
	where, args = andEq(where, args, "service", f.Service)
	rows, err := d.latest(ctx, gaugeQuery{table: "containers_1m", keys: []string{"container_id"}, cols: containerCols,
		strs:  []string{"container", "image", "host", "cluster", "namespace", "pod", "pod_uid", "service", "workload_kind", "workload"},
		where: where, args: args}, snapFrom(q), snapTo(q))
	if err != nil {
		return nil, err
	}
	inc, err := d.increase(ctx, "containers_1m", "container_id", "restarts", where, args, q.W.From, q.W.To)
	if err != nil {
		return nil, err
	}
	newest := map[string]time.Time{}
	for _, r := range rows {
		if r.last.After(newest[r.s[2]]) {
			newest[r.s[2]] = r.last
		}
	}
	out := make([]containerRow, 0, len(rows))
	for _, r := range rows {
		if newest[r.s[2]].Sub(r.last) > goneAfter {
			continue
		}
		c := model.Container{ID: r.keys[0], Name: r.s[0], Image: r.s[1], Host: r.s[2], Cluster: r.s[3], Namespace: r.s[4], Pod: r.s[5],
			PodUID: r.s[6], Service: r.s[7], CPU: r.v[0], CPULimit: r.v[1], MemoryBytes: r.v[2], MemoryLimit: r.v[3], Restarts: r.v[4],
			NetIn: r.v[5], NetOut: r.v[6], Throttled: r.v[7]}
		c.Status = ContainerStatus(c, inc[c.ID], r.last, q.Now)
		out = append(out, containerRow{c: c, kind: r.s[8], workload: r.s[9], last: r.last})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].c.Host != out[j].c.Host {
			return out[i].c.Host < out[j].c.Host
		}
		return out[i].c.Name+out[i].c.ID < out[j].c.Name+out[j].c.ID
	})
	return out, nil
}

type nodeRow struct {
	cluster, node                   string
	ready, allocCPU, allocMem, pods float64
	readyKnown                      bool
	last                            time.Time
}

func (d *DB) nodeRows(ctx context.Context, q Q, cluster string) ([]nodeRow, error) {
	where, args := andEq("", nil, "cluster", cluster)
	rows, err := d.latest(ctx, gaugeQuery{table: "k8s_nodes_1m", keys: []string{"cluster", "node"},
		cols: []string{"ready", "allocatable_cpu", "allocatable_memory", "pods"}, where: where, args: args}, snapFrom(q), snapTo(q))
	if err != nil {
		return nil, err
	}
	newest := map[string]time.Time{}
	for _, r := range rows {
		if r.last.After(newest[r.keys[0]]) {
			newest[r.keys[0]] = r.last
		}
	}
	var out []nodeRow
	for _, r := range rows {
		if newest[r.keys[0]].Sub(r.last) > goneAfter {
			continue
		}
		out = append(out, nodeRow{cluster: r.keys[0], node: r.keys[1], ready: r.v[0], readyKnown: r.ok[0], allocCPU: r.v[1], allocMem: r.v[2], pods: r.v[3], last: r.last})
	}
	return out, nil
}

type podRow struct {
	p    model.Pod
	last time.Time
}

// podRows returns the current pods (seen within goneAfter of their cluster's newest pod row); uid set = that pod
// whatever its age.
func (d *DB) podRows(ctx context.Context, q Q, f InfraFilter, uid string) ([]podRow, error) {
	where, args := andEq("", nil, "cluster", f.Cluster)
	where, args = andEq(where, args, "namespace", f.Namespace)
	where, args = andEq(where, args, "pod_uid", uid)
	rows, err := d.latest(ctx, gaugeQuery{table: "k8s_pods_1m", keys: []string{"cluster", "pod_uid"},
		cols: []string{"ready", "restarts", "cpu_request", "cpu_limit", "memory_request", "memory_limit"},
		strs: []string{"namespace", "pod", "node", "workload_kind", "workload", "service", "phase", "reason"}, where: where, args: args},
		snapFrom(q), snapTo(q))
	if err != nil {
		return nil, err
	}
	inc, err := d.increase(ctx, "k8s_pods_1m", "pod_uid", "restarts", where, args, q.W.From, q.W.To)
	if err != nil {
		return nil, err
	}
	newest := map[string]time.Time{}
	for _, r := range rows {
		if r.last.After(newest[r.keys[0]]) {
			newest[r.keys[0]] = r.last
		}
	}
	var out []podRow
	for _, r := range rows {
		if uid == "" && newest[r.keys[0]].Sub(r.last) > goneAfter {
			continue
		}
		p := model.Pod{Cluster: r.keys[0], UID: r.keys[1], Namespace: r.s[0], Name: r.s[1], Node: r.s[2], WorkloadKind: r.s[3], Workload: r.s[4],
			Service: r.s[5], Phase: r.s[6], Reason: r.s[7], Ready: r.v[0] >= 1, Restarts: r.v[1], RestartsInRange: inc[r.keys[1]],
			CPURequest: r.v[2], CPULimit: r.v[3], MemoryRequest: r.v[4], MemoryLimit: r.v[5], LastSeen: r.last.UnixMilli(),
			Containers: []model.PodContainer{}}
		if (f.Workload != "" && p.Workload != f.Workload) || (f.Node != "" && p.Node != f.Node) || (f.Service != "" && p.Service != f.Service) {
			continue
		}
		p.Status = PodStatus(p.Phase, p.Reason, p.Ready, p.RestartsInRange, r.last, q.Now)
		out = append(out, podRow{p: p, last: r.last})
	}
	return out, nil
}

type workloadRow struct {
	w    model.K8sWorkload
	last time.Time
}

func (d *DB) workloadRows(ctx context.Context, q Q, f InfraFilter) ([]workloadRow, error) {
	where, args := andEq("", nil, "cluster", f.Cluster)
	where, args = andEq(where, args, "namespace", f.Namespace)
	rows, err := d.latest(ctx, gaugeQuery{table: "k8s_workloads_1m", keys: []string{"cluster", "namespace", "kind", "name"},
		cols: []string{"desired", "ready", "available", "updated"}, where: where, args: args}, snapFrom(q), snapTo(q))
	if err != nil {
		return nil, err
	}
	newest := map[string]time.Time{}
	for _, r := range rows {
		if r.last.After(newest[r.keys[0]]) {
			newest[r.keys[0]] = r.last
		}
	}
	var out []workloadRow
	for _, r := range rows {
		if newest[r.keys[0]].Sub(r.last) > goneAfter {
			continue
		}
		w := model.K8sWorkload{Cluster: r.keys[0], Namespace: r.keys[1], Kind: r.keys[2], Name: r.keys[3],
			Desired: r.v[0], Ready: r.v[1], Available: r.v[2], Updated: r.v[3]}
		w.Status = WorkloadStatus(w.Desired, w.Available, r.last, q.Now)
		// A running Job counts succeeded pods as available, so desired > available is normal while it runs.
		if w.Kind == "Job" && w.Status != model.InfraStale && w.Ready > 0 {
			w.Status = model.InfraOK
		}
		out = append(out, workloadRow{w: w, last: r.last})
	}
	return out, nil
}

// attachContainers fills pod cpu / memory / containers from the container rows.
func attachContainers(pods []podRow, ctrs []containerRow) {
	by := map[string][]model.Container{}
	for _, c := range ctrs {
		if c.c.PodUID != "" {
			by[c.c.PodUID] = append(by[c.c.PodUID], c.c)
		}
	}
	for i := range pods {
		p := &pods[i].p
		for _, c := range by[p.UID] {
			p.CPU += c.CPU
			p.MemoryBytes += c.MemoryBytes
			p.Containers = append(p.Containers, model.PodContainer{Name: c.Name, Image: c.Image, CPU: c.CPU, MemoryBytes: c.MemoryBytes, Restarts: c.Restarts})
		}
		sort.Slice(p.Containers, func(a, b int) bool { return p.Containers[a].Name < p.Containers[b].Name })
	}
}

// ---------------------------------------------------------------- hosts

// ListHosts is GET /infra/hosts.
func (d *DB) ListHosts(ctx context.Context, q Q, f InfraFilter) ([]model.Host, error) {
	hosts, err := d.hostRows(ctx, q, f)
	if err != nil {
		return nil, err
	}
	ctrs, err := d.containerRows(ctx, q, InfraFilter{Cluster: f.Cluster})
	if err != nil {
		return nil, err
	}
	nodes, err := d.nodeRows(ctx, q, f.Cluster)
	if err != nil {
		return nil, err
	}
	where, args := andEq("", nil, "cluster", f.Cluster)
	spark, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "hosts_1m", entity: []string{"host"}, group: "host", cols: []string{"cpu", "memory"},
		where: where, args: args})
	if err != nil {
		return nil, err
	}
	return finishHosts(q, f, hosts, ctrs, nodes, spark), nil
}

func finishHosts(q Q, f InfraFilter, hosts []hostRow, ctrs []containerRow, nodes []nodeRow, spark map[string][][]float64) []model.Host {
	count := map[string]int{}
	for _, c := range ctrs {
		count[c.c.Host]++
	}
	notReady := map[string]bool{}
	for _, n := range nodes {
		if n.readyKnown && n.ready < 1 {
			notReady[n.cluster+"\x00"+n.node] = true
		}
	}
	needle := strings.ToLower(strings.TrimSpace(f.Q))
	out := []model.Host{}
	for _, r := range hosts {
		h := r.h
		if needle != "" && !strings.Contains(strings.ToLower(h.ID+" "+h.HostID+" "+h.Node+" "+h.Cluster), needle) {
			continue
		}
		h.Containers = count[h.ID]
		h.Status = HostStatus(h.CPU, h.Memory, h.Disk, h.Node != "" && notReady[h.Cluster+"\x00"+h.Node], r.last, q.Now)
		h.Spark = model.HostSpark{CPU: make([]float64, q.W.Points), Memory: make([]float64, q.W.Points)}
		if s := spark[h.ID]; s != nil {
			h.Spark.CPU, h.Spark.Memory = s[0], s[1]
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if statusRank[out[i].Status] != statusRank[out[j].Status] {
			return statusRank[out[i].Status] > statusRank[out[j].Status]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// GetHost is GET /infra/hosts/{host}.
func (d *DB) GetHost(ctx context.Context, q Q, host string) (*model.HostDetail, error) {
	hosts, err := d.hostRows(ctx, q, InfraFilter{Host: host})
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, ErrNotFound
	}
	ctrs, err := d.containerRows(ctx, q, InfraFilter{Host: host})
	if err != nil {
		return nil, err
	}
	nodes, err := d.nodeRows(ctx, q, hosts[0].h.Cluster)
	if err != nil {
		return nil, err
	}
	ser, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "hosts_1m", entity: []string{"host"}, cols: []string{"cpu", "memory", "disk", "net_in", "net_out"},
		where: " AND host = ?", args: []any{host}})
	if err != nil {
		return nil, err
	}
	s := ser[""]
	if s == nil {
		s = make([][]float64, 5)
	}
	hl := finishHosts(q, InfraFilter{}, hosts, ctrs, nodes, map[string][][]float64{host: {s[0], s[1]}})
	out := &model.HostDetail{Host: hl[0], ContainerList: []model.Container{}, Services: []string{}}
	out.Series = model.HostSeries{CPU: set(q.W, []string{"cpu"}, s[0:1]), Memory: set(q.W, []string{"memory"}, s[1:2]),
		Disk: set(q.W, []string{"disk"}, s[2:3]), Network: set(q.W, []string{"in", "out"}, s[3:5])}
	svc := map[string]bool{}
	for _, c := range ctrs {
		out.ContainerList = append(out.ContainerList, c.c)
		if c.c.Service != "" {
			svc[c.c.Service] = true
		}
	}
	if h := out.Host; h.Node != "" {
		pods, err := d.podRows(ctx, q, InfraFilter{Cluster: h.Cluster, Node: h.Node}, "")
		if err != nil {
			return nil, err
		}
		for _, p := range pods {
			if p.p.Service != "" {
				svc[p.p.Service] = true
			}
		}
	}
	for k := range svc {
		out.Services = append(out.Services, k)
	}
	sort.Strings(out.Services)
	out.Processes, err = d.hostProcesses(ctx, q, host)
	return out, err
}

// hostProcesses returns the processes of the host's newest minute, by cpu.
func (d *DB) hostProcesses(ctx context.Context, q Q, host string) ([]model.HostProcess, error) {
	rows, err := d.ch.Query(ctx, `SELECT pid, toString(exe), any(command), any(container_id), toFloat64(max(cpu)), toFloat64(max(memory_bytes)),
	    toFloat64(max(threads)), toFloat64(max(fds))
	  FROM processes_1m WHERE {{tenant}} AND host = ? AND minute = (
	    SELECT max(minute) FROM processes_1m WHERE {{tenant}} AND host = ? AND minute >= toDateTime(?) AND minute < toDateTime(?))
	  GROUP BY pid, exe ORDER BY 5 DESC LIMIT 100`, host, host, secs(snapFrom(q)), secs(snapTo(q)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.HostProcess{}
	for rows.Next() {
		var p model.HostProcess
		if err := rows.Scan(&p.PID, &p.Exe, &p.Command, &p.ContainerID, &p.CPU, &p.MemoryBytes, &p.Threads, &p.FDs); err != nil {
			return nil, err
		}
		p.CPU, p.MemoryBytes, p.Threads, p.FDs = fin(p.CPU), fin(p.MemoryBytes), fin(p.Threads), fin(p.FDs)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListContainers is GET /infra/containers.
func (d *DB) ListContainers(ctx context.Context, q Q, f InfraFilter) ([]model.Container, error) {
	rows, err := d.containerRows(ctx, q, f)
	if err != nil {
		return nil, err
	}
	out := make([]model.Container, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.c)
	}
	return out, nil
}

// ---------------------------------------------------------------- Kubernetes

type k8sState struct {
	nodes     []nodeRow
	pods      []podRow
	workloads []workloadRow
	ctrs      []containerRow
	hosts     []hostRow
	events    map[string]int64
}

func (d *DB) k8s(ctx context.Context, q Q, cluster string) (*k8sState, error) {
	st := &k8sState{}
	var err error
	if st.nodes, err = d.nodeRows(ctx, q, cluster); err != nil {
		return nil, err
	}
	if st.pods, err = d.podRows(ctx, q, InfraFilter{Cluster: cluster}, ""); err != nil {
		return nil, err
	}
	if st.workloads, err = d.workloadRows(ctx, q, InfraFilter{Cluster: cluster}); err != nil {
		return nil, err
	}
	if st.ctrs, err = d.containerRows(ctx, q, InfraFilter{Cluster: cluster}); err != nil {
		return nil, err
	}
	if st.hosts, err = d.hostRows(ctx, q, InfraFilter{Cluster: cluster}); err != nil {
		return nil, err
	}
	attachContainers(st.pods, st.ctrs)
	st.events, err = d.warningEvents(ctx, q, cluster)
	return st, err
}

func (d *DB) warningEvents(ctx context.Context, q Q, cluster string) (map[string]int64, error) {
	where, args := andEq("", nil, "attrs['k8s.cluster.name']", cluster)
	rows, err := d.ch.Query(ctx, `SELECT attrs['k8s.cluster.name'], toInt64(count()) FROM logs WHERE {{tenant}} AND service = 'k8s'
	  AND ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6)`+where+` GROUP BY 1`, append([]any{secs(q.W.From), secs(q.W.To)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var n int64
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		out[c] = n
	}
	return out, rows.Err()
}

// nodeView builds the node rows of one cluster (cpu / memory used from the node's host, else its containers).
func (st *k8sState) nodeView(q Q, cluster string) []model.K8sNode {
	hostOf := map[string]hostRow{}
	for _, h := range st.hosts {
		if h.h.Cluster == cluster && h.h.Node != "" {
			hostOf[h.h.Node] = h
		}
	}
	ctrCPU, ctrMem := map[string]float64{}, map[string]float64{}
	for _, c := range st.ctrs {
		if c.c.Cluster == cluster {
			ctrCPU[c.c.Host] += c.c.CPU
			ctrMem[c.c.Host] += c.c.MemoryBytes
		}
	}
	pods := map[string]int{}
	for _, p := range st.pods {
		if p.p.Cluster == cluster && p.p.Phase != "Succeeded" {
			pods[p.p.Node]++
		}
	}
	out := []model.K8sNode{}
	for _, n := range st.nodes {
		if n.cluster != cluster {
			continue
		}
		v := model.K8sNode{Name: n.node, Ready: !n.readyKnown || n.ready >= 1, CPUAllocatable: n.allocCPU, MemoryAllocatable: n.allocMem,
			Pods: pods[n.node], Host: n.node}
		var cpu, mem, disk float64
		if h, ok := hostOf[n.node]; ok {
			v.Host, v.CPU, v.MemoryBytes = h.h.ID, h.h.CPU*h.h.CPUCores, h.h.MemoryBytes
			cpu, mem, disk = h.h.CPU, h.h.Memory, h.h.Disk
		} else {
			v.CPU, v.MemoryBytes = ctrCPU[n.node], ctrMem[n.node]
			cpu, mem = div(v.CPU, v.CPUAllocatable), div(v.MemoryBytes, v.MemoryAllocatable)
		}
		if v.Pods == 0 {
			v.Pods = int(n.pods)
		}
		v.Status = HostStatus(cpu, mem, disk, !v.Ready, n.last, q.Now)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *k8sState) clusterNames() []string {
	seen := map[string]bool{}
	for _, n := range st.nodes {
		seen[n.cluster] = true
	}
	for _, p := range st.pods {
		seen[p.p.Cluster] = true
	}
	for _, w := range st.workloads {
		seen[w.w.Cluster] = true
	}
	var out []string
	for c := range seen {
		if c != "" {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func (st *k8sState) cluster(q Q, name string) model.K8sCluster {
	c := model.K8sCluster{Name: name, Status: model.InfraOK, WarningEvents: st.events[name]}
	var last time.Time
	for _, n := range st.nodeView(q, name) {
		c.Nodes++
		if n.Ready {
			c.NodesReady++
		}
		c.CPUAllocatable += n.CPUAllocatable
		c.MemoryAllocatable += n.MemoryAllocatable
		if n.Status != model.InfraStale {
			c.Status = worse(c.Status, n.Status)
		}
	}
	for _, n := range st.nodes {
		if n.cluster == name && n.last.After(last) {
			last = n.last
		}
	}
	ns := map[string]bool{}
	for _, p := range st.pods {
		if p.p.Cluster != name {
			continue
		}
		c.Pods++
		ns[p.p.Namespace] = true
		if p.p.Phase == "Running" {
			c.PodsRunning++
		}
		if p.p.Status == model.InfraCritical {
			c.PodsFailing++
		}
		if p.p.Status != model.InfraStale {
			c.Status = worse(c.Status, p.p.Status)
		}
		if p.last.After(last) {
			last = p.last
		}
	}
	for _, w := range st.workloads {
		if w.w.Cluster == name {
			c.Workloads++
			ns[w.w.Namespace] = true
		}
	}
	c.Namespaces = len(ns)
	for _, ct := range st.ctrs {
		if ct.c.Cluster == name {
			c.CPUUsed += ct.c.CPU
			c.MemoryUsed += ct.c.MemoryBytes
		}
	}
	if c.CPUUsed == 0 && c.MemoryUsed == 0 { // no container data: the nodes' hosts
		for _, h := range st.hosts {
			if h.h.Cluster == name {
				c.CPUUsed += h.h.CPU * h.h.CPUCores
				c.MemoryUsed += h.h.MemoryBytes
			}
		}
	}
	c.LastSeen = last.UnixMilli()
	if last.IsZero() || isStale(q.Now, last) {
		c.Status = model.InfraStale
		if last.IsZero() {
			c.LastSeen = 0
		}
	}
	return c
}

// ListK8sClusters is GET /infra/k8s/clusters.
func (d *DB) ListK8sClusters(ctx context.Context, q Q) ([]model.K8sCluster, error) {
	st, err := d.k8s(ctx, q, "")
	if err != nil {
		return nil, err
	}
	out := []model.K8sCluster{}
	for _, n := range st.clusterNames() {
		out = append(out, st.cluster(q, n))
	}
	return out, nil
}

// GetK8sCluster is GET /infra/k8s/clusters/{cluster}.
func (d *DB) GetK8sCluster(ctx context.Context, q Q, name string) (*model.K8sClusterDetail, error) {
	if name == "" {
		return nil, ErrNotFound
	}
	st, err := d.k8s(ctx, q, name)
	if err != nil {
		return nil, err
	}
	if len(st.clusterNames()) == 0 {
		return nil, ErrNotFound
	}
	out := &model.K8sClusterDetail{K8sCluster: st.cluster(q, name), NodeList: st.nodeView(q, name), NamespaceList: []model.K8sNamespace{}}
	nsBy := map[string]*model.K8sNamespace{}
	ns := func(n string) *model.K8sNamespace {
		if x, ok := nsBy[n]; ok {
			return x
		}
		x := &model.K8sNamespace{Name: n}
		nsBy[n] = x
		return x
	}
	for _, p := range st.pods {
		x := ns(p.p.Namespace)
		x.Pods++
		if p.p.Status == model.InfraCritical {
			x.Failing++
		}
	}
	for _, w := range st.workloads {
		ns(w.w.Namespace).Workloads++
	}
	for _, x := range nsBy {
		out.NamespaceList = append(out.NamespaceList, *x)
	}
	sort.Slice(out.NamespaceList, func(i, j int) bool { return out.NamespaceList[i].Name < out.NamespaceList[j].Name })

	cw := " AND cluster = ?"
	used, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "containers_1m", entity: []string{"container_id"}, cols: []string{"cpu", "memory_bytes"},
		where: cw, args: []any{name}, sumOver: true})
	if err != nil {
		return nil, err
	}
	req, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "k8s_pods_1m", entity: []string{"pod_uid"}, cols: []string{"cpu_request", "memory_request"},
		where: cw, args: []any{name}, sumOver: true})
	if err != nil {
		return nil, err
	}
	alloc, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "k8s_nodes_1m", entity: []string{"node"}, cols: []string{"allocatable_cpu", "allocatable_memory"},
		where: cw, args: []any{name}, sumOver: true})
	if err != nil {
		return nil, err
	}
	col := func(m map[string][][]float64, i int) []float64 {
		if s := m[""]; s != nil {
			return s[i]
		}
		return nil
	}
	ids := []string{"used", "requested", "allocatable"}
	out.Series = model.ResourceSeries{CPU: set(q.W, ids, [][]float64{col(used, 0), col(req, 0), col(alloc, 0)}),
		Memory: set(q.W, ids, [][]float64{col(used, 1), col(req, 1), col(alloc, 1)})}
	return out, nil
}

// ListWorkloads is GET /infra/k8s/workloads.
func (d *DB) ListWorkloads(ctx context.Context, q Q, f InfraFilter) ([]model.K8sWorkload, error) {
	ws, err := d.workloadRows(ctx, q, f)
	if err != nil {
		return nil, err
	}
	pods, err := d.podRows(ctx, q, InfraFilter{Cluster: f.Cluster, Namespace: f.Namespace}, "")
	if err != nil {
		return nil, err
	}
	ctrs, err := d.containerRows(ctx, q, InfraFilter{Cluster: f.Cluster, Namespace: f.Namespace})
	if err != nil {
		return nil, err
	}
	attachContainers(pods, ctrs)
	return workloadsView(ws, pods), nil
}

func workloadsView(ws []workloadRow, pods []podRow) []model.K8sWorkload {
	type agg struct {
		restarts, cpu, mem float64
		service            string
	}
	by := map[string]*agg{}
	for _, p := range pods {
		k := p.p.Cluster + "\x00" + p.p.Namespace + "\x00" + p.p.WorkloadKind + "\x00" + p.p.Workload
		a, ok := by[k]
		if !ok {
			a = &agg{}
			by[k] = a
		}
		a.restarts += p.p.RestartsInRange
		a.cpu += p.p.CPU
		a.mem += p.p.MemoryBytes
		if a.service == "" {
			a.service = p.p.Service
		}
	}
	out := []model.K8sWorkload{}
	for _, r := range ws {
		w := r.w
		if a := by[w.Cluster+"\x00"+w.Namespace+"\x00"+w.Kind+"\x00"+w.Name]; a != nil {
			w.Restarts, w.CPU, w.MemoryBytes, w.Service = a.restarts, a.cpu, a.mem, a.service
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Cluster+"\x00"+a.Namespace+"\x00"+a.Kind+"\x00"+a.Name < b.Cluster+"\x00"+b.Namespace+"\x00"+b.Kind+"\x00"+b.Name
	})
	return out
}

// ListPods is GET /infra/k8s/pods.
func (d *DB) ListPods(ctx context.Context, q Q, f InfraFilter) ([]model.Pod, error) {
	pods, err := d.podRows(ctx, q, f, "")
	if err != nil {
		return nil, err
	}
	ctrs, err := d.containerRows(ctx, q, InfraFilter{Cluster: f.Cluster, Namespace: f.Namespace})
	if err != nil {
		return nil, err
	}
	attachContainers(pods, ctrs)
	return podList(pods), nil
}

func podList(pods []podRow) []model.Pod {
	out := make([]model.Pod, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.p)
	}
	sort.Slice(out, func(i, j int) bool {
		if statusRank[out[i].Status] != statusRank[out[j].Status] {
			return statusRank[out[i].Status] > statusRank[out[j].Status]
		}
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out
}

// GetPod is GET /infra/k8s/pods/{uid}.
func (d *DB) GetPod(ctx context.Context, q Q, uid string) (*model.PodDetail, error) {
	pods, err := d.podRows(ctx, q, InfraFilter{}, uid)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, ErrNotFound
	}
	p := pods[0].p
	ctrs, err := d.containerRows(ctx, q, InfraFilter{Cluster: p.Cluster, Namespace: p.Namespace})
	if err != nil {
		return nil, err
	}
	attachContainers(pods, ctrs)
	out := &model.PodDetail{Pod: pods[0].p}
	usage, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "containers_1m", entity: []string{"container_id"}, cols: []string{"cpu", "memory_bytes"},
		where: " AND pod_uid = ?", args: []any{uid}, sumOver: true})
	if err != nil {
		return nil, err
	}
	spec, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "k8s_pods_1m", entity: []string{"pod_uid"},
		cols: []string{"cpu_request", "cpu_limit", "memory_request", "memory_limit", "restarts"}, where: " AND pod_uid = ?", args: []any{uid}, perBucket: "max"})
	if err != nil {
		return nil, err
	}
	lo, err := d.gaugeBuckets(ctx, q.W, seriesQuery{table: "k8s_pods_1m", entity: []string{"pod_uid"}, cols: []string{"restarts", "if(isFinite(restarts), 1, -inf)"},
		where: " AND pod_uid = ?", args: []any{uid}, perBucket: "min"})
	if err != nil {
		return nil, err
	}
	u, s, l := usage[""], spec[""], lo[""]
	if u == nil {
		u = make([][]float64, 2)
	}
	if s == nil {
		s = make([][]float64, 5)
	}
	var lmin, has []float64
	if l != nil {
		lmin, has = l[0], l[1]
	}
	ids := []string{"usage", "request", "limit"}
	out.Series = model.PodSeries{CPU: set(q.W, ids, [][]float64{u[0], s[0], s[1]}), Memory: set(q.W, ids, [][]float64{u[1], s[2], s[3]}),
		Restarts: set(q.W, []string{"restarts"}, [][]float64{RestartIncrease(s[4], lmin, has)})}
	out.Events, err = d.ListK8sEvents(ctx, q, InfraFilter{Cluster: p.Cluster, Namespace: p.Namespace, Kind: "Pod", Name: p.Name, Limit: 50})
	return out, err
}

// RestartIncrease turns per-bucket max / min of a cumulative restart counter into the increase per bucket (vs the
// previous bucket with data; within the bucket for the first one). has[i] > 0 marks buckets with data (nil = any
// bucket with a non-zero value); buckets without data are 0.
func RestartIncrease(hi, lo, has []float64) []float64 {
	out := make([]float64, len(hi))
	prev, seen := 0.0, false
	for i, h := range hi {
		if has != nil {
			if i >= len(has) || has[i] <= 0 {
				continue
			}
		} else if h <= 0 && (i >= len(lo) || lo[i] <= 0) {
			continue
		}
		switch {
		case seen:
			out[i] = math.Max(0, h-prev)
		case i < len(lo):
			out[i] = math.Max(0, h-lo[i])
		}
		prev, seen = h, true
	}
	return out
}

// ListK8sEvents is GET /infra/k8s/events: Warning events (logs, service = 'k8s'), newest first.
func (d *DB) ListK8sEvents(ctx context.Context, q Q, f InfraFilter) ([]model.K8sEvent, error) {
	where, args := andEq("", nil, "attrs['k8s.cluster.name']", f.Cluster)
	where, args = andEq(where, args, "attrs['k8s.namespace.name']", f.Namespace)
	where, args = andEq(where, args, "attrs['k8s.object.kind']", f.Kind)
	where, args = andEq(where, args, "attrs['k8s.object.name']", f.Name)
	rows, err := d.ch.Query(ctx, `SELECT toInt64(toUnixTimestamp64Milli(ts)), attrs['k8s.event.type'], attrs['k8s.event.reason'], body,
	    attrs['k8s.object.kind'], attrs['k8s.object.name'], attrs['k8s.namespace.name'], attrs['k8s.cluster.name'], toInt64OrZero(attrs['k8s.event.count'])
	  FROM logs WHERE {{tenant}} AND service = 'k8s' AND ts >= toDateTime64(?, 6) AND ts < toDateTime64(?, 6)`+where+`
	  ORDER BY ts DESC LIMIT ?`, append(append([]any{secs(q.W.From), secs(q.W.To)}, args...), clampLimit(f.Limit, 200, 1000))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.K8sEvent{}
	for rows.Next() {
		var e model.K8sEvent
		if err := rows.Scan(&e.TS, &e.Type, &e.Reason, &e.Message, &e.ObjectKind, &e.ObjectName, &e.Namespace, &e.Cluster, &e.Count); err != nil {
			return nil, err
		}
		if e.Type == "" {
			e.Type = "Warning"
		}
		if e.Count == 0 {
			e.Count = 1
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ServiceInfra is GET /services/{svc}/infra: the service's pods, containers, their hosts and serverless functions.
func (d *DB) ServiceInfra(ctx context.Context, q Q, svc string) (*model.ServiceInfra, error) {
	out := &model.ServiceInfra{Pods: []model.Pod{}, Hosts: []model.ServiceHost{}, Containers: []model.Container{}, Functions: []model.ServiceFunction{}}
	pods, err := d.podRows(ctx, q, InfraFilter{Service: svc}, "")
	if err != nil {
		return nil, err
	}
	ctrs, err := d.containerRows(ctx, q, InfraFilter{})
	if err != nil {
		return nil, err
	}
	attachContainers(pods, ctrs)
	out.Pods = podList(pods)
	podUID := map[string]bool{}
	nodes := map[string]bool{}
	for _, p := range pods {
		podUID[p.p.UID] = true
		nodes[p.p.Cluster+"\x00"+p.p.Node] = true
	}
	hostNames := map[string]bool{}
	for _, c := range ctrs {
		if c.c.Service == svc || podUID[c.c.PodUID] {
			out.Containers = append(out.Containers, c.c)
			hostNames[c.c.Host] = true
		}
	}
	if len(hostNames) > 0 || len(nodes) > 0 {
		hosts, err := d.hostRows(ctx, q, InfraFilter{})
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			if hostNames[h.h.ID] || (h.h.Node != "" && nodes[h.h.Cluster+"\x00"+h.h.Node]) {
				out.Hosts = append(out.Hosts, model.ServiceHost{ID: h.h.ID, CPU: h.h.CPU, Memory: h.h.Memory})
			}
		}
	}
	out.Functions, err = d.serviceFunctions(ctx, svc)
	return out, err
}
