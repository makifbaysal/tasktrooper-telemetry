// Package rca finds the root cause of a latency or error change by delta attribution over the call tree.
// FindRootCause is a port of apps/web/src/lib/rca.ts (same algorithm, same JSON shape as RcaResult in
// apps/web/src/data/types.ts); tree.go builds the tree from edges_1m / red_1m / callers_1m.
package rca

import (
	"fmt"
	"math"
	"regexp"
	"sort"
)

// Unit of node metrics.
type Unit string

const (
	UnitMs    Unit = "ms"
	UnitRatio Unit = "ratio"
)

// MinShare: siblings explaining at least this much are listed as alternatives.
const MinShare = 0.15

const maxDepth = 12

// Node mirrors RcaNode.
type Node struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"` // page | app_start | endpoint | db | cache | http | queue | external | code
	Label       string     `json:"label"`
	Service     string     `json:"service,omitempty"`
	Link        string     `json:"link,omitempty"`
	Baseline    float64    `json:"baseline"`
	Current     float64    `json:"current"`
	Children    []Child    `json:"children,omitempty"`
	Evidence    []Evidence `json:"evidence,omitempty"`
	OwnEvidence []Evidence `json:"ownEvidence,omitempty"`
}

// Child is an edge with its fan-out.
type Child struct {
	Node        *Node   `json:"node"`
	CallsPerReq float64 `json:"callsPerReq"`
	Parallel    bool    `json:"parallel,omitempty"`
}

// Evidence mirrors RcaEvidence.
type Evidence struct {
	Kind   string `json:"kind"` // deploy | migration | query | resource | log | external
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	At     int64  `json:"at,omitempty"` // unix ms
	Link   string `json:"link,omitempty"`
}

// Alt is a sibling that also moved.
type Alt struct {
	Node     *Node   `json:"node"`
	Explains float64 `json:"explains"`
}

// Hop mirrors RcaHop.
type Hop struct {
	Node         *Node   `json:"node"`
	Explains     float64 `json:"explains"`
	Contribution float64 `json:"contribution"`
	CallsPerReq  float64 `json:"callsPerReq"`
	Alternatives []Alt   `json:"alternatives"`
}

// Result mirrors RcaResult.
type Result struct {
	Unit       Unit    `json:"unit"`
	Path       []Hop   `json:"path"`
	Confidence float64 `json:"confidence"`
	Root       *Node   `json:"root"`
	Summary    string  `json:"summary"`
}

type cand struct {
	node         *Node
	callsPerReq  float64
	contribution float64
	explains     float64
}

// FindRootCause walks from root following the child that explains most of each node's change.
func FindRootCause(root *Node, unit Unit) Result {
	path := []Hop{}
	node := root
	confidence := 1.0
	for depth := 0; depth < maxDepth; depth++ {
		if len(node.Children) == 0 || node.Current-node.Baseline <= 0 {
			break
		}
		var positive []cand
		total := 0.0
		for _, c := range contributions(node) {
			if c.contribution > 0 {
				positive = append(positive, c)
				total += c.contribution
			}
		}
		if total <= 0 {
			break
		}
		for i := range positive {
			positive[i].explains = positive[i].contribution / total
		}
		sort.SliceStable(positive, func(i, j int) bool { return positive[i].explains > positive[j].explains })
		best := positive[0]
		alts := []Alt{}
		for _, c := range positive[1:] {
			if c.explains >= MinShare {
				alts = append(alts, Alt{Node: c.node, Explains: c.explains})
			}
		}
		path = append(path, Hop{Node: best.node, Explains: best.explains, Contribution: best.contribution, CallsPerReq: best.callsPerReq, Alternatives: alts})
		confidence *= best.explains
		if best.node.Kind == "code" {
			break
		}
		node = best.node
	}
	last := root
	if len(path) > 0 {
		last = path[len(path)-1].Node
	}
	return Result{Unit: unit, Path: path, Confidence: confidence, Root: last, Summary: summarize(root, last, unit)}
}

func contributions(node *Node) []cand {
	var out []cand
	var parallel []Child
	for _, c := range node.Children {
		if c.Parallel {
			parallel = append(parallel, c)
			continue
		}
		out = append(out, cand{node: c.Node, callsPerReq: c.CallsPerReq, contribution: c.CallsPerReq * (c.Node.Current - c.Node.Baseline)})
	}
	if len(parallel) > 0 {
		// only the slowest parallel branch is on the critical path
		before, after := math.Inf(-1), math.Inf(-1)
		critical := parallel[0]
		for _, c := range parallel {
			before = math.Max(before, c.CallsPerReq*c.Node.Baseline)
			after = math.Max(after, c.CallsPerReq*c.Node.Current)
			if c.CallsPerReq*c.Node.Current > critical.CallsPerReq*critical.Node.Current {
				critical = c
			}
		}
		out = append(out, cand{node: critical.Node, callsPerReq: critical.CallsPerReq, contribution: after - before})
	}
	explained := 0.0
	for _, c := range out {
		explained += c.contribution
	}
	if own := node.Current - node.Baseline - explained; own > 0 {
		label := node.Label
		if node.Service != "" {
			label = node.Service
		}
		out = append(out, cand{
			node:         &Node{ID: node.ID + ":code", Kind: "code", Label: label + " own code", Service: node.Service, Link: node.Link, Baseline: 0, Current: own, Evidence: node.OwnEvidence},
			callsPerReq:  1,
			contribution: own,
		})
	}
	return out
}

var shortRe = regexp.MustCompile(`^.* · `)

func short(n *Node) string { return shortRe.ReplaceAllString(n.Label, "") }

func noun(n *Node) string {
	switch n.Kind {
	case "db":
		return "database " + short(n)
	case "cache":
		return "cache " + short(n)
	case "queue":
		return "queue " + short(n)
	case "external":
		return "external dependency " + short(n)
	case "endpoint":
		return "endpoint " + n.Label
	}
	return short(n)
}

func summarize(root, cause *Node, unit Unit) string {
	if cause == root {
		return root.Label + " changed, but no single dependency explains it."
	}
	svc := cause.Service
	if svc == "" {
		svc = "The service"
	}
	if cause.Kind == "code" {
		if unit == UnitRatio {
			return svc + "'s own code started failing — none of the services or databases it calls explain it."
		}
		return svc + "'s own code got slower — none of the services or databases it calls explain it."
	}
	owner := ""
	if cause.Service != "" {
		owner = cause.Service + "'s "
	}
	if unit == UnitRatio {
		return fmt.Sprintf("Failures come from %s%s (%.1f%% → %.1f%% of calls failing).", owner, noun(cause), cause.Baseline*100, cause.Current*100)
	}
	x := 0.0
	if cause.Baseline > 0 {
		x = cause.Current / cause.Baseline
	}
	factor := "slower"
	if x >= 10 {
		factor = fmt.Sprintf("%d× slower", int(math.Round(x)))
	} else if x >= 2 {
		factor = fmt.Sprintf("%.1f× slower", x)
	}
	return fmt.Sprintf("Caused by %s%s — %s than usual.", owner, noun(cause), factor)
}

// Compact returns a copy of r whose nodes carry no children, for storing in incidents.root_cause.
func Compact(r Result) Result {
	strip := func(n *Node) *Node {
		if n == nil {
			return nil
		}
		c := *n
		c.Children = nil
		return &c
	}
	out := r
	out.Root = strip(r.Root)
	out.Path = make([]Hop, len(r.Path))
	for i, h := range r.Path {
		h.Node = strip(h.Node)
		alts := make([]Alt, len(h.Alternatives))
		for j, a := range h.Alternatives {
			alts[j] = Alt{Node: strip(a.Node), Explains: a.Explains}
		}
		h.Alternatives = alts
		out.Path[i] = h
	}
	return out
}
