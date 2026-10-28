// Package infra implements trooper-agent's `host` and `cluster` modes (contracts §20, ADR-019):
// host / container / process metrics from gopsutil, the kubelet or the Docker Engine API, and
// Kubernetes object state + Warning events from the API server. Everything is read-only and
// talks plain net/http (no client-go, no docker SDK).
package infra

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- minimal Kubernetes types

// OwnerRef is metadata.ownerReferences[].
type OwnerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller *bool  `json:"controller,omitempty"`
}

// ObjectMeta is the subset of metadata we read.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels"`
	OwnerReferences   []OwnerRef        `json:"ownerReferences"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
}

// Resources is resources.requests/limits.
type Resources struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

// ContainerSpec is spec.containers[].
type ContainerSpec struct {
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	Resources Resources `json:"resources"`
}

// ContainerState is status.containerStatuses[].state / lastState.
type ContainerState struct {
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting,omitempty"`
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running,omitempty"`
	Terminated *struct {
		Reason     string    `json:"reason"`
		ExitCode   int       `json:"exitCode"`
		FinishedAt time.Time `json:"finishedAt"`
	} `json:"terminated,omitempty"`
}

// ContainerStatus is status.containerStatuses[].
type ContainerStatus struct {
	Name         string         `json:"name"`
	Image        string         `json:"image"`
	ContainerID  string         `json:"containerID"`
	RestartCount int            `json:"restartCount"`
	Ready        bool           `json:"ready"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState"`
}

// Condition is a status condition.
type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// Pod is the subset of v1.Pod we read.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		NodeName   string          `json:"nodeName"`
		Containers []ContainerSpec `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string            `json:"phase"`
		Reason            string            `json:"reason"`
		Conditions        []Condition       `json:"conditions"`
		ContainerStatuses []ContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// Ready reports the Ready condition.
func (p *Pod) Ready() bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// Restarts sums the containers' restartCount.
func (p *Pod) Restarts() int {
	n := 0
	for _, c := range p.Status.ContainerStatuses {
		n += c.RestartCount
	}
	return n
}

// Status returns the container status by name.
func (p *Pod) ContainerStatus(name string) (ContainerStatus, bool) {
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == name {
			return c, true
		}
	}
	return ContainerStatus{}, false
}

// ---------------------------------------------------------------- rules shared by host and cluster mode

// ServiceLabels is the precedence of the trooper.service label (contracts §20).
var ServiceLabels = []string{"tasktrooper.io/service", "app.kubernetes.io/name", "app"}

// ServiceOf returns trooper.service for a pod's labels ("" when none is set).
func ServiceOf(labels map[string]string) string {
	for _, k := range ServiceLabels {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// badReasons are the container reasons reported as trooper.k8s.pod.reason (bounded set:
// cardinality discipline; "ContainerCreating"/"Completed" & co. are normal and not reported).
var badReasons = map[string]bool{
	"CrashLoopBackOff": true, "OOMKilled": true, "ImagePullBackOff": true, "ErrImagePull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "InvalidImageName": true,
	"RunContainerError": true, "ContainerCannotRun": true, "Evicted": true, "Error": true,
	"DeadlineExceeded": true, "StartError": true,
}

// RecentTermination: a lastState.terminated reason (e.g. OOMKilled before the restart) is reported
// while it is this recent, so a restarted pod still shows why it restarted.
const RecentTermination = 15 * time.Minute

// PodReason returns trooper.k8s.pod.reason: pod status reason (Evicted), then a container's waiting
// reason, its terminated reason, then a recent lastState.terminated reason; "" when healthy.
func PodReason(p *Pod, now time.Time) string {
	if badReasons[p.Status.Reason] {
		return p.Status.Reason
	}
	for _, c := range p.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil && badReasons[w.Reason] {
			// CrashLoopBackOff after an OOM kill: the OOM is the more useful cause
			if w.Reason == "CrashLoopBackOff" && c.LastState.Terminated != nil && c.LastState.Terminated.Reason == "OOMKilled" {
				return "OOMKilled"
			}
			return w.Reason
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		if t := c.State.Terminated; t != nil && badReasons[t.Reason] {
			return t.Reason
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		if t := c.LastState.Terminated; t != nil && badReasons[t.Reason] && now.Sub(t.FinishedAt) < RecentTermination {
			return t.Reason
		}
	}
	return ""
}

// ContainerReason is PodReason for one container.
func ContainerReason(c ContainerStatus, now time.Time) string {
	if w := c.State.Waiting; w != nil && badReasons[w.Reason] {
		return w.Reason
	}
	if t := c.State.Terminated; t != nil && badReasons[t.Reason] {
		return t.Reason
	}
	if t := c.LastState.Terminated; t != nil && badReasons[t.Reason] && now.Sub(t.FinishedAt) < RecentTermination {
		return t.Reason
	}
	return ""
}

var cronJobSuffix = regexp.MustCompile(`^(.+)-\d{8,}$`)

// controllerOf returns the controlling owner reference.
func controllerOf(refs []OwnerRef) (OwnerRef, bool) {
	for _, r := range refs {
		if r.Controller != nil && *r.Controller {
			return r, true
		}
	}
	if len(refs) > 0 {
		return refs[0], true
	}
	return OwnerRef{}, false
}

// Owners resolves intermediate owners (ReplicaSet → Deployment, Job → CronJob). Cluster mode fills
// it from the API; nil maps fall back to name heuristics (host mode).
type Owners struct {
	// "<ns>/<replicaset>" → deployment name ("" = standalone ReplicaSet)
	ReplicaSets map[string]string
	// "<ns>/<job>" → cronjob name ("" = standalone Job)
	Jobs map[string]string
}

// Workload returns (trooper.k8s.workload.kind, .name) of a pod. Pods without a controller (static
// or bare pods) return ("", "").
func (o Owners) Workload(meta ObjectMeta) (string, string) {
	ref, ok := controllerOf(meta.OwnerReferences)
	if !ok {
		return "", ""
	}
	switch ref.Kind {
	case "ReplicaSet":
		if o.ReplicaSets != nil {
			if d, ok := o.ReplicaSets[meta.Namespace+"/"+ref.Name]; ok {
				if d == "" {
					return "ReplicaSet", ref.Name
				}
				return "Deployment", d
			}
		}
		return "Deployment", deploymentFromReplicaSet(ref.Name, meta.Labels["pod-template-hash"])
	case "Job":
		if o.Jobs != nil {
			if c, ok := o.Jobs[meta.Namespace+"/"+ref.Name]; ok {
				if c == "" {
					return "Job", ref.Name
				}
				return "CronJob", c
			}
		}
		if m := cronJobSuffix.FindStringSubmatch(ref.Name); m != nil {
			return "CronJob", m[1]
		}
		return "Job", ref.Name
	case "Node":
		return "", "" // static pod
	}
	return ref.Kind, ref.Name
}

var hashLike = regexp.MustCompile(`^[a-z0-9]{5,10}$`)

// deploymentFromReplicaSet strips the pod-template-hash suffix ("api-7d9f8b6c5" → "api").
func deploymentFromReplicaSet(rs, hash string) string {
	if hash != "" && strings.HasSuffix(rs, "-"+hash) {
		return strings.TrimSuffix(rs, "-"+hash)
	}
	if i := strings.LastIndexByte(rs, '-'); i > 0 && hashLike.MatchString(rs[i+1:]) {
		return rs[:i]
	}
	return rs
}

// StripContainerID turns "containerd://abc" / "docker://abc" into "abc".
func StripContainerID(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		return s[i+3:]
	}
	return s
}

// ---------------------------------------------------------------- quantities

var binarySuffix = map[string]float64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60}
var decimalSuffix = map[string]float64{"n": 1e-9, "u": 1e-6, "m": 1e-3, "": 1, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18}

// ParseQuantity parses a Kubernetes resource quantity ("100m", "1.5", "128Mi", "1e3", "2G") into a
// float (cores for cpu, bytes for memory). Invalid input → 0, false.
func ParseQuantity(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if len(s) > 2 {
		if m, ok := binarySuffix[s[len(s)-2:]]; ok {
			v, err := strconv.ParseFloat(s[:len(s)-2], 64)
			return v * m, err == nil
		}
	}
	// exponent form (1e3, 2E6) — careful: "E" alone is the exa suffix
	if i := strings.IndexAny(s, "eE"); i > 0 && i < len(s)-1 {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v, true
		}
	}
	last := s[len(s)-1:]
	if m, ok := decimalSuffix[last]; ok && (last < "0" || last > "9") {
		v, err := strconv.ParseFloat(s[:len(s)-1], 64)
		return v * m, err == nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

// sumResource sums a resource over containers. Requests: ok when at least one container sets it.
// Limits: ok only when every container sets one (a single unlimited container = unlimited pod).
func sumResource(cs []ContainerSpec, limits bool, name string) (float64, bool) {
	total, n := 0.0, 0
	for _, c := range cs {
		m := c.Resources.Requests
		if limits {
			m = c.Resources.Limits
		}
		q := m[name]
		if q == "" && !limits {
			q = c.Resources.Limits[name] // the API server defaults request = limit
		}
		if v, ok := ParseQuantity(q); ok {
			total += v
			n++
		}
	}
	if limits {
		return total, n > 0 && n == len(cs)
	}
	return total, n > 0
}
