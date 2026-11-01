package selftel

import (
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RedactedTransport is a client transport for notification destinations. Unlike otelhttp it never records the
// URL path or query: Slack / Teams / generic webhook URLs carry their secret in the path. Spans have
// http.request.method, server.address/port, url.scheme, url.full (scheme://host only), status code and
// peer.service; traceparent/baggage are injected like any trooper client. base nil = http.DefaultTransport.
func RedactedTransport(base http.RoundTripper, peer string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &redacted{base: base, peer: peer}
}

type redacted struct {
	base http.RoundTripper
	peer string
}

func (t *redacted) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
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
	if t.peer != "" {
		attrs = append(attrs, attribute.String("peer.service", t.peer))
	}
	ctx, span := tracer().Start(ctx, req.Method, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()
	if span.IsRecording() {
		req = req.Clone(ctx)
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	}
	resp, err := t.base.RoundTrip(req)
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
