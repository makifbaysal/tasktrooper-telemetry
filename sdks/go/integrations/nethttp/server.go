package trooperhttp

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

type config struct {
	panicPolicy  trooper.PanicPolicy
	resolver     func(*http.Request) string
	profileRoute func(*http.Request) string
	otelOpts     []otelhttp.Option
}

// Option configures Middleware.
type Option func(*config)

// WithPanicPolicy sets what happens after a panic is recorded. Default trooper.PanicRepanic
// (net/http's server recovers and logs it); trooper.PanicRecover responds 500.
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.panicPolicy = p } }

// WithRouteResolver returns the route template for a request after (or while) it is routed.
// The default reads r.Pattern set by Go 1.22+ http.ServeMux. The resolver receives the request
// passed to the wrapped handler; it is called lazily when child spans start and once after the
// handler returns.
func WithRouteResolver(f func(*http.Request) string) Option {
	return func(c *config) { c.resolver = f }
}

// WithProfileRouteResolver returns the route template of a request *before* the wrapped handler
// routes it. It is used only for the pprof goroutine label trooper.route while continuous
// profiling runs (trooper.WithProfiling), because CPU samples are taken while the handler runs and
// the regular resolver only knows the route afterwards. When next is an *http.ServeMux the default
// asks the mux (ServeMux.Handler) — one extra route match per request, only while profiling.
// Without a pre-route (other routers) the goroutine is labelled with trooper.method only.
func WithProfileRouteResolver(f func(*http.Request) string) Option {
	return func(c *config) { c.profileRoute = f }
}

// WithOtelHTTPOptions passes options to the underlying otelhttp handler.
func WithOtelHTTPOptions(opts ...otelhttp.Option) Option {
	return func(c *config) { c.otelOpts = append(c.otelOpts, opts...) }
}

// Middleware returns net/http middleware that creates a server span (via otelhttp) and adds
// trooper.route, trooper.entry.route/method (on all child spans), trooper.caller and panic capture.
func Middleware(opts ...Option) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return Handler(next, opts...) }
}

// Handler wraps next (typically an *http.ServeMux) with Middleware.
func Handler(next http.Handler, opts ...Option) http.Handler {
	c := &config{resolver: PatternRoute}
	for _, o := range opts {
		o(c)
	}
	if c.profileRoute == nil {
		if mux, ok := next.(*http.ServeMux); ok {
			c.profileRoute = func(r *http.Request) string {
				_, pattern := mux.Handler(r)
				return StripPattern(pattern)
			}
		}
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var r2 *http.Request
		ctx, entry := trooper.BeginEntry(ctx, r.Method, "", func() string { return c.resolver(r2) })
		if c.profileRoute != nil && trooper.ProfileLabelsEnabled() {
			ctx = entry.LabelRoute(ctx, safeRoute(c.profileRoute, r.WithContext(ctx)))
		}
		r2 = r.WithContext(ctx)
		span := trace.SpanFromContext(ctx)
		finish := func() {
			route := ""
			func() {
				defer func() { _ = recover() }()
				route = c.resolver(r2)
			}()
			entry.Finish(route)
			if route != "" {
				trooper.SetServerRoute(span, route)
				span.SetName(r.Method + " " + route)
			}
			// Let otelhttp see the pattern (http.route on metrics).
			if r.Pattern == "" {
				r.Pattern = r2.Pattern
			}
		}
		defer func() {
			if rec := recover(); rec != nil {
				trooper.RecordPanic(ctx, rec)
				finish()
				if c.panicPolicy == trooper.PanicRepanic || rec == http.ErrAbortHandler {
					panic(rec)
				}
				span.SetAttributes(attribute.Int("http.response.status_code", http.StatusInternalServerError))
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r2)
		finish()
	})
	return otelhttp.NewHandler(inner, "http.server", c.otelOpts...)
}

func safeRoute(f func(*http.Request) string, r *http.Request) (route string) {
	defer func() {
		if recover() != nil {
			route = ""
		}
	}()
	return f(r)
}

// PatternRoute returns the path part of r.Pattern (Go 1.22+ ServeMux), stripping the method and
// host: "GET example.com/users/{id}" -> "/users/{id}".
func PatternRoute(r *http.Request) string {
	if r == nil {
		return ""
	}
	return StripPattern(r.Pattern)
}

// StripPattern strips the "METHOD " prefix and host from a ServeMux pattern.
func StripPattern(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = strings.TrimLeft(p[i+1:], " \t")
	}
	if !strings.HasPrefix(p, "/") {
		if i := strings.IndexByte(p, '/'); i >= 0 {
			p = p[i:]
		} else {
			return ""
		}
	}
	return p
}
