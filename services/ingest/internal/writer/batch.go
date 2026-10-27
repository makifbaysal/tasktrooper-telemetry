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
	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling"
	"github.com/tasktrooper/telemetry/services/ingest/internal/replay"
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

	LLMContent    []flatten.LLMContentRow   // prompt / completion content split off spans and GenAI log events
	Replay        []flatten.ReplayRow       // replay_segments index rows …
	Blobs         []Blob                    // … and their S3 objects (uploaded before the rows are inserted)
	ProfileStacks []flatten.ProfileStackRow // profile_stacks
	Profiles      []flatten.ProfileRow      // profiles

	Geo            *geo.DB // optional GeoIP database (country of RUM events from the client_ip header)
	sampled        int     // Spans[:sampled] already went through tail sampling
	contentSampled int     // LLMContent[:contentSampled] already filtered by tail sampling
	blobBytes      int     // sum of len(Blobs[i].Body)
}

// maxBufferedBlobBytes flushes early when replay segments pile up in memory.
const maxBufferedBlobBytes = 64 << 20

// Full reports whether the buffered blobs call for a flush regardless of row counts.
func (b *Batches) Full() bool { return b.blobBytes >= maxBufferedBlobBytes }

// Blob is one object to upload (session replay segment).
type Blob struct {
	Key  string
	Body []byte // gzip'ed JSON
}

// Replay segments and profiles are decompressed with these bounds (gw enforces the same limits).
const (
	maxReplayBytes  = 10 << 20
	maxProfileBytes = 32 << 20
)

// NewBatches returns empty batches.
func NewBatches() *Batches { return &Batches{Metrics: flatten.NewMetricsBatch()} }

// Rows is the size of the largest table batch (flush trigger).
func (b *Batches) Rows() int {
	return max(len(b.Spans), len(b.Logs), b.Metrics.Len(), len(b.Rum), len(b.LLMContent), len(b.Replay), len(b.ProfileStacks))
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
	clear(b.LLMContent)
	b.LLMContent = b.LLMContent[:0]
	b.contentSampled = 0
	clear(b.Replay)
	b.Replay = b.Replay[:0]
	clear(b.Blobs)
	b.Blobs = b.Blobs[:0]
	b.blobBytes = 0
	clear(b.ProfileStacks)
	b.ProfileStacks = b.ProfileStacks[:0]
	clear(b.Profiles)
	b.Profiles = b.Profiles[:0]
}

// recordMeta reads the headers ingest-gw attaches.
type recordMeta struct {
	tenant     tenant.Tenant
	receivedAt time.Time
	userAgent  string
	clientIP   string            // only used for the GeoIP lookup, never stored
	extra      map[string]string // topic-specific headers (tt.replay, tt.profiles)
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
		default:
			if m.extra == nil {
				m.extra = make(map[string]string, 12)
			}
			m.extra[h.Key] = string(h.Value)
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
		b.Spans, b.LLMContent = flatten.SpansWithContent(m.tenant, &req, b.Spans, b.LLMContent)
	case topics.Logs:
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(r.Value, &req); err != nil {
			return err
		}
		b.Logs, b.LLMContent = flatten.LogsWithContent(m.tenant, &req, m.receivedAt, b.Logs, b.LLMContent)
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
	case topics.Replay:
		return b.addReplay(r, m)
	case topics.Profiles:
		return b.addProfile(r, m)
	default:
		return fmt.Errorf("unexpected topic %q", r.Topic)
	}
	return nil
}

// addReplay: the value is the gzip'ed segment JSON; it is stored as is in S3 and indexed in replay_segments.
func (b *Batches) addReplay(r *kgo.Record, m recordMeta) error {
	if !profiling.IsGzip(r.Value) {
		return fmt.Errorf("replay value is not gzip'ed")
	}
	raw, err := profiling.Gunzip(r.Value, maxReplayBytes)
	if err != nil {
		return err
	}
	seg, err := replay.Decode(raw)
	if err != nil {
		return err
	}
	row := flatten.Replay(m.tenant, seg, len(r.Value), m.receivedAt)
	b.Replay = append(b.Replay, row)
	b.Blobs = append(b.Blobs, Blob{Key: row.BlobKey, Body: r.Value})
	b.blobBytes += len(r.Value)
	return nil
}

// addProfile parses a pprof / folded profile (metadata in the headers) into profile_stacks + profiles rows.
func (b *Batches) addProfile(r *kgo.Record, m recordMeta) error {
	h := m.extra
	start, err1 := strconv.ParseInt(h[topics.HeaderStart], 10, 64)
	end, err2 := strconv.ParseInt(h[topics.HeaderEnd], 10, 64)
	if err1 != nil || err2 != nil || start <= 0 || end < start {
		return fmt.Errorf("profile headers: invalid start/end %q/%q", h[topics.HeaderStart], h[topics.HeaderEnd])
	}
	if h[topics.HeaderService] == "" {
		return fmt.Errorf("profile headers: missing service")
	}
	series, err := profiling.Parse(r.Value, profiling.Options{Format: h[topics.HeaderFormat], Type: h[topics.HeaderType],
		Unit: h[topics.HeaderUnit], Runtime: h[topics.HeaderRuntime], MaxSize: maxProfileBytes})
	if err != nil {
		return err
	}
	meta := flatten.ProfileMeta{Service: h[topics.HeaderService], Env: h[topics.HeaderEnv], Version: h[topics.HeaderVersion],
		Host: h[topics.HeaderHost], Runtime: h[topics.HeaderRuntime], Start: time.UnixMilli(start).UTC(), End: time.UnixMilli(end).UTC()}
	b.ProfileStacks, b.Profiles = flatten.Profiles(m.tenant, meta, series, b.ProfileStacks, b.Profiles)
	return nil
}

// Sample applies tail sampling to the spans added since the last call (a failed flush keeps its rows,
// so already-sampled rows must not be sampled — and re-weighted — twice).
func (b *Batches) Sample(s TailSampler) {
	if !s.Enabled() || b.sampled >= len(b.Spans) {
		b.sampled = len(b.Spans)
		b.contentSampled = len(b.LLMContent)
		return
	}
	// LLM content of dropped traces goes too: a content row is dropped when its trace had spans in this window and
	// none of them was kept (content of a trace without spans here, e.g. from a log event, stays).
	var seen map[[16]byte]bool // trace id → kept
	if b.contentSampled < len(b.LLMContent) {
		seen = make(map[[16]byte]bool, (len(b.Spans)-b.sampled)/4+1)
		for i := b.sampled; i < len(b.Spans); i++ {
			seen[[16]byte(b.Spans[i].TraceID)] = false
		}
	}
	kept := s.Apply(b.Spans[b.sampled:])
	if seen != nil {
		for i := range kept {
			seen[[16]byte(kept[i].TraceID)] = true
		}
		out := b.LLMContent[:b.contentSampled]
		for _, c := range b.LLMContent[b.contentSampled:] {
			if k, ok := seen[[16]byte(c.TraceID)]; !ok || k {
				out = append(out, c)
			}
		}
		clear(b.LLMContent[len(out):])
		b.LLMContent = out
	}
	b.Spans = b.Spans[:b.sampled+len(kept)]
	b.sampled = len(b.Spans)
	b.contentSampled = len(b.LLMContent)
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
	add("llm_content", flatten.LLMContentColumns, len(b.LLMContent), func(i int) []any { return b.LLMContent[i].Values() })
	add("replay_segments", flatten.ReplayColumns, len(b.Replay), func(i int) []any { return b.Replay[i].Values() })
	add("profile_stacks", flatten.ProfileStackColumns, len(b.ProfileStacks), func(i int) []any { return b.ProfileStacks[i].Values() })
	add("profiles", flatten.ProfileColumns, len(b.Profiles), func(i int) []any { return b.Profiles[i].Values() })
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
