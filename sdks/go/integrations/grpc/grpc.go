// Package troopergrpc instruments gRPC servers and clients for TaskTrooper Telemetry.
//
//	srv := grpc.NewServer(
//		grpc.StatsHandler(troopergrpc.ServerHandler()),
//		grpc.ChainUnaryInterceptor(troopergrpc.UnaryServerInterceptor()),
//		grpc.ChainStreamInterceptor(troopergrpc.StreamServerInterceptor()),
//	)
//	conn, _ := grpc.NewClient(addr, grpc.WithStatsHandler(troopergrpc.ClientHandler()), ...)
//
// It wraps the official otelgrpc stats handlers and sets trooper.route (= full method name,
// e.g. /pkg.Service/Method), trooper.entry.route on every span of the RPC and trooper.caller
// (baggage tt.svc). The interceptors record panics. Metadata and payloads are not recorded.
package troopergrpc

import (
	"context"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	trooper "github.com/tasktrooper/telemetry-go"
)

// ServerHandler returns an otelgrpc server stats handler extended with trooper attributes.
func ServerHandler(opts ...otelgrpc.Option) stats.Handler {
	return serverHandler{otelgrpc.NewServerHandler(opts...)}
}

// ClientHandler returns the otelgrpc client stats handler. Outgoing metadata carries traceparent
// and baggage tt.svc=<this service> via the global trooper propagator.
func ClientHandler(opts ...otelgrpc.Option) stats.Handler { return otelgrpc.NewClientHandler(opts...) }

type serverHandler struct{ stats.Handler }

func (h serverHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = h.Handler.TagRPC(ctx, info)
	func() {
		defer func() { _ = recover() }()
		ctx, _ = trooper.BeginEntry(ctx, "", info.FullMethodName, nil)
		trace.SpanFromContext(ctx).SetAttributes(trooper.AttrRoute.String(info.FullMethodName))
	}()
	return ctx
}

// HandleRPC forwards to otelgrpc and, at the end of the RPC, restores the pprof goroutine labels
// set for continuous profiling (trooper.route = full method, trooper.method = GRPC).
func (h serverHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	h.Handler.HandleRPC(ctx, s)
	if _, ok := s.(*stats.End); ok {
		trooper.EntryFromContext(ctx).Finish("")
	}
}

type config struct{ panicPolicy trooper.PanicPolicy }

// Option configures the interceptors.
type Option func(*config)

// WithPanicPolicy sets the panic policy. Default trooper.PanicRepanic (gRPC does not recover
// panics: the process crashes, as without instrumentation); trooper.PanicRecover returns
// codes.Internal.
func WithPanicPolicy(p trooper.PanicPolicy) Option { return func(c *config) { c.panicPolicy = p } }

func newConfig(opts []Option) *config {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *config) handlePanic(ctx context.Context, rec any, err *error) {
	trooper.RecordPanic(ctx, rec)
	if c.panicPolicy == trooper.PanicRepanic {
		panic(rec)
	}
	*err = status.Error(codes.Internal, "internal error")
}

// UnaryServerInterceptor records panics of unary handlers on the server span.
func UnaryServerInterceptor(opts ...Option) grpc.UnaryServerInterceptor {
	c := newConfig(opts)
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				c.handlePanic(ctx, rec, &err)
			}
		}()
		return handler(ctx, req)
	}
}

// StreamServerInterceptor records panics of stream handlers on the server span.
func StreamServerInterceptor(opts ...Option) grpc.StreamServerInterceptor {
	c := newConfig(opts)
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				c.handlePanic(ss.Context(), rec, &err)
			}
		}()
		return handler(srv, ss)
	}
}
