package selfobs

import (
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Doer is the HTTP client interface of the AWS SDK (s3.HTTPClient).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// HTTPClient wraps base with one client span per request (peer.service = peer, e.g. "s3"). Spans start only under a
// recording parent (a traced flush): startup calls and self-only flushes stay silent. The span records the method,
// scheme://host only (object keys carry session ids) and the status code. No trace headers are injected (signed
// requests stay byte-for-byte what the SDK built).
func HTTPClient(base Doer, peer string) Doer { return &tracedDoer{base: base, peer: peer} }

type tracedDoer struct {
	base Doer
	peer string
}

func (d *tracedDoer) Do(req *http.Request) (*http.Response, error) {
	parent := trace.SpanFromContext(req.Context())
	if !parent.IsRecording() {
		return d.base.Do(req)
	}
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", req.Method),
		attribute.String("url.scheme", req.URL.Scheme),
		attribute.String("url.full", req.URL.Scheme+"://"+req.URL.Host),
		attribute.String("server.address", req.URL.Hostname()),
	}
	if p := req.URL.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			attrs = append(attrs, attribute.Int("server.port", n))
		}
	}
	if d.peer != "" {
		attrs = append(attrs, attribute.String("peer.service", d.peer))
	}
	_, span := parent.TracerProvider().Tracer(ScopeName).Start(req.Context(), req.Method,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()
	resp, err := d.base.Do(req)
	if err != nil {
		span.SetAttributes(attribute.String("error.type", "network"))
		span.SetStatus(codes.Error, err.Error())
		return resp, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetAttributes(attribute.String("error.type", strconv.Itoa(resp.StatusCode)))
		span.SetStatus(codes.Error, "")
	}
	return resp, nil
}
