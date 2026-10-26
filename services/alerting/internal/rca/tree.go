package rca

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Stat is one side (current or baseline window) of an endpoint or edge aggregate.
type Stat struct {
	Calls, Errors, DurSum float64 // DurSum in ms
}

func (s Stat) metric(u Unit) float64 {
	if s.Calls <= 0 {
		return 0
	}
	if u == UnitRatio {
		return s.Errors / s.Calls
	}
	return s.DurSum / s.Calls
}

// Pair is a stat in both windows.
type Pair struct{ Cur, Base Stat }

// Edge is an outgoing dependency of an endpoint (edges_1m).
type Edge struct {
	Dep, DepKind, ChildService string
	Pair
}

// RouteCalls is how often a caller hit one route of a callee (callers_1m).
type RouteCalls struct {
	Route, Method string
	Pair
}

// Deploy is a version first seen for a service.
type Deploy struct {
	Version string
	At      time.Time
}

// Source reads the aggregates the tree is built from. An empty route means "all routes of the service".
type Source interface {
	Endpoint(ctx context.Context, service, route, method string) (Pair, error)
	Edges(ctx context.Context, service, route, method string) ([]Edge, error)
	CalleeRoutes(ctx context.Context, callee, caller string) ([]RouteCalls, error)
	Deploys(ctx context.Context, service string) ([]Deploy, error)
}

// Limits bound the number of queries a single analysis runs.
type Limits struct {
	MaxDepth int // endpoint hops (service → service)
	MaxNodes int
}

// DefaultLimits are used by the engine.
var DefaultLimits = Limits{MaxDepth: 4, MaxNodes: 60}

type builder struct {
	src   Source
	unit  Unit
	lim   Limits
	nodes int
	seen  map[string]bool
}

// Build constructs the call tree rooted at (service, route, method).
func Build(ctx context.Context, src Source, service, route, method string, unit Unit, lim Limits) (*Node, error) {
	b := &builder{src: src, unit: unit, lim: lim, seen: map[string]bool{}}
	p, err := src.Endpoint(ctx, service, route, method)
	if err != nil {
		return nil, err
	}
	root := b.endpointNode(service, route, method, p)
	if err := b.expand(ctx, root, service, route, method, p, 0); err != nil {
		return nil, err
	}
	return root, nil
}

// Analyze builds the tree and runs the attribution.
func Analyze(ctx context.Context, src Source, service, route, method string, unit Unit) (Result, *Node, error) {
	root, err := Build(ctx, src, service, route, method, unit, DefaultLimits)
	if err != nil {
		return Result{}, nil, err
	}
	return FindRootCause(root, unit), root, nil
}

func endpointLabel(service, route, method string) string {
	if route == "" {
		return service
	}
	return strings.TrimSpace(method + " " + route)
}

func (b *builder) endpointNode(service, route, method string, p Pair) *Node {
	b.nodes++
	return &Node{
		ID:       fmt.Sprintf("%s:%s %s", service, method, route),
		Kind:     "endpoint",
		Label:    endpointLabel(service, route, method),
		Service:  service,
		Baseline: p.Base.metric(b.unit),
		Current:  p.Cur.metric(b.unit),
	}
}

func perReq(edgeCalls, parentCalls Stat) float64 {
	if parentCalls.Calls > 0 {
		return edgeCalls.Calls / parentCalls.Calls
	}
	return 0
}

// callsPerReq prefers the current window; falls back to baseline when the parent had no traffic now.
func callsPerReq(edge, parent Pair) float64 {
	if v := perReq(edge.Cur, parent.Cur); v > 0 {
		return v
	}
	return perReq(edge.Base, parent.Base)
}

func depNodeKind(depKind string, hasService bool) string {
	switch depKind {
	case "db", "cache", "queue":
		return depKind
	case "http":
		if hasService {
			return "http"
		}
	}
	return "external"
}

func (b *builder) expand(ctx context.Context, n *Node, service, route, method string, p Pair, depth int) error {
	key := service + "|" + method + "|" + route
	if b.seen[key] {
		return nil // cycles (A → B → A)
	}
	b.seen[key] = true
	if deps, err := b.src.Deploys(ctx, service); err == nil {
		for _, d := range deps {
			n.OwnEvidence = append(n.OwnEvidence, Evidence{Kind: "deploy", Title: fmt.Sprintf("%s %s deployed", service, d.Version), At: d.At.UnixMilli()})
		}
	}
	edges, err := b.src.Edges(ctx, service, route, method)
	if err != nil {
		return err
	}
	for _, e := range edges {
		if b.nodes >= b.lim.MaxNodes {
			break
		}
		kind := depNodeKind(e.DepKind, e.ChildService != "")
		owner := service // the caller owns its DBs, caches and queues ("your dependency")
		if kind == "http" {
			owner = e.ChildService
		}
		child := &Node{
			ID:       service + ">" + e.Dep,
			Kind:     kind,
			Label:    e.Dep,
			Service:  owner,
			Baseline: e.Base.metric(b.unit),
			Current:  e.Cur.metric(b.unit),
		}
		b.nodes++
		n.Children = append(n.Children, Child{Node: child, CallsPerReq: callsPerReq(e.Pair, p)})
		if kind == "http" && depth+1 < b.lim.MaxDepth {
			if err := b.expandHTTP(ctx, child, service, e, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// expandHTTP splits a service-to-service edge into the callee routes this caller hits.
func (b *builder) expandHTTP(ctx context.Context, n *Node, caller string, e Edge, depth int) error {
	routes, err := b.src.CalleeRoutes(ctx, e.ChildService, caller)
	if err != nil {
		return err
	}
	var total Pair
	for _, r := range routes {
		total.Cur.Calls += r.Cur.Calls
		total.Base.Calls += r.Base.Calls
	}
	for _, r := range routes {
		if b.nodes >= b.lim.MaxNodes {
			break
		}
		ep, err := b.src.Endpoint(ctx, e.ChildService, r.Route, r.Method)
		if err != nil {
			return err
		}
		child := b.endpointNode(e.ChildService, r.Route, r.Method, ep)
		n.Children = append(n.Children, Child{Node: child, CallsPerReq: callsPerReq(r.Pair, total)})
		if err := b.expand(ctx, child, e.ChildService, r.Route, r.Method, ep, depth); err != nil {
			return err
		}
	}
	return nil
}
