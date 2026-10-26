package gw

import (
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
)

// Kafka records are capped (Redpanda default ~1 MiB), requests are not (5 MB). Oversized requests are
// re-packed greedily into several requests, each ≤ max bytes, keeping resource/scope headers on every part.

const itemOverhead = 8 // tag + length varints per nested message, generous

// packer groups leaf items under (resource, scope) parents into parts of bounded size.
type packer[R, S comparable, I any] struct {
	max   int
	parts [][]group[R, S, I]
	cur   []group[R, S, I]
	size  int
}

type group[R, S comparable, I any] struct {
	res   R
	scope S
	items []I
}

func (p *packer[R, S, I]) add(res R, scope S, item I, headerSize, itemSize int) {
	need := itemSize + itemOverhead
	n := len(p.cur)
	same := n > 0 && p.cur[n-1].res == res && p.cur[n-1].scope == scope
	if !same {
		need += headerSize
	}
	if p.size+need > p.max && p.size > 0 {
		p.flush()
		same = false
		need = itemSize + itemOverhead + headerSize
	}
	if same {
		p.cur[n-1].items = append(p.cur[n-1].items, item)
	} else {
		p.cur = append(p.cur, group[R, S, I]{res: res, scope: scope, items: []I{item}})
	}
	p.size += need
}

func (p *packer[R, S, I]) flush() {
	if len(p.cur) > 0 {
		p.parts = append(p.parts, p.cur)
	}
	p.cur, p.size = nil, 0
}

func splitTraces(req *coltracepb.ExportTraceServiceRequest, max int) []*coltracepb.ExportTraceServiceRequest {
	p := packer[*tracepb.ResourceSpans, *tracepb.ScopeSpans, *tracepb.Span]{max: max}
	for _, rs := range req.GetResourceSpans() {
		rsz := proto.Size(rs.GetResource()) + itemOverhead*2
		for _, ss := range rs.GetScopeSpans() {
			hs := rsz + proto.Size(ss.GetScope())
			for _, sp := range ss.GetSpans() {
				p.add(rs, ss, sp, hs, proto.Size(sp))
			}
		}
	}
	p.flush()
	out := make([]*coltracepb.ExportTraceServiceRequest, 0, len(p.parts))
	for _, part := range p.parts {
		r := &coltracepb.ExportTraceServiceRequest{}
		var last *tracepb.ResourceSpans
		var cur *tracepb.ResourceSpans
		for _, g := range part {
			if g.res != last {
				cur = &tracepb.ResourceSpans{Resource: g.res.GetResource(), SchemaUrl: g.res.GetSchemaUrl()}
				r.ResourceSpans = append(r.ResourceSpans, cur)
				last = g.res
			}
			cur.ScopeSpans = append(cur.ScopeSpans, &tracepb.ScopeSpans{Scope: g.scope.GetScope(), SchemaUrl: g.scope.GetSchemaUrl(), Spans: g.items})
		}
		out = append(out, r)
	}
	return out
}

func splitLogs(req *collogspb.ExportLogsServiceRequest, max int) []*collogspb.ExportLogsServiceRequest {
	p := packer[*logspb.ResourceLogs, *logspb.ScopeLogs, *logspb.LogRecord]{max: max}
	for _, rl := range req.GetResourceLogs() {
		rsz := proto.Size(rl.GetResource()) + itemOverhead*2
		for _, sl := range rl.GetScopeLogs() {
			hs := rsz + proto.Size(sl.GetScope())
			for _, lr := range sl.GetLogRecords() {
				p.add(rl, sl, lr, hs, proto.Size(lr))
			}
		}
	}
	p.flush()
	out := make([]*collogspb.ExportLogsServiceRequest, 0, len(p.parts))
	for _, part := range p.parts {
		r := &collogspb.ExportLogsServiceRequest{}
		var last, cur *logspb.ResourceLogs
		for _, g := range part {
			if g.res != last {
				cur = &logspb.ResourceLogs{Resource: g.res.GetResource(), SchemaUrl: g.res.GetSchemaUrl()}
				r.ResourceLogs = append(r.ResourceLogs, cur)
				last = g.res
			}
			cur.ScopeLogs = append(cur.ScopeLogs, &logspb.ScopeLogs{Scope: g.scope.GetScope(), SchemaUrl: g.scope.GetSchemaUrl(), LogRecords: g.items})
		}
		out = append(out, r)
	}
	return out
}

// splitMetrics packs at Metric granularity (a single metric with a huge number of points is not split).
func splitMetrics(req *colmetricspb.ExportMetricsServiceRequest, max int) []*colmetricspb.ExportMetricsServiceRequest {
	p := packer[*metricspb.ResourceMetrics, *metricspb.ScopeMetrics, *metricspb.Metric]{max: max}
	for _, rm := range req.GetResourceMetrics() {
		rsz := proto.Size(rm.GetResource()) + itemOverhead*2
		for _, sm := range rm.GetScopeMetrics() {
			hs := rsz + proto.Size(sm.GetScope())
			for _, m := range sm.GetMetrics() {
				p.add(rm, sm, m, hs, proto.Size(m))
			}
		}
	}
	p.flush()
	out := make([]*colmetricspb.ExportMetricsServiceRequest, 0, len(p.parts))
	for _, part := range p.parts {
		r := &colmetricspb.ExportMetricsServiceRequest{}
		var last, cur *metricspb.ResourceMetrics
		for _, g := range part {
			if g.res != last {
				cur = &metricspb.ResourceMetrics{Resource: g.res.GetResource(), SchemaUrl: g.res.GetSchemaUrl()}
				r.ResourceMetrics = append(r.ResourceMetrics, cur)
				last = g.res
			}
			cur.ScopeMetrics = append(cur.ScopeMetrics, &metricspb.ScopeMetrics{Scope: g.scope.GetScope(), SchemaUrl: g.scope.GetSchemaUrl(), Metrics: g.items})
		}
		out = append(out, r)
	}
	return out
}

func splitRUM(b *rumpb.RumBatch, max int) []*rumpb.RumBatch {
	hs := proto.Size(b.GetResource()) + itemOverhead
	var out []*rumpb.RumBatch
	cur := &rumpb.RumBatch{Resource: b.GetResource()}
	size := hs
	for _, ev := range b.GetEvents() {
		n := proto.Size(ev) + itemOverhead
		if size+n > max && len(cur.Events) > 0 {
			out = append(out, cur)
			cur = &rumpb.RumBatch{Resource: b.GetResource()}
			size = hs
		}
		cur.Events = append(cur.Events, ev)
		size += n
	}
	if len(cur.Events) > 0 {
		out = append(out, cur)
	}
	return out
}
