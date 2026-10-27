package gw

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colprofilespb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // register gzip decompressor
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// GRPCServer returns a gRPC server exposing the OTLP Trace/Metrics/Logs/Profiles services. MaxRecvMsgSize bounds
// the decompressed message size as well (gzip bombs are rejected with ResourceExhausted); it is the larger of the OTLP
// and the profile body limits, and messages of the other signals above MaxBodyBytes are refused in the handler.
func (s *Server) GRPCServer() *grpc.Server {
	g := grpc.NewServer(grpc.MaxRecvMsgSize(int(max(s.cfg.MaxBodyBytes, s.cfg.ProfileMaxBodyBytes))))
	colprofilespb.RegisterProfilesServiceServer(g, profilesSvc{s: s})
	coltracepb.RegisterTraceServiceServer(g, traceSvc{s: s})
	colmetricspb.RegisterMetricsServiceServer(g, metricsSvc{s: s})
	collogspb.RegisterLogsServiceServer(g, logsSvc{s: s})
	return g
}

type traceSvc struct {
	coltracepb.UnimplementedTraceServiceServer
	s *Server
}

func (t traceSvc) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	return &coltracepb.ExportTraceServiceResponse{}, t.s.grpcExport(ctx, sigTraces, req)
}

type metricsSvc struct {
	colmetricspb.UnimplementedMetricsServiceServer
	s *Server
}

func (m metricsSvc) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	return &colmetricspb.ExportMetricsServiceResponse{}, m.s.grpcExport(ctx, sigMetrics, req)
}

type logsSvc struct {
	collogspb.UnimplementedLogsServiceServer
	s *Server
}

func (l logsSvc) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	return &collogspb.ExportLogsServiceResponse{}, l.s.grpcExport(ctx, sigLogs, req)
}

func (s *Server) grpcExport(ctx context.Context, sig signal, msg proto.Message) error {
	start := time.Now()
	endpoint := "grpc:" + sig.topic
	size := proto.Size(msg)
	err := s.grpcExportInner(ctx, sig, msg, size)
	var ie *ingestError
	code := http.StatusOK
	if errors.As(err, &ie) {
		code = ie.status
	} else if err != nil {
		code = http.StatusInternalServerError
	}
	observe(endpoint, code, ie, size, start)
	return toStatus(err)
}

func (s *Server) grpcExportInner(ctx context.Context, sig signal, msg proto.Message, size int) error {
	md, _ := metadata.FromIncomingContext(ctx)
	raw := first(md, "x-trooper-key")
	if raw == "" {
		if a := first(md, "authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
			raw = strings.TrimSpace(a[7:])
		}
	}
	// Key auth over gRPC never checked allowed_origins; a forwarded Origin (grpc-web proxies) only disqualifies the
	// internal credential.
	internal := strings.TrimSpace(first(md, "x-trooper-internal"))
	origin := ""
	if internal != "" {
		origin = first(md, "origin")
	}
	key, err := s.authenticate(ctx, internal, first(md, "x-trooper-project"), origin, raw)
	if err != nil {
		return err
	}
	if err := s.allow(ctx, key); err != nil {
		return err
	}
	if sig.topic != topics.Profiles && int64(size) > s.cfg.MaxBodyBytes {
		return &ingestError{status: http.StatusRequestEntityTooLarge, msg: "message exceeds " + strconv.FormatInt(s.cfg.MaxBodyBytes, 10) + " bytes"}
	}
	m := meta{tenant: key.Tenant, receivedAt: s.now()}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
			m.clientIP = host
		}
	}
	if xff := first(md, "x-forwarded-for"); xff != "" {
		m.clientIP = strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if req, ok := msg.(*colprofilespb.ExportProfilesServiceRequest); ok {
		return s.publishOTLPProfiles(req, nil, nil, m)
	}
	return s.publishMsg(sig, msg, m)
}

func first(md metadata.MD, k string) string {
	if v := md.Get(k); len(v) > 0 {
		return v[0]
	}
	return ""
}

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	var ie *ingestError
	if !errors.As(err, &ie) {
		return status.Error(codes.Internal, err.Error())
	}
	var c codes.Code
	switch ie.status {
	case http.StatusUnauthorized:
		c = codes.Unauthenticated
	case http.StatusForbidden:
		c = codes.PermissionDenied
	case http.StatusTooManyRequests:
		c = codes.ResourceExhausted
	case http.StatusServiceUnavailable:
		c = codes.Unavailable
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		c = codes.InvalidArgument
	default:
		c = codes.Internal
	}
	st := status.New(c, ie.msg)
	if ie.retryAfter > 0 { // OTLP exporters honor RetryInfo on ResourceExhausted/Unavailable
		if d, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(time.Duration(ie.retryAfter) * time.Second)}); err == nil {
			st = d
		}
	}
	return st.Err()
}
