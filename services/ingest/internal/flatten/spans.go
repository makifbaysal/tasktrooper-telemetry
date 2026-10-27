package flatten

import (
	"strconv"
	"strings"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tasktrooper/telemetry/services/ingest/internal/norm"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// SpanRow is one row of telemetry.spans.
type SpanRow struct {
	OrgID, ProjectID          uint32
	TS                        time.Time
	DurationNs                uint64
	TraceID, SpanID, ParentID []byte
	Service, Env, Version     string
	Kind                      string
	Name                      string
	IsEntry                   uint8
	SampleWeight              float32
	Route, HTTPMethod         string
	StatusCode                uint16
	IsError                   uint8
	ErrorType                 string
	ErrorFingerprint          uint64
	EntryRoute, EntryMethod   string
	Caller, CallerKind        string
	Dep, DepKind, DepService  string
	Topic, ConsumerGroup      string
	MessagingSystem           string
	Attempt                   uint16
	DLQ                       uint8
	DBQueryHash               uint64
	Attrs, ResourceAttrs      map[string]string
	EventTS                   []time.Time
	EventName                 []string
	EventAttrs                []map[string]string

	kind    tracepb.Span_SpanKind // for in-batch entry-route inheritance
	hasEntR bool                  // entry route came from the SDK or the span itself
}

// SpanColumns is the INSERT column list matching SpanRow.Values.
const SpanColumns = "org_id, project_id, ts, duration_ns, trace_id, span_id, parent_span_id, service, env, version, kind, name, " +
	"is_entry, sample_weight, route, http_method, status_code, is_error, error_type, error_fingerprint, entry_route, entry_method, " +
	"caller, caller_kind, dep, dep_kind, dep_service, topic, messaging_system, consumer_group, attempt, dlq, db_query_hash, attrs, resource_attrs, " +
	"events.ts, events.name, events.attrs"

// Values returns the row in SpanColumns order.
func (r *SpanRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.TS, r.DurationNs, r.TraceID, r.SpanID, r.ParentID, r.Service, r.Env, r.Version,
		r.Kind, r.Name, r.IsEntry, r.SampleWeight, r.Route, r.HTTPMethod, r.StatusCode, r.IsError, r.ErrorType,
		r.ErrorFingerprint, r.EntryRoute, r.EntryMethod, r.Caller, r.CallerKind, r.Dep, r.DepKind, r.DepService, r.Topic,
		r.MessagingSystem, r.ConsumerGroup, r.Attempt, r.DLQ, r.DBQueryHash, r.Attrs, r.ResourceAttrs, r.EventTS, r.EventName, r.EventAttrs}
}

// resourceInfo is what every span/log/metric of one OTLP Resource shares.
type resourceInfo struct {
	service, env, version string
	attrs                 map[string]string
	sampleRate            float64
}

func newResource(kvs []*commonpb.KeyValue) resourceInfo {
	a := attrIndex(kvs)
	r := resourceInfo{
		service: a.str("service.name"),
		env:     a.first("deployment.environment.name", "deployment.environment"),
		version: a.first("service.version", "trooper.deploy.version"),
		attrs:   attrMap(kvs, 0),
	}
	if r.service == "" {
		r.service = "unknown_service"
	}
	r.sampleRate, _ = a.float("trooper.sample_rate")
	return r
}

var kindNames = [...]string{"internal", "internal", "server", "client", "producer", "consumer"}

func kindName(k tracepb.Span_SpanKind) string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "internal"
}

// Spans appends one SpanRow per OTLP span in req to out (LLM content is dropped; see SpansWithContent).
func Spans(t tenant.Tenant, req *coltracepb.ExportTraceServiceRequest, out []SpanRow) []SpanRow {
	out, _ = SpansWithContent(t, req, out, nil)
	return out
}

// SpansWithContent is Spans that also appends the prompt / completion content of LLM spans (contracts §25) to content.
func SpansWithContent(t tenant.Tenant, req *coltracepb.ExportTraceServiceRequest, out []SpanRow, content []LLMContentRow) ([]SpanRow, []LLMContentRow) {
	for _, rs := range req.GetResourceSpans() {
		res := newResource(rs.GetResource().GetAttributes())
		start := len(out)
		n := countSpans(rs)
		var byID map[[8]byte]int
		if n > 1 {
			byID = make(map[[8]byte]int, n)
			i := start
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					if id := sp.GetSpanId(); len(id) == 8 {
						byID[[8]byte(id)] = i
					}
					i++
				}
			}
		}
		spans := make([]*tracepb.Span, 0, n)
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				spans = append(spans, sp)
			}
		}
		for _, sp := range spans {
			row, c := spanRow(t, &res, sp, spans, byID, start)
			out = append(out, row)
			if c != nil {
				content = append(content, *c)
			}
		}
		if byID != nil {
			inheritEntryRoute(out[start:], byID, start)
		}
	}
	return out, content
}

func countSpans(rs *tracepb.ResourceSpans) int {
	n := 0
	for _, ss := range rs.GetScopeSpans() {
		n += len(ss.GetSpans())
	}
	return n
}

func spanRow(t tenant.Tenant, res *resourceInfo, sp *tracepb.Span, batch []*tracepb.Span, byID map[[8]byte]int, base int) (SpanRow, *LLMContentRow) {
	a := attrIndex(sp.GetAttributes())
	genAI := hasGenAIKeys(sp.GetAttributes())
	var llm llmInfo
	if genAI {
		llm = detectLLM(a, sp.GetName())
	}
	k := sp.GetKind()
	r := SpanRow{
		OrgID: t.OrgID, ProjectID: t.ProjectID,
		TraceID:  fixedID(sp.GetTraceId(), 16),
		SpanID:   fixedID(sp.GetSpanId(), 8),
		ParentID: fixedID(sp.GetParentSpanId(), 8),
		Service:  res.service, Env: res.env, Version: res.version,
		Kind: kindName(k), Name: sp.GetName(),
		ResourceAttrs: res.attrs,
		SampleWeight:  1,
		kind:          k,
	}
	st, et := sp.GetStartTimeUnixNano(), sp.GetEndTimeUnixNano()
	r.TS = time.Unix(0, int64(st)).UTC()
	if et > st {
		r.DurationNs = et - st
	}

	rate, ok := a.float("trooper.sample_rate")
	if !ok {
		rate = res.sampleRate
	}
	if rate > 0 && rate < 1 {
		r.SampleWeight = float32(1 / rate)
	}

	r.HTTPMethod = strings.ToUpper(a.first("http.request.method", "http.method"))
	if sc, ok := a.int("http.response.status_code"); ok {
		r.StatusCode = uint16(sc)
	} else if sc, ok := a.int("http.status_code"); ok {
		r.StatusCode = uint16(sc)
	}

	isServer := k == tracepb.Span_SPAN_KIND_SERVER
	isConsumer := k == tracepb.Span_SPAN_KIND_CONSUMER
	if isServer || isConsumer {
		if isEntry(sp, batch, byID, base) {
			r.IsEntry = 1
		}
	}

	switch {
	case isServer:
		r.Route = serverRoute(a, sp.GetName())
	case isConsumer:
		r.Route = a.first("trooper.process", "messaging.consumer.group.name", "messaging.kafka.consumer.group", "messaging.destination.name")
		if r.Route == "" {
			r.Route = sp.GetName()
		}
	}

	r.EntryRoute = a.str("trooper.entry.route")
	r.EntryMethod = strings.ToUpper(a.str("trooper.entry.method"))
	if r.IsEntry == 1 {
		if r.HTTPMethod == "" {
			r.HTTPMethod = r.EntryMethod // SDKs that only set trooper.entry.* (e.g. manual BeginEntry)
		}
		r.EntryRoute, r.EntryMethod = r.Route, r.HTTPMethod
	}
	r.hasEntR = r.EntryRoute != ""
	if !isServer && !isConsumer {
		r.Route = r.EntryRoute // non-entry spans are ordered under the request they belong to
	}

	if r.IsEntry == 1 && isServer {
		if c := a.str("trooper.caller"); c != "" {
			r.Caller, r.CallerKind = c, norm.CallerService
		} else {
			r.Caller, r.CallerKind = norm.ClassifyCaller(a.first("user_agent.original", "http.user_agent"))
		}
	}

	// Messaging.
	if isConsumer || k == tracepb.Span_SPAN_KIND_PRODUCER || a.get("messaging.system") != nil {
		r.Topic = a.first("messaging.destination.name", "trooper.consumer.topic")
		r.MessagingSystem = MessagingSystem(a.str("messaging.system"))
		r.ConsumerGroup = a.first("messaging.consumer.group.name", "messaging.kafka.consumer.group", "trooper.consumer.group")
		if n, ok := a.int("trooper.consumer.attempt"); ok && n > 0 {
			r.Attempt = uint16(min(n, 65535))
		}
		if a.bool("trooper.consumer.dlq") {
			r.DLQ = 1
		}
	}

	// Dependencies.
	if k == tracepb.Span_SPAN_KIND_CLIENT || k == tracepb.Span_SPAN_KIND_PRODUCER {
		r.Dep, r.DepKind, r.DepService = depOf(a)
		// The HTTP call under an LLM span (e.g. "HTTP · api.openai.com") is the same time as its "ai" dependency.
		if r.DepKind == "http" && parentIsModelCall(sp, batch, byID, base) {
			r.Dep, r.DepKind, r.DepService = "", "", ""
		}
	}
	// LLM model calls are dependencies of kind "ai" whatever their span kind (contracts §25).
	if isModelCall(llm.op) {
		r.Dep, r.DepKind, r.DepService = llmDep(llm.provider, llm.model), "ai", ""
	}

	var content *LLMContentRow
	if genAI {
		var c LLMContentRow
		r.Attrs, c = genAIAttrs(sp.GetAttributes(), a, llm, 4)
		if !c.empty() {
			c.OrgID, c.ProjectID, c.TS, c.TraceID, c.SpanID, c.Service = r.OrgID, r.ProjectID, r.TS, r.TraceID, r.SpanID, r.Service
			content = &c
		}
	} else {
		r.Attrs = attrMap(sp.GetAttributes(), 4)
	}

	if q := a.first("db.query.text", "db.statement"); q != "" {
		nq := norm.Query(q)
		r.DBQueryHash = norm.QueryHash(nq)
		r.Attrs["db.query.text"] = capValue(nq, MaxAttrValueLen) // never store literals
		delete(r.Attrs, "db.statement")
	}

	// Events (+ exception details).
	var excType, excMsg, excStack string
	if evs := sp.GetEvents(); len(evs) > 0 {
		r.EventTS = make([]time.Time, len(evs))
		r.EventName = make([]string, len(evs))
		r.EventAttrs = make([]map[string]string, len(evs))
		for i, ev := range evs {
			r.EventTS[i] = time.Unix(0, int64(ev.GetTimeUnixNano())).UTC()
			r.EventName[i] = ev.GetName()
			m := make(map[string]string, len(ev.GetAttributes()))
			for _, kv := range ev.GetAttributes() {
				if len(m) >= MaxAttrKeys {
					break
				}
				limit := MaxAttrValueLen
				if kv.GetKey() == "exception.stacktrace" {
					limit = maxStackLen
				}
				m[kv.GetKey()] = capValue(anyString(kv.GetValue()), limit)
			}
			r.EventAttrs[i] = m
			if ev.GetName() == "exception" && excType == "" && excMsg == "" {
				ea := attrIndex(ev.GetAttributes())
				excType, excMsg, excStack = ea.str("exception.type"), ea.str("exception.message"), ea.str("exception.stacktrace")
			}
		}
	} else {
		r.EventTS, r.EventName, r.EventAttrs = []time.Time{}, []string{}, []map[string]string{}
	}
	if excType == "" && excMsg == "" {
		excType, excMsg, excStack = a.str("exception.type"), a.str("exception.message"), a.str("exception.stacktrace")
	}

	// Errors.
	status := sp.GetStatus()
	if status.GetCode() == tracepb.Status_STATUS_CODE_ERROR || (r.StatusCode >= 500 && k != tracepb.Span_SPAN_KIND_INTERNAL) {
		r.IsError = 1
	}
	if msg := status.GetMessage(); msg != "" {
		putAttr(r.Attrs, "otel.status_description", msg, MaxAttrValueLen)
	}
	if r.IsError == 1 {
		r.ErrorType = excType
		if r.ErrorType == "" {
			r.ErrorType = a.str("error.type")
		}
		if r.ErrorType == "" && r.StatusCode >= 400 {
			r.ErrorType = "HTTP " + strconv.Itoa(int(r.StatusCode))
		}
		msg := excMsg
		if msg == "" {
			msg = status.GetMessage()
		}
		putAttr(r.Attrs, "exception.type", r.ErrorType, MaxAttrValueLen)
		putAttr(r.Attrs, "exception.message", msg, MaxAttrValueLen)
		if msg == "" && excStack == "" {
			// No details (e.g. bare 5xx): group by where it happened.
			msg = r.Name + " " + r.Route
		}
		r.ErrorFingerprint = norm.ErrorFingerprint(r.Service, r.ErrorType, msg, excStack)
	}
	return r, content
}

// parentIsModelCall reports whether sp's parent (in the same resource batch) is an LLM model-call span.
func parentIsModelCall(sp *tracepb.Span, batch []*tracepb.Span, byID map[[8]byte]int, base int) bool {
	parent := sp.GetParentSpanId()
	if byID == nil || len(parent) != 8 {
		return false
	}
	i, ok := byID[[8]byte(parent)]
	if !ok {
		return false
	}
	p := batch[i-base]
	if !hasGenAIKeys(p.GetAttributes()) {
		return false
	}
	return isModelCall(detectLLM(attrIndex(p.GetAttributes()), p.GetName()).op)
}

// serverRoute: trooper.route → http.route → normalized url.path / http.target → rpc service/method → span name.
func serverRoute(a attrIndex, name string) string {
	if r := a.first("trooper.route", "http.route", "trooper.entry.route"); r != "" {
		return r
	}
	if p := a.first("url.path", "http.target"); p != "" {
		return norm.Route(p)
	}
	if u := a.str("url.full"); u != "" {
		return norm.Route(u)
	}
	if svc := a.str("rpc.service"); svc != "" {
		return "/" + svc + "/" + a.str("rpc.method")
	}
	return name
}

// isEntry: a server/consumer span whose parent is missing or lives in another service.
// Uses the OTLP remote-parent flag when present; otherwise, if the parent is in the same
// resource batch and is not an outgoing call, the span is nested (not an entry).
func isEntry(sp *tracepb.Span, batch []*tracepb.Span, byID map[[8]byte]int, base int) bool {
	parent := sp.GetParentSpanId()
	if len(parent) == 0 || isZero(parent) {
		return true
	}
	flags := sp.GetFlags()
	if flags&uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK) != 0 {
		return flags&uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK) != 0
	}
	if byID != nil && len(parent) == 8 {
		if i, ok := byID[[8]byte(parent)]; ok {
			pk := batch[i-base].GetKind()
			return pk == tracepb.Span_SPAN_KIND_CLIENT || pk == tracepb.Span_SPAN_KIND_PRODUCER
		}
	}
	return true
}

// inheritEntryRoute fills entry_route/entry_method (and route for non-entry spans) of spans the SDK didn't
// tag, by walking parents inside the same resource batch up to the nearest entry span.
func inheritEntryRoute(rows []SpanRow, byID map[[8]byte]int, base int) {
	for i := range rows {
		r := &rows[i]
		if r.hasEntR {
			continue
		}
		p := r.ParentID
		for depth := 0; depth < 64 && !isZero(p); depth++ {
			j, ok := byID[[8]byte(p)]
			if !ok {
				break
			}
			pr := &rows[j-base]
			if pr.hasEntR {
				r.EntryRoute, r.EntryMethod = pr.EntryRoute, pr.EntryMethod
				break
			}
			p = pr.ParentID
		}
		if r.EntryRoute != "" && r.kind != tracepb.Span_SPAN_KIND_SERVER && r.kind != tracepb.Span_SPAN_KIND_CONSUMER {
			r.Route = r.EntryRoute
		}
	}
}

var cacheSystems = map[string]bool{"redis": true, "memcached": true, "valkey": true, "hazelcast": true, "aerospike": true}

// depOf names the dependency of a client/producer span per contracts §4.
func depOf(a attrIndex) (dep, kind, svc string) {
	svc = a.str("peer.service")
	if sys := a.first("db.system.name", "db.system"); sys != "" {
		target := a.first("db.namespace", "db.name", "server.address", "net.peer.name")
		kind = "db"
		if cacheSystems[strings.ToLower(sys)] {
			kind = "cache"
			if t := a.first("server.address", "net.peer.name"); t != "" {
				target = t
			}
		}
		return join(sys, target), kind, svc
	}
	if sys := a.str("messaging.system"); sys != "" {
		return join(sys, a.first("messaging.destination.name", "trooper.consumer.topic")), "queue", svc
	}
	isHTTP := a.get("http.request.method") != nil || a.get("http.method") != nil || a.get("url.full") != nil || a.get("http.url") != nil
	rpcSys := a.str("rpc.system")
	if isHTTP || rpcSys != "" {
		target := svc
		if target == "" {
			target = a.first("server.address", "net.peer.name", "http.host")
		}
		if target == "" {
			target = hostOf(a.first("url.full", "http.url"))
		}
		if target == "" && rpcSys != "" {
			target = a.str("rpc.service")
		}
		prefix := "HTTP"
		if rpcSys == "grpc" {
			prefix = "gRPC"
		}
		return join(prefix, target), "http", svc
	}
	return "", "", svc
}

func join(a, b string) string {
	if b == "" {
		return a
	}
	return a + " · " + b
}

func hostOf(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return ""
	}
	h := u[i+3:]
	if j := strings.IndexAny(h, "/?#"); j >= 0 {
		h = h[:j]
	}
	if k := strings.LastIndexByte(h, '@'); k >= 0 {
		h = h[k+1:]
	}
	if c := strings.LastIndexByte(h, ':'); c >= 0 && !strings.Contains(h[c:], "]") {
		h = h[:c]
	}
	return h
}

// MessagingSystem normalizes messaging.system to the values the UI and rollups use (contracts §5):
// kafka | rabbitmq | aws_sqs | gcp_pubsub | aws_sns | servicebus | eventhubs | nats | …
// Unknown systems are lower-cased with '.', '-' and ' ' mapped to '_'.
func MessagingSystem(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	s = strings.NewReplacer(".", "_", "-", "_", " ", "_", "/", "_").Replace(s)
	switch s {
	case "kafka", "apache_kafka", "msk", "aws_msk", "confluent", "redpanda":
		return "kafka"
	case "rabbitmq", "rabbit", "amqp", "amqp091", "amqp_0_9_1":
		return "rabbitmq"
	case "aws_sqs", "sqs", "amazon_sqs", "amazonsqs":
		return "aws_sqs"
	case "gcp_pubsub", "pubsub", "google_pubsub", "gcp_pub_sub", "google_cloud_pubsub", "googlepubsub":
		return "gcp_pubsub"
	case "aws_sns", "sns", "amazon_sns":
		return "aws_sns"
	case "servicebus", "azure_servicebus", "azure_service_bus", "service_bus":
		return "servicebus"
	case "eventhubs", "azure_eventhubs", "event_hubs":
		return "eventhubs"
	case "nats", "nats_jetstream", "jetstream":
		return "nats"
	}
	return s
}
