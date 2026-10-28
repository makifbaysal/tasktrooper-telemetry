package rca

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

// Infrastructure root-cause candidates (contracts §20, ADR-019): for a service incident, pods of the same service that
// were OOMKilled / CrashLoopBackOff or restarted in the window, and hosts running the service whose CPU is saturated.
// Confidence is 0.6–0.85: infra signals are strong but circumstantial (the pod may have restarted for another reason).

const (
	InfraMinConfidence = 0.6
	InfraMaxConfidence = 0.85
	// HostCPUSaturated is the average CPU (0..1) above which a host is a candidate.
	HostCPUSaturated = 0.9
	maxInfraCands    = 5
)

// PodSignal is one pod of the service over the RCA window.
type PodSignal struct {
	Cluster, Namespace, Pod, Node string
	Reasons                       []string // waiting/terminated reasons seen in the window
	RestartIncrease               float64
}

// HostSignal is one host (or node) running the service.
type HostSignal struct {
	Host string
	CPU  float64 // average 0..1 over the window
}

// InfraCandidate is one infra root-cause candidate (incidents.root_cause.infra[]).
type InfraCandidate struct {
	Kind       string  `json:"kind"`   // pod | host
	Entity     string  `json:"entity"` // namespace/pod | host
	Cluster    string  `json:"cluster,omitempty"`
	Node       string  `json:"node,omitempty"`
	Reason     string  `json:"reason,omitempty"` // OOMKilled | CrashLoopBackOff | restarts | cpu
	Restarts   float64 `json:"restarts,omitempty"`
	Value      float64 `json:"value,omitempty"` // host cpu (0..1)
	Title      string  `json:"title"`
	Confidence float64 `json:"confidence"`
}

func clampConf(c float64) float64 { return math.Min(InfraMaxConfidence, math.Max(InfraMinConfidence, c)) }

// InfraCandidates ranks pod and host signals (most confident first, at most 5).
func InfraCandidates(pods []PodSignal, hosts []HostSignal) []InfraCandidate {
	var out []InfraCandidate
	affected := 0
	for _, p := range pods {
		if p.RestartIncrease > 0 || slices.Contains(p.Reasons, "OOMKilled") || slices.Contains(p.Reasons, "CrashLoopBackOff") {
			affected++
		}
	}
	for _, p := range pods {
		var reason string
		var conf float64
		switch {
		case slices.Contains(p.Reasons, "OOMKilled"):
			reason, conf = "OOMKilled", 0.8
		case slices.Contains(p.Reasons, "CrashLoopBackOff"):
			reason, conf = "CrashLoopBackOff", 0.75
		case p.RestartIncrease > 0:
			reason, conf = "restarts", 0.6
		default:
			continue
		}
		if p.RestartIncrease >= 3 {
			conf += 0.05
		}
		if affected > 1 { // several pods of the service failing at once
			conf += 0.05
		}
		entity := p.Pod
		if p.Namespace != "" {
			entity = p.Namespace + "/" + p.Pod
		}
		title := fmt.Sprintf("Pod %s restarted (%s)", p.Pod, reason)
		if reason == "restarts" {
			title = fmt.Sprintf("Pod %s restarted %g× in the last 15 min", p.Pod, p.RestartIncrease)
		}
		out = append(out, InfraCandidate{Kind: "pod", Entity: entity, Cluster: p.Cluster, Node: p.Node, Reason: reason,
			Restarts: p.RestartIncrease, Title: title, Confidence: clampConf(conf)})
	}
	for _, h := range hosts {
		if !(h.CPU > HostCPUSaturated) {
			continue
		}
		conf := InfraMinConfidence + (h.CPU-HostCPUSaturated)*2.5
		out = append(out, InfraCandidate{Kind: "host", Entity: h.Host, Reason: "cpu", Value: h.CPU,
			Title: fmt.Sprintf("Host %s CPU saturated (%.0f%%)", h.Host, h.CPU*100), Confidence: clampConf(conf)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	if len(out) > maxInfraCands {
		out = out[:maxInfraCands]
	}
	return out
}

// ApplyInfra merges infra candidates into a (compacted) call-tree result. The best candidate becomes the answer when
// there is no tree, the tree ends in the service's own code or explains nothing, or it is less confident; the tree's
// answer is then kept in TreeSummary/TreeConfidence. Candidates are also added to the root node's evidence
// (kind "resource") so the UI shows them without knowing the infra field.
func ApplyInfra(res *Result, service string, unit Unit, cands []InfraCandidate) *Result {
	if len(cands) == 0 {
		return res
	}
	best := cands[0]
	if res == nil {
		if unit == "" {
			unit = UnitRatio
		}
		res = &Result{Unit: unit, Path: []Hop{}, Root: &Node{ID: "service:" + service, Kind: "endpoint", Label: service, Service: service},
			Summary: best.Title + ".", Confidence: best.Confidence}
	} else {
		weak := res.Root == nil || res.Root.Kind == "code" || len(res.Path) == 0 || res.Confidence < best.Confidence
		if weak {
			tc := res.Confidence
			res.TreeSummary, res.TreeConfidence = res.Summary, &tc
			res.Summary, res.Confidence = best.Title+".", best.Confidence
		}
	}
	res.Infra = cands
	if res.Root != nil {
		root := *res.Root
		root.Evidence = append(slices.Clone(root.Evidence), infraEvidence(cands)...)
		res.Root = &root
	}
	return res
}

func infraEvidence(cands []InfraCandidate) []Evidence {
	ev := make([]Evidence, len(cands))
	for i, c := range cands {
		d := fmt.Sprintf("%.0f%% confidence", c.Confidence*100)
		if c.Cluster != "" {
			d = c.Cluster + " · " + d
		}
		ev[i] = Evidence{Kind: "resource", Title: c.Title, Detail: d}
	}
	return ev
}
