package gw

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	colprofilespb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling"
	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling/otlptest"
	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

// otlpProfilesReq builds a CPU profile request with nFuncs distinct leaf functions (one sample each).
func otlpProfilesReq(nFuncs int) *colprofilespb.ExportProfilesServiceRequest {
	b := otlptest.New()
	main := b.Loc("main.main")
	var samples []*profilespb.Sample
	for i := range nFuncs {
		st := b.Stack(b.Loc(fmt.Sprintf("github.com/acme/shop/internal/orders.(*Service).handler%06d", i)), main)
		samples = append(samples, &profilespb.Sample{StackIndex: st, Values: []int64{int64(i + 1)}})
	}
	b.Resource([]string{"service.name", "api", "process.runtime.name", "go"},
		&profilespb.Profile{SampleType: b.VT("cpu", "nanoseconds"), TimeUnixNano: 1_730_000_000_000_000_000, DurationNano: 10e9, Samples: samples})
	return b.Req
}

const otlpProfilesJSON = `{"resourceProfiles":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},
 "scopeProfiles":[{"profiles":[{"sampleType":{"typeStrindex":1,"unitStrindex":2},"timeUnixNano":"1730000000000000000","durationNano":"10000000000",
  "profileId":"0102030405060708090a0b0c0d0e0f10","samples":[{"stackIndex":1,"values":["30"]}],"futureField":true}]}]}],
 "dictionary":{"stringTable":["","cpu","nanoseconds","main.main"],"functionTable":[{},{"nameStrindex":3}],
  "locationTable":[{},{"lines":[{"functionIndex":1}]}],"stackTable":[{},{"locationIndices":[1]}],"attributeTable":[{}],"mappingTable":[{}],"linkTable":[{}]}}`

func TestOTLPProfilesHTTP(t *testing.T) {
	small := otlpProfilesReq(3)
	smallPB, _ := proto.Marshal(small)
	medium := otlpProfilesReq(2000) // compresses well, < 256 KiB raw
	mediumPB, _ := proto.Marshal(medium)
	large := otlpProfilesReq(6000) // > 256 KiB raw → gzip'ed before producing
	largePB, _ := proto.Marshal(large)
	if len(mediumPB) > otlpProfileGzipAbove || len(largePB) <= otlpProfileGzipAbove {
		t.Fatalf("fixture sizes %d / %d", len(mediumPB), len(largePB))
	}
	key := map[string]string{"x-trooper-key": "good"}

	tests := []struct {
		name       string
		method     string
		ct, enc    string
		hdr        map[string]string
		query      string
		body       []byte
		limiter    RateLimiter
		full       bool
		wantStatus int
		wantRecs   int
		want       *colprofilespb.ExportProfilesServiceRequest // decoded record value
		wantGzip   bool
		check      func(t *testing.T, req *colprofilespb.ExportProfilesServiceRequest)
	}{
		{name: "protobuf", ct: "application/x-protobuf", hdr: key, body: smallPB, wantStatus: 200, wantRecs: 1, want: small},
		{name: "protobuf bearer, no content type", hdr: map[string]string{"Authorization": "Bearer good"}, body: smallPB, wantStatus: 200, wantRecs: 1, want: small},
		{name: "content-encoding gzip: wire bytes forwarded", ct: "application/x-protobuf", enc: "gzip", hdr: key, body: gz(mediumPB), wantStatus: 200, wantRecs: 1, want: medium, wantGzip: true},
		{name: "large protobuf gzip'ed by gw", ct: "application/x-protobuf", hdr: key, body: largePB, wantStatus: 200, wantRecs: 1, want: large, wantGzip: true},
		{name: "otlp json with hex profileId", ct: "application/json", hdr: key, body: []byte(otlpProfilesJSON), wantStatus: 200, wantRecs: 1,
			check: func(t *testing.T, req *colprofilespb.ExportProfilesServiceRequest) {
				p := req.GetResourceProfiles()[0].GetScopeProfiles()[0].GetProfiles()[0]
				if !bytes.Equal(p.GetProfileId(), []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}) || p.GetSamples()[0].GetValues()[0] != 30 {
					t.Fatalf("profile %v", p)
				}
				if r := profiling.ConvertOTLP(req, nowFixed); len(r.Profiles) != 1 || r.Profiles[0].Series.Total != 30 || r.Profiles[0].Service != "api" {
					t.Fatalf("converted %+v", r)
				}
			}},
		{name: "empty request", ct: "application/json", hdr: key, body: []byte(`{}`), wantStatus: 200},
		{name: "empty protobuf", ct: "application/x-protobuf", hdr: key, body: nil, wantStatus: 200},
		{name: "garbage protobuf", ct: "application/x-protobuf", hdr: key, body: []byte("\xff\xff\xff garbage"), wantStatus: 400},
		{name: "garbage json", ct: "application/json", hdr: key, body: []byte(`{"resourceProfiles":`), wantStatus: 400},
		{name: "missing key", ct: "application/x-protobuf", body: smallPB, wantStatus: 401},
		{name: "query key refused", ct: "application/x-protobuf", query: "key=good", body: smallPB, wantStatus: 401},
		{name: "origin not allowed for key", ct: "application/x-protobuf", hdr: map[string]string{"x-trooper-key": "browser", "Origin": "https://evil.com"}, body: smallPB, wantStatus: 403},
		{name: "rate limited", ct: "application/x-protobuf", hdr: key, body: smallPB, limiter: denyLimiter{true}, wantStatus: 429},
		{name: "buffer full", ct: "application/x-protobuf", hdr: key, body: smallPB, full: true, wantStatus: 503},
		{name: "wire > 4 MiB", ct: "application/x-protobuf", hdr: key, body: make([]byte, 4<<20+1), wantStatus: 413},
		{name: "bomb > 32 MiB", ct: "application/x-protobuf", enc: "gzip", hdr: key, body: gz(make([]byte, 33<<20)), wantStatus: 413},
		{name: "bad encoding", ct: "application/x-protobuf", enc: "br", hdr: key, body: smallPB, wantStatus: 415},
		{name: "GET", method: http.MethodGet, hdr: key, wantStatus: 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &memSink{full: tt.full}
			lim := tt.limiter
			if lim == nil {
				lim = denyLimiter{}
			}
			srv := newTestServer(sink, lim, 1<<20, 900<<10)
			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			url := OTLPProfilesPath
			if tt.query != "" {
				url += "?" + tt.query
			}
			req := httptest.NewRequest(method, url, bytes.NewReader(tt.body))
			if tt.ct != "" {
				req.Header.Set("Content-Type", tt.ct)
			}
			if tt.enc != "" {
				req.Header.Set("Content-Encoding", tt.enc)
			}
			for k, v := range tt.hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			if len(sink.recs) != tt.wantRecs {
				t.Fatalf("records = %d, want %d", len(sink.recs), tt.wantRecs)
			}
			if tt.wantStatus == 200 {
				wantCT := "application/x-protobuf"
				if tt.ct == "application/json" {
					wantCT = "application/json"
				}
				if got := rec.Header().Get("Content-Type"); got != wantCT {
					t.Fatalf("content type %q", got)
				}
			}
			if tt.wantRecs == 0 {
				return
			}
			r := sink.recs[0]
			h := map[string]string{}
			for _, x := range r.Headers {
				h[x.Key] = string(x.Value)
			}
			if r.Topic != topics.Profiles || string(r.Key) != "9" || h[topics.HeaderFormat] != profiling.FormatOTLP ||
				h[topics.HeaderProjectID] != "9" || h[topics.HeaderService] != "" || h[topics.HeaderStart] != "" {
				t.Fatalf("record %s %v", r.Topic, h)
			}
			if profiling.IsGzip(r.Value) != tt.wantGzip {
				t.Fatalf("gzip'ed = %v, want %v", profiling.IsGzip(r.Value), tt.wantGzip)
			}
			got, err := profiling.DecodeOTLP(r.Value, 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want != nil && !proto.Equal(got, tt.want) {
				t.Fatal("record value differs from the request")
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestOTLPProfilesGRPC(t *testing.T) {
	sink := &memSink{}
	srv := newTestServer(sink, denyLimiter{}, 1<<20, 900<<10) // OTLP limit 1 MiB, profiles 32 MiB
	lis := bufconn.Listen(1 << 20)
	g := srv.GRPCServer()
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	conn, err := grpc.NewClient("passthrough:///buf", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(64<<20)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := colprofilespb.NewProfilesServiceClient(conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-trooper-key", "good")

	if _, err := c.Export(context.Background(), otlpProfilesReq(1)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no key: %v", err)
	}
	// A 2 MiB profile request is above the OTLP limit but within the profile limit.
	big := otlpProfilesReq(20000)
	if proto.Size(big) <= 1<<20 {
		t.Fatalf("fixture %d bytes", proto.Size(big))
	}
	for _, req := range []*colprofilespb.ExportProfilesServiceRequest{otlpProfilesReq(2), big} {
		if _, err := c.Export(ctx, req, grpc.UseCompressor(gzip.Name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Export(ctx, &colprofilespb.ExportProfilesServiceRequest{}); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(sink.recs) != 2 {
		t.Fatalf("records = %d", len(sink.recs))
	}
	for i, want := range []bool{false, true} {
		r := sink.recs[i]
		if r.Topic != topics.Profiles || profiling.IsGzip(r.Value) != want {
			t.Fatalf("record %d: %s gzip=%v", i, r.Topic, profiling.IsGzip(r.Value))
		}
		if got, err := profiling.DecodeOTLP(r.Value, 32<<20); err != nil || len(got.GetResourceProfiles()) != 1 {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	// Other signals keep the OTLP limit although the server accepts larger messages for profiles.
	tc := coltracepb.NewTraceServiceClient(conn)
	if _, err := tc.Export(ctx, traceReq(12000)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized traces: %v", err)
	}
	if len(sink.recs) != 2 {
		t.Fatalf("records = %d", len(sink.recs))
	}
}

var nowFixed = time.Unix(1_800_000_000, 0)
