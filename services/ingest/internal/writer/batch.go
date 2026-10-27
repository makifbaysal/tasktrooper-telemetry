// Package writer is ingest-writer: it consumes the ingest topics, flattens payloads into rows and
// batch-inserts them into ClickHouse, committing Kafka offsets only after a successful insert.
package writer

import (
	"fmt"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/flatten"
	"github.com/tasktrooper/telemetry/services/ingest/internal/geo"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// Batches accumulates rows for every table until the next flush.
type Batches struct {
	Spans   []flatten.SpanRow
	Logs    []flatten.LogRow
	Metrics *flatten.MetricsBatch
	Rum     []flatten.RumRow

	Geo     *geo.DB // optional GeoIP database (country of RUM events from the client_ip header)
	sampled int     // Spans[:sampled] already went through tail sampling
}

// NewBatches returns empty batches.
func NewBatches() *Batches { return &Batches{Metrics: flatten.NewMetricsBatch()} }

// Rows is the size of the largest table batch (flush trigger).
func (b *Batches) Rows() int {
	return max(len(b.Spans), len(b.Logs), b.Metrics.Len(), len(b.Rum))
}

// Reset empties all batches.
func (b *Batches) Reset() {
	clear(b.Spans) // drop references so maps can be collected
	b.Spans = b.Spans[:0]
	b.sampled = 0
	clear(b.Logs)
	b.Logs = b.Logs[:0]
	clear(b.Rum)
	b.Rum = b.Rum[:0]
	b.Metrics.Reset()
}

// recordMeta reads the headers ingest-gw attaches.
type recordMeta struct {
	tenant     tenant.Tenant
	receivedAt time.Time
	userAgent  string
	clientIP   string // only used for the GeoIP lookup, never stored
}

func parseHeaders(r *kgo.Record) (recordMeta, error) {
	var org, project, recv string
	var m recordMeta
	for _, h := range r.Headers {
		switch h.Key {
		case topics.HeaderOrgID:
			org = string(h.Value)
		case topics.HeaderProjectID:
			project = string(h.Value)
		case topics.HeaderReceivedAt:
			recv = string(h.Value)
		case topics.HeaderUserAgent:
			m.userAgent = string(h.Value)
		case topics.HeaderClientIP:
			m.clientIP = string(h.Value)
		}
	}
	t, err := tenant.Parse(org, project)
	if err != nil {
		return m, fmt.Errorf("tenant headers: %w", err)
	}
	m.tenant = t
	if ms, err := strconv.ParseInt(recv, 10, 64); err == nil && ms > 0 {
		m.receivedAt = time.UnixMilli(ms).UTC()
	} else {
		m.receivedAt = r.Timestamp.UTC()
	}
	return m, nil
}

// Add decodes one record and appends its rows. A returned error means the record is unusable
// (it is skipped, never retried).
func (b *Batches) Add(r *kgo.Record) error {
	m, err := parseHeaders(r)
	if err != nil {
		return err
	}
	switch r.Topic {
	case topics.Traces:
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(r.Value, &req); err != nil {
			return err
		}
		b.Spans = flatten.Spans(m.tenant, &req, b.Spans)
	case topics.Logs:
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(r.Value, &req); err != nil {
			return err
		}
		b.Logs = flatten.Logs(m.tenant, &req, m.receivedAt, b.Logs)
	case topics.Metrics:
		var req colmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(r.Value, &req); err != nil {
			return err
		}
		b.Metrics.Add(m.tenant, &req)
	case topics.RUM:
		var req rumpb.RumBatch
		if err := proto.Unmarshal(r.Value, &req); err != nil {
			return err
		}
		b.Rum = flatten.Rum(m.tenant, &req, flatten.RumMeta{ReceivedAt: m.receivedAt, UserAgent: m.userAgent, Country: b.Geo.Country(m.clientIP)}, b.Rum)
	default:
		return fmt.Errorf("unexpected topic %q", r.Topic)
	}
	return nil
}

// Sample applies tail sampling to the spans added since the last call (a failed flush keeps its rows,
// so already-sampled rows must not be sampled — and re-weighted — twice).
func (b *Batches) Sample(s TailSampler) {
	if !s.Enabled() || b.sampled >= len(b.Spans) {
		b.sampled = len(b.Spans)
		return
	}
	kept := s.Apply(b.Spans[b.sampled:])
	b.Spans = b.Spans[:b.sampled+len(kept)]
	b.sampled = len(b.Spans)
}

// tableBatch is one INSERT.
type tableBatch struct {
	table, columns string
	n              int
	row            func(i int) []any
}

func mapRows[K comparable, V interface{ Values() []any }](m map[K]V) (int, func(int) []any) {
	vs := make([]V, 0, len(m))
	for _, v := range m {
		vs = append(vs, v)
	}
	return len(vs), func(i int) []any { return vs[i].Values() }
}

// tables lists the non-empty inserts of this flush.
func (b *Batches) tables() []tableBatch {
	var out []tableBatch
	add := func(table, cols string, n int, row func(int) []any) {
		if n > 0 {
			out = append(out, tableBatch{table, cols, n, row})
		}
	}
	add("spans", flatten.SpanColumns, len(b.Spans), func(i int) []any { return b.Spans[i].Values() })
	add("logs", flatten.LogColumns, len(b.Logs), func(i int) []any { return b.Logs[i].Values() })
	add("rum_events", flatten.RumColumns, len(b.Rum), func(i int) []any { return b.Rum[i].Values() })
	mb := b.Metrics
	add("metrics", flatten.MetricColumns, len(mb.Metrics), func(i int) []any { return mb.Metrics[i].Values() })
	n, f := mapRows(mb.DB)
	add("db_1m", flatten.DBColumns, n, f)
	n, f = mapRows(mb.DBQueries)
	add("db_queries_1m", flatten.DBQueryColumns, n, f)
	n, f = mapRows(mb.BrokerLag)
	add("broker_lag_1m", flatten.BrokerLagColumns, n, f)
	n, f = mapRows(mb.BrokerGroups)
	add("broker_groups_1m", flatten.BrokerGroupColumns, n, f)
	n, f = mapRows(mb.BrokerNodes)
	add("broker_nodes_1m", flatten.BrokerNodeColumns, n, f)
	n, f = mapRows(mb.BrokerTopics)
	add("broker_topics_1m", flatten.BrokerTopicColumns, n, f)
	for fam, rows := range mb.Infra {
		n, f = mapRows(rows)
		add(flatten.InfraTable(flatten.Family(fam)), flatten.InfraColumns(flatten.Family(fam)), n, f)
	}
	return out
}
