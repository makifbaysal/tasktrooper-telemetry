package infra

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
)

// Node is the subset of v1.Node we read.
type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		ProviderID string `json:"providerID"`
	} `json:"spec"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []Condition       `json:"conditions"`
	} `json:"status"`
}

type deployment struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas     int `json:"readyReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
		UpdatedReplicas   int `json:"updatedReplicas"`
	} `json:"status"`
}

type statefulSet struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas     int  `json:"readyReplicas"`
		AvailableReplicas *int `json:"availableReplicas"`
		UpdatedReplicas   int  `json:"updatedReplicas"`
	} `json:"status"`
}

type daemonSet struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		DesiredNumberScheduled int `json:"desiredNumberScheduled"`
		NumberReady            int `json:"numberReady"`
		NumberAvailable        int `json:"numberAvailable"`
		UpdatedNumberScheduled int `json:"updatedNumberScheduled"`
	} `json:"status"`
}

type replicaSet struct {
	Metadata ObjectMeta `json:"metadata"`
}

type job struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Completions *int `json:"completions"`
	} `json:"spec"`
	Status struct {
		Active         int         `json:"active"`
		Ready          *int        `json:"ready"`
		Succeeded      int         `json:"succeeded"`
		Failed         int         `json:"failed"`
		StartTime      time.Time   `json:"startTime"`
		CompletionTime time.Time   `json:"completionTime"`
		Conditions     []Condition `json:"conditions"`
	} `json:"status"`
}

func (j *job) condition(t string) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == t && c.Status == "True" {
			return true
		}
	}
	return false
}

type cronJob struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Suspend *bool `json:"suspend"`
	} `json:"spec"`
	Status struct {
		Active []struct {
			Name string `json:"name"`
		} `json:"active"`
	} `json:"status"`
}

// Event is the subset of core/v1 Event we read.
type Event struct {
	Metadata       ObjectMeta `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"involvedObject"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Type           string    `json:"type"`
	Count          int       `json:"count"`
	FirstTimestamp time.Time `json:"firstTimestamp"`
	LastTimestamp  time.Time `json:"lastTimestamp"`
	EventTime      time.Time `json:"eventTime"`
	Series         *struct {
		Count            int       `json:"count"`
		LastObservedTime time.Time `json:"lastObservedTime"`
	} `json:"series"`
}

// Time is the event's latest occurrence.
func (e *Event) Time() time.Time {
	for _, t := range []time.Time{seriesTime(e), e.LastTimestamp, e.EventTime, e.FirstTimestamp, e.Metadata.CreationTimestamp} {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func seriesTime(e *Event) time.Time {
	if e.Series != nil {
		return e.Series.LastObservedTime
	}
	return time.Time{}
}

// Occurrences is the event's count (series.count for events.k8s.io-style events).
func (e *Event) Occurrences() int {
	if e.Series != nil && e.Series.Count > 0 {
		return e.Series.Count
	}
	if e.Count > 0 {
		return e.Count
	}
	return 1
}

// LogSink receives Warning events (the OTLP log emitter).
type LogSink interface {
	Warn(ctx context.Context, r emit.LogRecord)
}

// ClusterScraper reports trooper.k8s.node|pod|workload.* from an in-memory copy of the cluster
// kept current by list + watch (one Reflector per kind, contracts §22) and ships Warning events
// (watched too) as logs. Snapshot metrics are built from that state on every Scrape (30 s).
// Read-only: list and watch GETs only.
type ClusterScraper struct {
	C      *KubeClient
	Events LogSink
	// EventLookback: on the first list only events newer than this are sent (no replay of the
	// cluster's whole event history after a restart).
	EventLookback time.Duration
	// MinBackoff / MaxBackoff / WatchTimeout tune the reflectors (tests); zero = defaults.
	MinBackoff, MaxBackoff, WatchTimeout time.Duration
	now                                  func() time.Time

	startOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}

	nodes *Reflector[Node]
	pods  *Reflector[Pod]
	rss   *Reflector[replicaSet]
	deps  *Reflector[deployment]
	sts   *Reflector[statefulSet]
	dss   *Reflector[daemonSet]
	jobs  *Reflector[job]
	cjs   *Reflector[cronJob]
	evs   *Reflector[Event]

	evMu       sync.Mutex
	sent       map[string]int // event uid → last count sent
	lastActive time.Time      // last successful event list / watch event (relist cutoff)
}

// NewClusterScraper builds the scraper; the watches start on the first Scrape.
func NewClusterScraper(c *KubeClient, events LogSink) *ClusterScraper {
	return &ClusterScraper{C: c, Events: events, EventLookback: 5 * time.Minute, sent: map[string]int{}, now: time.Now}
}

func newReflector[T any](s *ClusterScraper, path string, meta func(*T) *ObjectMeta) *Reflector[T] {
	return &Reflector[T]{C: s.C, Path: path, Meta: meta, MinBackoff: s.MinBackoff, MaxBackoff: s.MaxBackoff, WatchTimeout: s.WatchTimeout}
}

func (s *ClusterScraper) start() {
	s.nodes = newReflector(s, "/api/v1/nodes", func(o *Node) *ObjectMeta { return &o.Metadata })
	s.pods = newReflector(s, "/api/v1/pods", func(o *Pod) *ObjectMeta { return &o.Metadata })
	s.rss = newReflector(s, "/apis/apps/v1/replicasets", func(o *replicaSet) *ObjectMeta { return &o.Metadata })
	s.deps = newReflector(s, "/apis/apps/v1/deployments", func(o *deployment) *ObjectMeta { return &o.Metadata })
	s.sts = newReflector(s, "/apis/apps/v1/statefulsets", func(o *statefulSet) *ObjectMeta { return &o.Metadata })
	s.dss = newReflector(s, "/apis/apps/v1/daemonsets", func(o *daemonSet) *ObjectMeta { return &o.Metadata })
	s.jobs = newReflector(s, "/apis/batch/v1/jobs", func(o *job) *ObjectMeta { return &o.Metadata })
	s.cjs = newReflector(s, "/apis/batch/v1/cronjobs", func(o *cronJob) *ObjectMeta { return &o.Metadata })
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	var wg sync.WaitGroup
	run := func(f func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(ctx)
		}()
	}
	run(s.nodes.Run)
	run(s.pods.Run)
	run(s.rss.Run)
	run(s.deps.Run)
	run(s.sts.Run)
	run(s.dss.Run)
	run(s.jobs.Run)
	run(s.cjs.Run)
	if s.Events != nil {
		s.evs = newReflector(s, "/api/v1/events", func(o *Event) *ObjectMeta { return &o.Metadata })
		s.evs.Extra = url.Values{"fieldSelector": {"type=Warning"}}
		s.evs.NoStore = true
		s.evs.OnList = s.onEventList
		s.evs.OnEvent = s.onEvent
		run(s.evs.Run)
	}
	go func() {
		wg.Wait()
		close(s.done)
	}()
}

// Close stops the watches.
func (s *ClusterScraper) Close() error {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	return nil
}

// syncState is implemented by every *Reflector.
type syncState interface {
	Ready() <-chan struct{}
	Synced() (bool, error)
}

// Scrape implements scrape.Scraper: starts the watches on the first call, waits (bounded by ctx)
// until every kind has finished its first list, then reports from memory. Nodes and pods are
// required; a kind that cannot be listed (RBAC, API group missing) only drops that kind.
func (s *ClusterScraper) Scrape(ctx context.Context) (emit.Result, error) {
	var r emit.Result
	s.startOnce.Do(s.start)
	wait := []syncState{s.nodes, s.pods, s.rss, s.deps, s.sts, s.dss, s.jobs, s.cjs}
	if s.evs != nil {
		wait = append(wait, s.evs)
	}
	for _, x := range wait {
		select {
		case <-x.Ready():
		case <-ctx.Done():
		}
	}
	for _, req := range []struct {
		name string
		x    syncState
	}{{"nodes", s.nodes}, {"pods", s.pods}} {
		if ok, err := req.x.Synced(); !ok {
			if err == nil {
				err = errors.New("initial list not finished")
			}
			return r, fmt.Errorf("%s: %w", req.name, err)
		}
	}
	now := s.now()
	nodes, pods := s.nodes.Items(), s.pods.Items()
	rss, deps, sts, dss := s.rss.Items(), s.deps.Items(), s.sts.Items(), s.dss.Items()
	jobs, cjs := s.jobs.Items(), s.cjs.Items()

	owners := Owners{ReplicaSets: map[string]string{}, Jobs: map[string]string{}}
	for _, rs := range rss {
		d := ""
		if ref, ok := controllerOf(rs.Metadata.OwnerReferences); ok && ref.Kind == "Deployment" {
			d = ref.Name
		}
		owners.ReplicaSets[rs.Metadata.Namespace+"/"+rs.Metadata.Name] = d
	}
	for _, j := range jobs {
		c := ""
		if ref, ok := controllerOf(j.Metadata.OwnerReferences); ok && ref.Kind == "CronJob" {
			c = ref.Name
		}
		owners.Jobs[j.Metadata.Namespace+"/"+j.Metadata.Name] = c
	}

	// nodes
	podsOnNode := map[string]int{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != "" && p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed" {
			podsOnNode[p.Spec.NodeName]++
		}
	}
	for _, n := range nodes {
		a := []attribute.KeyValue{attribute.String(emit.AttrK8sNode, n.Metadata.Name)}
		ready := 0.0
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = 1
			}
		}
		r.Gauge(emit.K8sNodeReady, ready, a...)
		if v, ok := ParseQuantity(n.Status.Allocatable["cpu"]); ok {
			r.Gauge(emit.K8sNodeAllocatableCPU, v, a...)
		}
		if v, ok := ParseQuantity(n.Status.Allocatable["memory"]); ok {
			r.Gauge(emit.K8sNodeAllocatableMemory, v, a...)
		}
		r.Gauge(emit.K8sNodePods, float64(podsOnNode[n.Metadata.Name]), a...)
	}

	// pods
	for i := range pods {
		p := &pods[i]
		kind, wl := owners.Workload(p.Metadata)
		a := PodAttrs(p, kind, wl, PodReason(p, now))
		r.Gauge(emit.K8sPodReady, b2f(p.Ready()), a...)
		r.Gauge(emit.K8sPodRestarts, float64(p.Restarts()), a...)
		if v, ok := sumResource(p.Spec.Containers, false, "cpu"); ok {
			r.Gauge(emit.K8sPodCPURequest, v, a...)
		}
		if v, ok := sumResource(p.Spec.Containers, true, "cpu"); ok {
			r.Gauge(emit.K8sPodCPULimit, v, a...)
		}
		if v, ok := sumResource(p.Spec.Containers, false, "memory"); ok {
			r.Gauge(emit.K8sPodMemoryRequest, v, a...)
		}
		if v, ok := sumResource(p.Spec.Containers, true, "memory"); ok {
			r.Gauge(emit.K8sPodMemoryLimit, v, a...)
		}
	}

	// workloads
	wlGauges := func(m ObjectMeta, kind string, desired, ready, available, updated int) {
		a := []attribute.KeyValue{
			attribute.String(emit.AttrK8sNamespace, m.Namespace),
			attribute.String(emit.AttrWorkloadKind, kind),
			attribute.String(emit.AttrWorkloadName, m.Name),
		}
		r.Gauge(emit.K8sWorkloadDesired, float64(desired), a...)
		r.Gauge(emit.K8sWorkloadReady, float64(ready), a...)
		r.Gauge(emit.K8sWorkloadAvailable, float64(available), a...)
		r.Gauge(emit.K8sWorkloadUpdated, float64(updated), a...)
	}
	for _, d := range deps {
		wlGauges(d.Metadata, "Deployment", intOr(d.Spec.Replicas, 1), d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas)
	}
	for _, s := range sts {
		avail := s.Status.ReadyReplicas
		if s.Status.AvailableReplicas != nil {
			avail = *s.Status.AvailableReplicas
		}
		wlGauges(s.Metadata, "StatefulSet", intOr(s.Spec.Replicas, 1), s.Status.ReadyReplicas, avail, s.Status.UpdatedReplicas)
	}
	for _, d := range dss {
		wlGauges(d.Metadata, "DaemonSet", d.Status.DesiredNumberScheduled, d.Status.NumberReady, d.Status.NumberAvailable, d.Status.UpdatedNumberScheduled)
	}
	// Jobs: standalone ones only (CronJob children are summarised by their CronJob), and only
	// while running or finished in the last 24 h (finished jobs without a TTL pile up).
	// desired = completions, ready = ready (else active) pods, available = updated = succeeded.
	lastChild := map[string]*job{} // cronjob → newest finished child
	for i := range jobs {
		j := &jobs[i]
		key := j.Metadata.Namespace + "/" + j.Metadata.Name
		if cj := owners.Jobs[key]; cj != "" {
			if j.condition("Complete") || j.condition("Failed") {
				ck := j.Metadata.Namespace + "/" + cj
				if prev := lastChild[ck]; prev == nil || j.Status.StartTime.After(prev.Status.StartTime) {
					lastChild[ck] = j
				}
			}
			continue
		}
		finished := j.condition("Complete") || j.condition("Failed")
		if finished && !j.Status.CompletionTime.IsZero() && now.Sub(j.Status.CompletionTime) > 24*time.Hour {
			continue
		}
		if finished && j.Status.CompletionTime.IsZero() && now.Sub(j.Status.StartTime) > 24*time.Hour {
			continue
		}
		ready := j.Status.Active
		if j.Status.Ready != nil {
			ready = *j.Status.Ready
		}
		wlGauges(j.Metadata, "Job", intOr(j.Spec.Completions, 1), ready, j.Status.Succeeded, j.Status.Succeeded)
	}
	// CronJobs: desired = 1 (0 when suspended), ready = updated = running jobs,
	// available = 0 when the newest finished run failed, else desired.
	for _, c := range cjs {
		desired := 1
		if c.Spec.Suspend != nil && *c.Spec.Suspend {
			desired = 0
		}
		avail := desired
		if last := lastChild[c.Metadata.Namespace+"/"+c.Metadata.Name]; last != nil && last.condition("Failed") {
			avail = 0
		}
		wlGauges(c.Metadata, "CronJob", desired, len(c.Status.Active), avail, len(c.Status.Active))
	}

	return r, nil
}

// PodAttrs are the attributes of trooper.k8s.pod.*.
func PodAttrs(p *Pod, kind, workload, reason string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(emit.AttrK8sNamespace, p.Metadata.Namespace),
		attribute.String(emit.AttrK8sPod, p.Metadata.Name),
		attribute.String(emit.AttrK8sPodUID, p.Metadata.UID),
		attribute.String(emit.AttrK8sNode, p.Spec.NodeName),
		attribute.String(emit.AttrK8sPodPhase, p.Status.Phase),
		attribute.String(emit.AttrK8sPodReason, reason),
		attribute.String(emit.AttrWorkloadKind, kind),
		attribute.String(emit.AttrWorkloadName, workload),
		attribute.String(emit.AttrService, ServiceOf(p.Metadata.Labels)),
	}
}

// onEventList handles the (re)list of Warning events: unseen uid+count are sent, except on the
// first list (and after a long disconnect) events older than the lookback.
func (s *ClusterScraper) onEventList(ctx context.Context, evs []Event, relist bool) {
	now := s.now()
	s.evMu.Lock()
	cutoff := now.Add(-s.EventLookback)
	if relist && !s.lastActive.IsZero() && s.lastActive.Before(cutoff) {
		cutoff = s.lastActive.Add(-time.Minute)
	}
	next := make(map[string]int, len(evs))
	var out []emit.LogRecord
	for i := range evs {
		e := &evs[i]
		if e.Type != "Warning" {
			continue
		}
		uid, count := eventUID(e), e.Occurrences()
		next[uid] = count
		if prev, ok := s.sent[uid]; ok && prev == count {
			continue
		}
		if e.Time().Before(cutoff) {
			continue
		}
		out = append(out, eventRecord(e, count))
	}
	s.sent = next
	s.lastActive = now
	s.evMu.Unlock()
	for _, rec := range out {
		s.Events.Warn(ctx, rec)
	}
}

// onEvent handles a watched event: ADDED / MODIFIED with a new count is sent once.
func (s *ClusterScraper) onEvent(ctx context.Context, typ string, e *Event) {
	uid := eventUID(e)
	s.evMu.Lock()
	s.lastActive = s.now()
	if typ == "DELETED" {
		delete(s.sent, uid)
		s.evMu.Unlock()
		return
	}
	count := e.Occurrences()
	if e.Type != "Warning" || s.sent[uid] == count {
		s.evMu.Unlock()
		return
	}
	s.sent[uid] = count
	s.evMu.Unlock()
	s.Events.Warn(ctx, eventRecord(e, count))
}

func eventUID(e *Event) string {
	if e.Metadata.UID != "" {
		return e.Metadata.UID
	}
	return e.Metadata.Namespace + "/" + e.Metadata.Name
}

// eventRecord: cluster-scoped objects (Node) have no namespace (their events live in "default").
func eventRecord(e *Event, count int) emit.LogRecord {
	return emit.LogRecord{Time: e.Time(), Body: e.Message, Attrs: EventAttrs(e, e.InvolvedObject.Namespace, count)}
}

// EventAttrs are the log attributes of a Warning event.
func EventAttrs(e *Event, ns string, count int) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(emit.AttrEventReason, e.Reason),
		attribute.String(emit.AttrEventType, e.Type),
		attribute.String(emit.AttrObjectKind, e.InvolvedObject.Kind),
		attribute.String(emit.AttrObjectName, e.InvolvedObject.Name),
		attribute.String(emit.AttrK8sNamespace, ns),
		attribute.Int(emit.AttrEventCount, count),
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}
