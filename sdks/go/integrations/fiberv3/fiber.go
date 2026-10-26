// Package trooperfiber3 instruments gofiber/fiber v3 for TaskTrooper Telemetry.
//
//	app := fiber.New()
//	app.Use(recover.New())
//	trooperfiber3.Use(app)             // registers contrib otel + trooper middleware
//	app.Get("/users/:id", getUser)     // trooper.route = /users/:id
//
// It wraps github.com/gofiber/contrib/v3/otel (server span, HTTP semconv, metrics) and adds
// trooper.route, trooper.entry.route/method on every span of the request, trooper.caller (baggage
// tt.svc) and panic capture. Handlers must use c.Context() for child spans.
//
// The entry route on child spans is resolved lazily from c.Route() at span start, so spans
// started in middleware (before the route handler) carry no entry route. Trace headers are not
// injected into responses. No headers, query strings or bodies are recorded.
package trooperfiber3

import (
	fiberotel "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

type config struct {
	panicPolicy trooper.PanicPolicy
	otelOpts    []fiberotel.Option
}

// Option configures the middleware.
type Option func(*config)

// WithPanicPolicy sets the panic policy. Default trooper.PanicRepanic (use fiber's recover
// middleware); trooper.PanicRecover returns fiber.ErrInternalServerError.
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.panicPolicy = p } }

// WithOtelOptions passes options to the underlying contrib otel middleware.
func WithOtelOptions(opts ...fiberotel.Option) Option {
	return func(c *config) { c.otelOpts = append(c.otelOpts, opts...) }
}

// Handlers returns the contrib otel handler followed by the trooper handler; register both in order.
func Handlers(opts ...Option) []fiber.Handler {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	oo := append([]fiberotel.Option{fiberotel.WithResponsePropagators(propagation.NewCompositeTextMapPropagator())}, c.otelOpts...)
	return []fiber.Handler{fiberotel.Middleware(oo...), trooperHandler(c)}
}

// Use registers Handlers on r (app or group).
func Use(r fiber.Router, opts ...Option) {
	for _, h := range Handlers(opts...) {
		r.Use(h)
	}
}

func trooperHandler(cfg *config) fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		resolve := func() string { return routePath(c) }
		ctx, entry := trooper.BeginEntry(c.Context(), c.Method(), "", resolve)
		span := trace.SpanFromContext(ctx)
		c.SetContext(ctx)
		finish := func() {
			route := ""
			if c.Matched() {
				route = routePath(c)
			}
			entry.Finish(route) // freeze before fiber recycles c
			trooper.SetServerRoute(span, route)
		}
		defer func() {
			if rec := recover(); rec != nil {
				trooper.RecordPanic(ctx, rec)
				finish()
				if cfg.panicPolicy == trooper.PanicRepanic {
					panic(rec)
				}
				err = fiber.ErrInternalServerError
			}
		}()
		err = c.Next()
		finish()
		return err
	}
}

// routePath returns the template of the route handler currently executing ("" in middleware).
func routePath(c fiber.Ctx) (path string) {
	defer func() {
		if recover() != nil {
			path = ""
		}
	}()
	if c.IsMiddleware() {
		return ""
	}
	r := c.Route()
	if r == nil || len(r.Handlers) == 0 {
		return ""
	}
	return r.Path
}
