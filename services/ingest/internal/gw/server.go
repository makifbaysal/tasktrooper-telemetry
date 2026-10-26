// Package gw is ingest-gw: OTLP/HTTP, OTLP/gRPC and RUM endpoints that authenticate, rate-limit,
// validate and publish payloads to Redpanda (contracts §2–3).
package gw

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/auth"
	"github.com/tasktrooper/telemetry/services/ingest/internal/obs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/ratelimit"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// Config holds ingest-gw settings.
type Config struct {
	HTTPAddr       string
	GRPCAddr       string
	MaxBodyBytes   int64 // request limit after decompression (413 above)
	MaxRecordBytes int   // Kafka record limit; larger requests are split
	DefaultRPS     int   // per-project request rate when the key has no rate_limit_rps (≤0 disables)
	ServeMetrics   bool  // expose Prometheus /metrics on the HTTP port (when TT_INGEST_METRICS_ADDR is unset)
}

// RateLimiter is satisfied by *ratelimit.Limiter.
type RateLimiter interface {
	Allow(ctx context.Context, project uint32, rps int) (bool, time.Duration)
}

// Server serves the ingest endpoints.
type Server struct {
	cfg          Config
	keys         auth.Lookup
	lim          RateLimiter
	sink         Sink
	log          *slog.Logger
	now          func() time.Time
	integrations Integrations // Vercel drains; nil → /v1/drains/* answers 503
}

// New builds a Server.
func New(cfg Config, keys auth.Lookup, lim RateLimiter, sink Sink, log *slog.Logger) *Server {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 5 << 20
	}
	if cfg.MaxRecordBytes <= 0 {
		cfg.MaxRecordBytes = 900 << 10
	}
	return &Server{cfg: cfg, keys: keys, lim: lim, sink: sink, log: log, now: time.Now}
}

// signal describes one ingest endpoint.
type signal struct {
	topic  string
	rum    bool
	cors   bool
	newMsg func() proto.Message
	split  func(m proto.Message, max int) []proto.Message
}

var (
	sigTraces = signal{topics.Traces, false, true,
		func() proto.Message { return &coltracepb.ExportTraceServiceRequest{} },
		func(m proto.Message, max int) []proto.Message {
			return toMsgs(splitTraces(m.(*coltracepb.ExportTraceServiceRequest), max))
		}}
	sigMetrics = signal{topics.Metrics, false, false,
		func() proto.Message { return &colmetricspb.ExportMetricsServiceRequest{} },
		func(m proto.Message, max int) []proto.Message {
			return toMsgs(splitMetrics(m.(*colmetricspb.ExportMetricsServiceRequest), max))
		}}
	sigLogs = signal{topics.Logs, false, true,
		func() proto.Message { return &collogspb.ExportLogsServiceRequest{} },
		func(m proto.Message, max int) []proto.Message {
			return toMsgs(splitLogs(m.(*collogspb.ExportLogsServiceRequest), max))
		}}
	sigRUM = signal{topics.RUM, true, true,
		func() proto.Message { return &rumpb.RumBatch{} },
		func(m proto.Message, max int) []proto.Message { return toMsgs(splitRUM(m.(*rumpb.RumBatch), max)) }}
)

func toMsgs[T proto.Message](in []T) []proto.Message {
	out := make([]proto.Message, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("/v1/traces", s.endpoint(sigTraces))
	mux.Handle("/v1/metrics", s.endpoint(sigMetrics))
	mux.Handle("/v1/logs", s.endpoint(sigLogs))
	mux.Handle("/v1/rum", s.endpoint(sigRUM))
	mux.Handle("/v1/drains/vercel", s.vercelDrain())
	if s.cfg.ServeMetrics {
		mux.Handle("/metrics", obs.Handler())
	}
	return mux
}

// meta is what every published record carries in its headers.
type meta struct {
	tenant     tenant.Tenant
	receivedAt time.Time
	clientIP   string
	userAgent  string // RUM only
}

// ingestError maps to HTTP and gRPC status.
type ingestError struct {
	status     int
	msg        string
	retryAfter int    // seconds, 429/503
	authReason string // set on auth failures (metrics label)
}

func (e *ingestError) Error() string { return e.msg }

var (
	errUnauthorized = &ingestError{status: http.StatusUnauthorized, msg: "missing or invalid ingest key", authReason: "invalid_key"}
	errForbidden    = &ingestError{status: http.StatusForbidden, msg: "origin not allowed for this key", authReason: "origin"}
	errUnavailable  = &ingestError{status: http.StatusServiceUnavailable, msg: "ingest buffer full, retry later", retryAfter: 1}
)

func (s *Server) endpoint(sig signal) http.Handler {
	return instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && sig.cors {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "false")
			h.Set("Access-Control-Expose-Headers", "Retry-After")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding, X-Trooper-Key, Authorization, Traceparent, Tracestate")
				h.Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jsonReq := isJSON(r.Header.Get("Content-Type"), sig.rum)
		if err := s.handle(r, sig, origin, jsonReq); err != nil {
			if errors.Is(err, errForbidden) {
				w.Header().Del("Access-Control-Allow-Origin") // origin not in the key's allowed_origins
			}
			s.writeError(w, err)
			return
		}
		if jsonReq {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}
		// Empty Export*ServiceResponse encodes to zero bytes.
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
}

func (s *Server) handle(r *http.Request, sig signal, origin string, jsonReq bool) error {
	raw := requestKey(r, sig.rum)
	key, err := s.keys.Lookup(r.Context(), raw)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidKey) {
			return errUnauthorized
		}
		s.log.Error("key lookup failed", "err", err)
		return &ingestError{status: http.StatusServiceUnavailable, msg: "auth backend unavailable", retryAfter: 5}
	}
	if origin != "" && !key.OriginAllowed(origin) {
		return errForbidden
	}
	if err := s.allow(r.Context(), key); err != nil {
		return err
	}
	body, err := readBody(r, s.cfg.MaxBodyBytes)
	switch {
	case errors.Is(err, errTooLarge):
		return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "body exceeds " + strconv.FormatInt(s.cfg.MaxBodyBytes, 10) + " bytes"}
	case errors.Is(err, errBadEncoding):
		return &ingestError{status: http.StatusUnsupportedMediaType, msg: err.Error()}
	case err != nil:
		return &ingestError{status: http.StatusBadRequest, msg: "read body: " + err.Error()}
	}
	m := meta{tenant: key.Tenant, receivedAt: s.now(), clientIP: clientIP(r)}
	if sig.rum {
		m.userAgent = r.Header.Get("User-Agent")
	}
	return s.publishBody(sig, body, jsonReq, m)
}

func (s *Server) allow(ctx context.Context, key *auth.Key) error {
	rps := key.RateLimitRPS
	if rps <= 0 {
		rps = s.cfg.DefaultRPS
	}
	if s.lim == nil || rps <= 0 {
		return nil
	}
	if ok, wait := s.lim.Allow(ctx, key.Tenant.ProjectID, rps); !ok {
		return &ingestError{status: http.StatusTooManyRequests, msg: "rate limit exceeded", retryAfter: ratelimit.RetryAfterSeconds(wait)}
	}
	return nil
}

// publishBody validates the payload and publishes it. Protobuf bodies within the record limit are
// forwarded byte-for-byte; JSON is converted to protobuf; oversized payloads are split.
func (s *Server) publishBody(sig signal, body []byte, jsonReq bool, m meta) error {
	msg := sig.newMsg()
	var err error
	switch {
	case jsonReq && sig.rum:
		err = jsonUnmarshal.Unmarshal(body, msg)
	case jsonReq:
		err = unmarshalOTLPJSON(body, msg)
	default:
		err = proto.Unmarshal(body, msg)
	}
	if err != nil {
		return &ingestError{status: http.StatusBadRequest, msg: "invalid payload: " + err.Error()}
	}
	if !jsonReq && len(body) <= s.cfg.MaxRecordBytes {
		if isEmpty(msg) {
			return nil
		}
		return s.send(sig.topic, [][]byte{body}, m)
	}
	return s.publishMsg(sig, msg, m)
}

// publishMsg marshals (and splits if needed) a decoded message.
func (s *Server) publishMsg(sig signal, msg proto.Message, m meta) error {
	if isEmpty(msg) {
		return nil
	}
	parts := []proto.Message{msg}
	if proto.Size(msg) > s.cfg.MaxRecordBytes {
		parts = sig.split(msg, s.cfg.MaxRecordBytes)
	}
	values := make([][]byte, 0, len(parts))
	for _, p := range parts {
		b, err := proto.Marshal(p)
		if err != nil {
			return &ingestError{status: http.StatusBadRequest, msg: "encode: " + err.Error()}
		}
		values = append(values, b)
	}
	return s.send(sig.topic, values, m)
}

func isEmpty(m proto.Message) bool {
	switch x := m.(type) {
	case *coltracepb.ExportTraceServiceRequest:
		return len(x.GetResourceSpans()) == 0
	case *colmetricspb.ExportMetricsServiceRequest:
		return len(x.GetResourceMetrics()) == 0
	case *collogspb.ExportLogsServiceRequest:
		return len(x.GetResourceLogs()) == 0
	case *rumpb.RumBatch:
		return len(x.GetEvents()) == 0
	}
	return false
}

func (s *Server) send(topic string, values [][]byte, m meta) error {
	org, project := m.tenant.Headers()
	headers := []kgo.RecordHeader{
		{Key: topics.HeaderOrgID, Value: []byte(org)},
		{Key: topics.HeaderProjectID, Value: []byte(project)},
		{Key: topics.HeaderReceivedAt, Value: headerValue(m.receivedAt.UnixMilli())},
	}
	if m.clientIP != "" {
		headers = append(headers, kgo.RecordHeader{Key: topics.HeaderClientIP, Value: []byte(m.clientIP)})
	}
	if m.userAgent != "" {
		headers = append(headers, kgo.RecordHeader{Key: topics.HeaderUserAgent, Value: []byte(m.userAgent)})
	}
	key := []byte(project)
	for _, v := range values {
		if err := s.sink.Send(&kgo.Record{Topic: topic, Key: key, Value: v, Headers: headers}); err != nil {
			obs.GWProduceErrors.WithLabelValues(topic, "buffer_full").Inc()
			if errors.Is(err, ErrBufferFull) {
				return errUnavailable
			}
			s.log.Error("produce enqueue failed", "topic", topic, "err", err)
			return errUnavailable
		}
	}
	return nil
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	var ie *ingestError
	if !errors.As(err, &ie) {
		ie = &ingestError{status: http.StatusInternalServerError, msg: err.Error()}
	}
	if sw, ok := w.(*statusWriter); ok {
		sw.err = ie
	}
	if ie.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(ie.retryAfter))
	}
	http.Error(w, ie.msg, ie.status)
}

// requestKey extracts the ingest key: x-trooper-key, Authorization: Bearer, or ?key= (RUM only, for sendBeacon).
func requestKey(r *http.Request, allowQuery bool) string {
	if k := r.Header.Get("X-Trooper-Key"); k != "" {
		return strings.TrimSpace(k)
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if allowQuery {
		return r.URL.Query().Get("key")
	}
	return ""
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
