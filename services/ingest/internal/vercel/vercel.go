// Package vercel converts Vercel drain payloads (log, trace, speed-insights and web-analytics drains)
// into the ingest wire formats: OTLP logs, OTLP traces and RUM batches (contracts §10, ADR-012).
//
// Schemas: https://vercel.com/docs/drains/reference/{logs,traces,speed-insights,analytics}.
// Bodies are a JSON array, NDJSON or a single JSON object; trace drains send OTLP/HTTP JSON or protobuf.
// The drain kind is detected per entry, so one request may even mix kinds.
package vercel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/tasktrooper/telemetry/services/ingest/internal/norm"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
)

// Integration is the per-drain configuration resolved from Postgres integrations (provider = 'vercel').
type Integration struct {
	ID        int64
	ProjectID int64             // TaskTrooper project the integration belongs to (must match the ingest key)
	Name      string            // integration name (fallback RUM app / service name)
	Secret    []byte            // drain signature secret (config.drainSecretRef); empty → data requests are rejected
	Verify    string            // config.verifyToken, echoed in x-vercel-verify
	Projects  map[string]string // config.projectMap: Vercel project name or id → service name
	VitalsInS bool              // config.speedInsightsUnit == "s": LCP/FCP/TTFB/INP/FID arrive in seconds
}

// VerifySignature checks x-vercel-signature = hex(HMAC-SHA1(secret, body)) in constant time.
func VerifySignature(secret, body []byte, sig string) bool {
	if len(secret) == 0 || sig == "" {
		return false
	}
	want, err := hex.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		return false
	}
	m := hmac.New(sha1.New, secret)
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}

// Sign returns the signature Vercel would send (tests, e2e).
func Sign(secret, body []byte) string {
	m := hmac.New(sha1.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Kinds counted per request.
const (
	KindLog       = "log"
	KindTrace     = "trace"
	KindSpeed     = "speed_insights"
	KindAnalytics = "web_analytics"
	KindUnknown   = "unknown"
)

// Result is what one drain request turns into.
type Result struct {
	Logs   *collogspb.ExportLogsServiceRequest // nil when there are no log entries
	Traces []*coltracepb.ExportTraceServiceRequest
	RUM    []*rumpb.RumBatch // one batch per (app, env, version, device, os, browser) resource
	Counts map[string]int    // entries per kind
}

// ErrEmpty means the body had no entries.
var ErrEmpty = errors.New("vercel: empty drain body")

// Convert parses a decompressed JSON / NDJSON drain body.
func Convert(in *Integration, body []byte) (*Result, error) {
	entries, err := split(body)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, ErrEmpty
	}
	c := newConverter(in)
	for _, raw := range entries {
		if err := c.add(raw); err != nil {
			return nil, err
		}
	}
	return c.result(), nil
}

// ConvertTraceProto handles a protobuf trace drain body (OTLP/HTTP protobuf).
func ConvertTraceProto(in *Integration, req *coltracepb.ExportTraceServiceRequest) *Result {
	c := newConverter(in)
	c.enrichTraces(req)
	c.res.Traces = append(c.res.Traces, req)
	c.res.Counts[KindTrace]++
	return c.result()
}

// split returns the JSON values of an array, NDJSON stream or single object.
func split(body []byte) ([]json.RawMessage, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	if body[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, fmt.Errorf("vercel: invalid JSON array: %w", err)
		}
		return arr, nil
	}
	var out []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		var m json.RawMessage
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("vercel: invalid JSON/NDJSON: %w", err)
		}
		out = append(out, m)
	}
}

// probe decides the kind of one entry.
type probe struct {
	Schema        string          `json:"schema"`
	ResourceSpans json.RawMessage `json:"resourceSpans"`
	Source        string          `json:"source"`
	EventType     string          `json:"eventType"`
	MetricType    string          `json:"metricType"`
}

type converter struct {
	in       *Integration
	res      *Result
	logRes   map[string]*logspb.ResourceLogs
	rumBatch map[string]*rumpb.RumBatch
}

func newConverter(in *Integration) *converter {
	return &converter{in: in, res: &Result{Counts: map[string]int{}}, logRes: map[string]*logspb.ResourceLogs{}, rumBatch: map[string]*rumpb.RumBatch{}}
}

func (c *converter) add(raw json.RawMessage) error {
	var p probe
	if err := json.Unmarshal(raw, &p); err != nil {
		c.res.Counts[KindUnknown]++
		return nil // not an object (e.g. a stray scalar): ignore
	}
	switch {
	case len(p.ResourceSpans) > 0:
		req, err := traceJSON(raw)
		if err != nil {
			return err
		}
		c.enrichTraces(req)
		c.res.Traces = append(c.res.Traces, req)
		c.res.Counts[KindTrace]++
	case strings.HasPrefix(p.Schema, "vercel.speed_insights") || (p.Schema == "" && p.MetricType != ""):
		var e speedEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("vercel: speed insights entry: %w", err)
		}
		c.addSpeed(&e)
		c.res.Counts[KindSpeed]++
	case strings.HasPrefix(p.Schema, "vercel.analytics") || (p.Schema == "" && p.EventType != ""):
		var e analyticsEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("vercel: analytics entry: %w", err)
		}
		c.addAnalytics(&e)
		c.res.Counts[KindAnalytics]++
	case p.Source != "" || p.Schema == "" || strings.HasPrefix(p.Schema, "vercel.log"):
		var e logEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("vercel: log entry: %w", err)
		}
		c.addLog(&e)
		c.res.Counts[KindLog]++
	default:
		c.res.Counts[KindUnknown]++
	}
	return nil
}

func (c *converter) result() *Result {
	if len(c.logRes) > 0 {
		keys := sortedKeys(c.logRes)
		req := &collogspb.ExportLogsServiceRequest{}
		for _, k := range keys {
			req.ResourceLogs = append(req.ResourceLogs, c.logRes[k])
		}
		c.res.Logs = req
	}
	for _, k := range sortedKeys(c.rumBatch) {
		c.res.RUM = append(c.res.RUM, c.rumBatch[k])
	}
	return c.res
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// service maps a Vercel project (id or name) to a service name: projectMap[id] → projectMap[name] → name → id.
func (c *converter) service(projectID, projectName string) string {
	if s := c.in.Projects[projectID]; s != "" && projectID != "" {
		return s
	}
	if s := c.in.Projects[projectName]; s != "" && projectName != "" {
		return s
	}
	if projectName != "" {
		return projectName
	}
	if projectID != "" {
		return projectID
	}
	if c.in.Name != "" {
		return c.in.Name
	}
	return "vercel"
}

// Route turns a Vercel/Next.js route or path into a route template: "[id]" → ":id",
// "[...slug]" / "[[...slug]]" → "*slug", then the usual id normalization.
func Route(p string) string {
	if p == "" {
		return ""
	}
	if strings.IndexByte(p, '[') >= 0 {
		var b strings.Builder
		for seg := range strings.SplitSeq(p, "/") {
			if seg == "" {
				continue
			}
			b.WriteByte('/')
			b.WriteString(nextSegment(seg))
		}
		p = b.String()
	}
	return norm.Route(p)
}

func nextSegment(seg string) string {
	i := strings.IndexByte(seg, '[')
	if i < 0 {
		return seg
	}
	j := strings.LastIndexByte(seg, ']')
	if j < i {
		return seg
	}
	inner := strings.Trim(seg[i:j+1], "[]")
	if strings.HasPrefix(inner, "...") {
		return seg[:i] + "*" + strings.TrimPrefix(inner, "...") + seg[j+1:]
	}
	return seg[:i] + ":" + inner + seg[j+1:]
}

// ── flexible JSON scalars ──

// flexString accepts a JSON string or number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if string(b) == "null" {
		return nil
	}
	*f = flexString(strings.TrimSpace(string(b)))
	return nil
}

// flexTime accepts unix ms (number or numeric string) or an RFC 3339 string.
type flexTime struct{ t time.Time }

func (f *flexTime) UnmarshalJSON(b []byte) error {
	var s flexString
	if err := s.UnmarshalJSON(b); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	if v, err := strconv.ParseFloat(string(s), 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
		switch {
		case v > 1e17: // ns
			f.t = time.Unix(0, int64(v))
		case v > 1e14: // µs
			f.t = time.UnixMicro(int64(v))
		case v > 1e11: // ms
			f.t = time.UnixMilli(int64(v))
		default: // s
			f.t = time.Unix(int64(v), 0)
		}
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, string(s))
	if err != nil {
		return nil // unknown format: fall back to receive time
	}
	f.t = t
	return nil
}

// ms returns unix ms, or now when unset.
func (f flexTime) ms() int64 {
	if f.t.IsZero() {
		return time.Now().UnixMilli()
	}
	return f.t.UnixMilli()
}

// ── helpers ──

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

type kvs []*commonpb.KeyValue

func (a *kvs) str(k, v string) {
	if v != "" {
		*a = append(*a, strKV(k, v))
	}
}

func (a *kvs) int(k string, v int64) { *a = append(*a, intKV(k, v)) }

func hexBytes(s string, n int) []byte {
	if len(s) != 2*n {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// hashID is a salted, one-way id (Vercel device ids must not be stored as-is).
func hashID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func resource(service string, in *Integration, extra ...*commonpb.KeyValue) *resourcepb.Resource {
	a := kvs{strKV("service.name", service), strKV("cloud.provider", "vercel")}
	if in.ID != 0 {
		a = append(a, strKV("trooper.integration.id", strconv.FormatInt(in.ID, 10)))
	}
	return &resourcepb.Resource{Attributes: append(a, extra...)}
}

// traceJSON decodes an OTLP/JSON trace payload. Vercel's JSON uses lower-case enum names ("kind": "server")
// and hex ids; both are rewritten to what protojson expects.
func traceJSON(raw []byte) (*coltracepb.ExportTraceServiceRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("vercel: trace JSON: %w", err)
	}
	fixOTLP(v, "")
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("vercel: trace JSON: %w", err)
	}
	return &req, nil
}

var spanKinds = map[string]int{"unspecified": 0, "internal": 1, "server": 2, "client": 3, "producer": 4, "consumer": 5}
var statusCodes = map[string]int{"unset": 0, "ok": 1, "error": 2}

func fixOTLP(v any, parent string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			switch k {
			case "traceId", "spanId", "parentSpanId", "trace_id", "span_id", "parent_span_id":
				if s, ok := e.(string); ok && (len(s) == 32 || len(s) == 16) {
					if b, err := hex.DecodeString(s); err == nil {
						x[k] = base64.StdEncoding.EncodeToString(b)
					}
				}
			case "kind":
				if s, ok := e.(string); ok {
					if n, ok := spanKinds[strings.TrimPrefix(strings.ToLower(s), "span_kind_")]; ok {
						x[k] = n
					}
				}
			case "code":
				if s, ok := e.(string); ok && parent == "status" {
					if n, ok := statusCodes[strings.TrimPrefix(strings.ToLower(s), "status_code_")]; ok {
						x[k] = n
					}
				}
			default:
				fixOTLP(e, k)
			}
		}
	case []any:
		for _, e := range x {
			fixOTLP(e, parent)
		}
	}
}

// enrichTraces tags resources as Vercel and maps the service name through projectMap.
func (c *converter) enrichTraces(req *coltracepb.ExportTraceServiceRequest) {
	for _, rs := range req.GetResourceSpans() {
		if rs.Resource == nil {
			rs.Resource = &resourcepb.Resource{}
		}
		var svcKV *commonpb.KeyValue
		var projectID, projectName string
		has := map[string]bool{}
		for _, kv := range rs.Resource.Attributes {
			has[kv.GetKey()] = true
			switch kv.GetKey() {
			case "service.name":
				svcKV = kv
			case "vercel.projectId", "vercel.project_id":
				projectID = kv.GetValue().GetStringValue()
			case "vercel.projectName", "vercel.project_name":
				projectName = kv.GetValue().GetStringValue()
			}
		}
		mapped := c.in.Projects[projectID]
		if mapped == "" && projectName != "" {
			mapped = c.in.Projects[projectName]
		}
		if mapped == "" && svcKV != nil {
			mapped = c.in.Projects[svcKV.GetValue().GetStringValue()]
		}
		switch {
		case mapped != "" && svcKV != nil:
			svcKV.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: mapped}}
		case svcKV == nil:
			rs.Resource.Attributes = append(rs.Resource.Attributes, strKV("service.name", c.service(projectID, projectName)))
		}
		if !has["cloud.provider"] {
			rs.Resource.Attributes = append(rs.Resource.Attributes, strKV("cloud.provider", "vercel"))
		}
		if c.in.ID != 0 && !has["trooper.integration.id"] {
			rs.Resource.Attributes = append(rs.Resource.Attributes, strKV("trooper.integration.id", strconv.FormatInt(c.in.ID, 10)))
		}
	}
}
