// Package trooperchi instruments go-chi/chi v5 routers for TaskTrooper Telemetry.
//
//	r := chi.NewRouter()
//	r.Use(trooperchi.Middleware())
//	r.Get("/users/{id}", getUser) // trooper.route = /users/:id
//
// Register the middleware with r.Use (inside the router) so the chi route context is available.
// No headers, query strings or bodies are recorded.
package trooperchi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	trooperhttp "github.com/tasktrooper/telemetry-go/integrations/nethttp"
)

// Option configures Middleware (same options as trooperhttp: WithPanicPolicy, WithOtelHTTPOptions).
type Option = trooperhttp.Option

var (
	// WithPanicPolicy sets the panic policy (default trooper.PanicRepanic, use chi middleware.Recoverer).
	WithPanicPolicy = trooperhttp.WithPanicPolicy
	// WithOtelHTTPOptions passes options to the underlying otelhttp handler.
	WithOtelHTTPOptions = trooperhttp.WithOtelHTTPOptions
)

// Middleware returns chi middleware creating a server span with trooper.route from the chi route
// pattern, entry route on all child spans, trooper.caller and panic capture.
func Middleware(opts ...Option) func(http.Handler) http.Handler {
	opts = append([]Option{trooperhttp.WithRouteResolver(RoutePattern)}, opts...)
	return trooperhttp.Middleware(opts...)
}

// RoutePattern returns the chi route pattern matched so far for r ("" before routing).
func RoutePattern(r *http.Request) string {
	if r == nil {
		return ""
	}
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return ""
	}
	p := rctx.RoutePattern()
	if p == "" || p == "/*" && len(rctx.RoutePatterns) == 0 {
		return ""
	}
	return p
}
