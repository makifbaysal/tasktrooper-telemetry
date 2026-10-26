// Package trooperfiber instruments gofiber/fiber v2 for TaskTrooper Telemetry.
// For Fiber v3 use github.com/tasktrooper/telemetry-go/integrations/fiberv3.
//
//	app := fiber.New()
//	app.Use(recover.New())
//	trooperfiber.Use(app)              // registers otelfiber + trooper middleware
//	app.Get("/users/:id", getUser)     // trooper.route = /users/:id
//
// It wraps github.com/gofiber/contrib/otelfiber/v2 (server span, metrics) and adds trooper.route,
// trooper.entry.route/method on every span of the request, trooper.caller (baggage tt.svc) and
// panic capture. Handlers must use c.UserContext() for child spans.
//
// Fiber resolves the route only when the matching handler runs, so the entry route on child spans
// is resolved lazily from c.Route() at span start: spans started in middleware registered after
// trooperfiber (before the route handler) carry no entry route. Trace headers are not injected into
// responses. No headers, query strings or bodies are recorded.
package trooperfiber

import (
	"context"
	"reflect"

	"github.com/gofiber/contrib/otelfiber/v2"
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

type config struct {
	panicPolicy trooper.PanicPolicy
	otelOpts    []otelfiber.Option
}

// Option configures the middleware.
type Option func(*config)

// WithPanicPolicy sets the panic policy. Default trooper.PanicRepanic (use fiber's recover
// middleware); trooper.PanicRecover returns fiber.ErrInternalServerError.
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.panicPolicy = p } }

// WithOtelFiberOptions passes options to the underlying otelfiber middleware.
func WithOtelFiberOptions(opts ...otelfiber.Option) Option {
	return func(c *config) { c.otelOpts = append(c.otelOpts, opts...) }
}

// Handlers returns the otelfiber handler followed by the trooper handler; register both in order.
func Handlers(opts ...Option) []fiber.Handler {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	oo := append([]otelfiber.Option{otelfiber.WithPropagators(extractOnly{})}, c.otelOpts...)
	return []fiber.Handler{otelfiber.Middleware(oo...), trooperHandler(c)}
}

// Use registers Handlers on r (app or group).
func Use(r fiber.Router, opts ...Option) {
	for _, h := range Handlers(opts...) {
		r.Use(h)
	}
}

func trooperHandler(cfg *config) fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		own := c.Route()
		resolve := func() string { return routePath(c, own) }
		ctx, entry := trooper.BeginEntry(c.UserContext(), c.Method(), "", resolve)
		span := trace.SpanFromContext(ctx)
		c.SetUserContext(ctx)
		finish := func() {
			route := routePath(c, own)
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

// routePath returns the template of the route currently executing, or "" for middleware
// (app.Use) routes and unmatched requests.
func routePath(c *fiber.Ctx, own *fiber.Route) (path string) {
	defer func() {
		if recover() != nil {
			path = ""
		}
	}()
	r := c.Route()
	if r == nil || r == own || len(r.Handlers) == 0 || isUse(r) {
		return ""
	}
	return r.Path
}

// isUse reads the unexported Route.use flag (middleware/prefix routes).
func isUse(r *fiber.Route) bool {
	f := reflect.ValueOf(r).Elem().FieldByName("use")
	return f.IsValid() && f.Kind() == reflect.Bool && f.Bool()
}

// extractOnly extracts with the global propagator but never injects (otelfiber would otherwise
// write traceparent/baggage, including tt.svc, into every response).
type extractOnly struct{}

func (extractOnly) Inject(context.Context, propagation.TextMapCarrier) {}
func (extractOnly) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, c)
}
func (extractOnly) Fields() []string { return otel.GetTextMapPropagator().Fields() }
