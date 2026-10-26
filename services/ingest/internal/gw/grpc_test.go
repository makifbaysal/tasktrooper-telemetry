package gw

import (
	"context"
	"net"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

func TestGRPCExport(t *testing.T) {
	sink := &memSink{}
	srv := newTestServer(sink, denyLimiter{}, 1<<20, 900<<10)
	lis := bufconn.Listen(1 << 20)
	g := srv.GRPCServer()
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	conn, err := grpc.NewClient("passthrough:///buf", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := coltracepb.NewTraceServiceClient(conn)

	_, err = c.Export(context.Background(), traceReq(2))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no key: %v", err)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer good")
	if _, err := c.Export(ctx, traceReq(2), grpc.UseCompressor(gzip.Name)); err != nil {
		t.Fatal(err)
	}
	if len(sink.recs) != 1 || sink.recs[0].Topic != topics.Traces || string(sink.recs[0].Key) != "9" {
		t.Fatalf("records: %+v", sink.recs)
	}

	limited := newTestServer(sink, denyLimiter{true}, 1<<20, 900<<10)
	if err := limited.grpcExport(metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-trooper-key", "good")), sigTraces, traceReq(1)); status.Code(err) != codes.ResourceExhausted || len(status.Convert(err).Details()) != 1 {
		t.Fatalf("rate limited: %v", err)
	}
}
