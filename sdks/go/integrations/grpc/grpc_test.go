package troopergrpc

import (
	"context"
	"net"
	"strings"
	"testing"

	"time"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	trooper "github.com/tasktrooper/telemetry-go"
	"github.com/tasktrooper/telemetry-go/troopertest"
)

var gotBaggage string

func unary(name string, fn func(ctx context.Context) error) grpc.MethodDesc {
	return grpc.MethodDesc{MethodName: name, Handler: func(_ any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
		in := new(emptypb.Empty)
		if err := dec(in); err != nil {
			return nil, err
		}
		h := func(ctx context.Context, _ any) (any, error) { return &emptypb.Empty{}, fn(ctx) }
		if ic == nil {
			return h(ctx, in)
		}
		return ic(ctx, in, &grpc.UnaryServerInfo{FullMethod: "/test.Svc/" + name}, h)
	}}
}

var desc = grpc.ServiceDesc{
	ServiceName: "test.Svc",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		unary("Ok", func(ctx context.Context) error {
			md, _ := metadata.FromIncomingContext(ctx)
			gotBaggage = strings.Join(md.Get("baggage"), ",")
			_, child := otel.Tracer("x").Start(ctx, "child", trace.WithSpanKind(trace.SpanKindClient))
			child.End()
			return nil
		}),
		unary("Panic", func(context.Context) error { panic("boom") }),
	},
}

func dial(t *testing.T, policy trooper.PanicPolicy, clientOpts ...grpc.DialOption) *grpc.ClientConn {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.StatsHandler(ServerHandler()), grpc.ChainUnaryInterceptor(UnaryServerInterceptor(WithPanicPolicy(policy))))
	srv.RegisterService(&desc, struct{}{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, clientOpts...)
	conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestServer(t *testing.T) {
	tests := []struct {
		name, method string
		policy       trooper.PanicPolicy
		wantCode     codes.Code
	}{
		{name: "ok", method: "/test.Svc/Ok", wantCode: codes.OK},
		{name: "unknown method", method: "/test.Svc/Nope", wantCode: codes.Unimplemented},
		{name: "panic recover", method: "/test.Svc/Panic", policy: trooper.PanicRecover, wantCode: codes.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := troopertest.New(t, "svc")
			conn := dial(t, tt.policy)
			md := metadata.MD{}
			for k, v := range troopertest.CallerHeaders("upstream") {
				md.Set(k, v)
			}
			ctx := metadata.NewOutgoingContext(t.Context(), md)
			err := conn.Invoke(ctx, tt.method, &emptypb.Empty{}, &emptypb.Empty{})
			if status.Code(err) != tt.wantCode {
				t.Fatalf("code=%v want %v (%v)", status.Code(err), tt.wantCode, err)
			}
			if tt.wantCode == codes.Unimplemented {
				return // gRPC does not invoke stats handlers for unknown methods; must not crash
			}
			server := waitServerSpan(h)
			if server == nil {
				t.Fatal("no server span")
			}
			if got := troopertest.AttrString(server, trooper.AttrRoute); got != tt.method {
				t.Errorf("trooper.route=%q", got)
			}
			if got := troopertest.AttrString(server, trooper.AttrEntryRoute); got != tt.method {
				t.Errorf("entry route=%q", got)
			}
			if got := troopertest.AttrString(server, trooper.AttrCaller); got != "upstream" {
				t.Errorf("caller=%q", got)
			}
			if tt.name == "ok" {
				if got := troopertest.AttrString(h.SpanByName("child"), trooper.AttrEntryRoute); got != tt.method {
					t.Errorf("child entry route=%q", got)
				}
			}
			if tt.name == "panic recover" && (!troopertest.HasException(server) || server.Status().Code != otelcodes.Error) {
				t.Errorf("panic not recorded: %v", server.Status())
			}
		})
	}
}

func TestRepanicPropagates(t *testing.T) {
	troopertest.New(t, "svc")
	ctx, span := otel.Tracer("x").Start(t.Context(), "server", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	defer func() {
		if recover() == nil {
			t.Error("expected re-panic")
		}
	}()
	_, _ = UnaryServerInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { panic("boom") })
}

func TestClientHandlerBaggage(t *testing.T) {
	troopertest.New(t, "svc")
	conn := dial(t, trooper.PanicRecover, grpc.WithStatsHandler(ClientHandler()))
	if err := conn.Invoke(t.Context(), "/test.Svc/Ok", &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBaggage, "tt.svc=svc") {
		t.Errorf("baggage=%q", gotBaggage)
	}
}

// waitServerSpan waits for the server span: gRPC ends it after the response is sent.
func waitServerSpan(h *troopertest.Harness) sdktrace.ReadOnlySpan {
	for i := 0; i < 200; i++ {
		if s := h.SpanByKind(trace.SpanKindServer); s != nil {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}
