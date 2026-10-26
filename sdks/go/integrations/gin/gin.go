// Package troopergin instruments gin-gonic/gin for TaskTrooper Telemetry.
//
//	r := gin.New()
//	r.Use(gin.Recovery())
//	troopergin.Use(r)                 // or: r.Use(troopergin.Middleware()...)
//	r.GET("/users/:id", getUser)      // trooper.route = /users/:id
//
// It wraps the official otelgin middleware (server span, HTTP semconv, metrics) and adds
// trooper.route, trooper.entry.route/method on every span of the request, trooper.caller (from
// baggage tt.svc) and panic capture. Handlers must use c.Request.Context() for child spans.
// No headers, query strings or bodies are recorded.
package troopergin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/trace"

	trooper "github.com/tasktrooper/telemetry-go"
)

type config struct {
	panicPolicy trooper.PanicPolicy
	serverName  string
	otelOpts    []otelgin.Option
}

// Option configures the middleware.
type Option func(*config)

// WithPanicPolicy sets the panic policy. Default trooper.PanicRepanic (gin.Recovery responds 500);
// trooper.PanicRecover aborts with 500 itself.
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.panicPolicy = p } }

// WithServerName sets the server name passed to otelgin (server.address); default "".
func WithServerName(name string) Option { return func(c *config) { c.serverName = name } }

// WithOtelGinOptions passes options to the underlying otelgin middleware.
func WithOtelGinOptions(opts ...otelgin.Option) Option {
	return func(c *config) { c.otelOpts = append(c.otelOpts, opts...) }
}

// Middleware returns the otelgin middleware followed by the trooper middleware.
// Register both: r.Use(troopergin.Middleware()...).
func Middleware(opts ...Option) gin.HandlersChain {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return gin.HandlersChain{otelgin.Middleware(c.serverName, c.otelOpts...), trooperHandler(c)}
}

// Use registers Middleware on r.
func Use(r gin.IRoutes, opts ...Option) { r.Use(Middleware(opts...)...) }

func trooperHandler(cfg *config) gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		ctx, entry := trooper.BeginEntry(c.Request.Context(), c.Request.Method, route, nil)
		span := trace.SpanFromContext(ctx)
		trooper.SetServerRoute(span, route)
		c.Request = c.Request.WithContext(ctx)
		defer func() {
			if rec := recover(); rec != nil {
				trooper.RecordPanic(ctx, rec)
				entry.Finish("")
				if cfg.panicPolicy == trooper.PanicRepanic || rec == http.ErrAbortHandler {
					panic(rec)
				}
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
		entry.Finish("")
	}
}
