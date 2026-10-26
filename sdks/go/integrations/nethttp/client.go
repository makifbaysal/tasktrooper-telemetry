package trooperhttp

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type clientConfig struct {
	peerService string
	otelOpts    []otelhttp.Option
}

// ClientOption configures NewTransport.
type ClientOption func(*clientConfig)

// WithPeerService sets peer.service on client spans (dependency name "HTTP · <name>").
func WithPeerService(name string) ClientOption {
	return func(c *clientConfig) { c.peerService = name }
}

// WithClientOtelHTTPOptions passes options to the underlying otelhttp transport.
func WithClientOtelHTTPOptions(opts ...otelhttp.Option) ClientOption {
	return func(c *clientConfig) { c.otelOpts = append(c.otelOpts, opts...) }
}

// NewTransport wraps base (nil = http.DefaultTransport) so every outgoing request gets a client
// span and carries traceparent + baggage (tt.svc=<this service>, via the global trooper propagator).
// Pass the request context (http.NewRequestWithContext) so the span joins the current trace.
func NewTransport(base http.RoundTripper, opts ...ClientOption) http.RoundTripper {
	c := &clientConfig{}
	for _, o := range opts {
		o(c)
	}
	if base == nil {
		base = http.DefaultTransport
	}
	oo := c.otelOpts
	if c.peerService != "" {
		oo = append([]otelhttp.Option{otelhttp.WithSpanOptions(trace.WithAttributes(attribute.String("peer.service", c.peerService)))}, oo...)
	}
	return otelhttp.NewTransport(base, oo...)
}

// NewClient returns an *http.Client using NewTransport(nil, opts...).
func NewClient(opts ...ClientOption) *http.Client {
	return &http.Client{Transport: NewTransport(nil, opts...)}
}
