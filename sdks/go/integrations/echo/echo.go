// Package trooperecho instruments labstack/echo v4 for TaskTrooper Telemetry.
//
//	e := echo.New()
//	e.Use(middleware.Recover())
//	e.Use(trooperecho.Middleware())
//	e.GET("/users/:id", getUser)      // trooper.route = /users/:id
//
// It wraps github.com/labstack/echo-otel/v4 (the maintained successor of the deprecated contrib
// otelecho) and adds trooper.route, trooper.entry.route/method on every span of the request,
// trooper.caller (baggage tt.svc) and panic capture. Handlers must use c.Request().Context() for
// child spans. Register with e.Use (not e.Pre) so the route is known. No headers, query strings
// or bodies are recorded.
package trooperecho

import (
	"net/http"

	echootel "github.com/labstack/echo-otel/v4"
	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

type config struct {
	panicPolicy trooper.PanicPolicy
	otel        echootel.Config
}

// Option configures Middleware.
type Option func(*config)

// WithPanicPolicy sets the panic policy. Default trooper.PanicRepanic (echo's Recover middleware
// handles it); trooper.PanicRecover returns a 500 *echo.HTTPError.
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.panicPolicy = p } }

// WithOtelConfig sets the configuration of the underlying echo-otel middleware.
func WithOtelConfig(cfg echootel.Config) Option { return func(c *config) { c.otel = cfg } }

// Middleware returns the echo middleware.
func Middleware(opts ...Option) echo.MiddlewareFunc {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}
	otelMW, err := cfg.otel.ToMiddleware()
	if err != nil { // invalid ServerName: never crash the host, fall back to defaults
		otelMW, _ = echootel.Config{}.ToMiddleware()
	}
	inner := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			req := c.Request()
			route := c.Path()
			ctx, entry := trooper.BeginEntry(req.Context(), req.Method, route, nil)
			trooper.SetServerRoute(trace.SpanFromContext(ctx), route)
			c.SetRequest(req.WithContext(ctx))
			defer func() {
				if rec := recover(); rec != nil {
					trooper.RecordPanic(ctx, rec)
					entry.Finish("")
					if cfg.panicPolicy == trooper.PanicRepanic || rec == http.ErrAbortHandler {
						panic(rec)
					}
					err = echo.NewHTTPError(http.StatusInternalServerError)
				}
			}()
			err = next(c)
			entry.Finish(c.Path())
			return err
		}
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc { return otelMW(inner(next)) }
}
