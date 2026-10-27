package gw

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/tasktrooper/telemetry/services/pkg/topics"
)

const replaySeg = `{"resource":{"service":"shop-web","env":"production","platform":"web"},"session_id":"3f2b9c1e-aa01-4b6e-9f00-1d2e3f4a5b6c",
 "segment":3,"start_ts":1730000000000,"end_ts":1730000005000,"format":"rrweb","has_full_snapshot":true,"view_name":"/checkout",
 "stats":{"clicks":3},"events":[{"type":4,"data":{},"timestamp":1730000000000},{"type":2,"data":{},"timestamp":1730000000001}]}`

func gunzipAll(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("value is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReplay(t *testing.T) {
	big := `{"session_id":"s","segment":0,"format":"rrweb","events":["` + strings.Repeat("a", 11<<20) + `"]}`
	tests := []struct {
		name       string
		ct, enc    string
		hdr        map[string]string
		query      string
		body       []byte
		limiter    RateLimiter
		wantStatus int
	}{
		{name: "json + key header", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(replaySeg), wantStatus: 200},
		{name: "gzip + query key (sendBeacon)", ct: "text/plain;charset=UTF-8", enc: "gzip", query: "key=good", body: gz([]byte(replaySeg)), wantStatus: 200},
		{name: "browser origin allowed", ct: "application/json", query: "key=browser", hdr: map[string]string{"Origin": "https://shop.example.com"}, body: []byte(replaySeg), wantStatus: 200},
		{name: "browser origin denied", ct: "application/json", query: "key=browser", hdr: map[string]string{"Origin": "https://evil.com"}, body: []byte(replaySeg), wantStatus: 403},
		{name: "internal credential from browser", ct: "application/json", hdr: map[string]string{"x-trooper-internal": "x", "Origin": "https://a.b"}, body: []byte(replaySeg), wantStatus: 403},
		{name: "missing key", ct: "application/json", body: []byte(replaySeg), wantStatus: 401},
		{name: "rate limited", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(replaySeg), limiter: denyLimiter{true}, wantStatus: 429},
		{name: "invalid json", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"session_id":`), wantStatus: 400},
		{name: "missing session", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"segment":0,"format":"rrweb","events":[]}`), wantStatus: 400},
		{name: "unsafe session id", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"session_id":"../x","segment":0,"format":"rrweb","events":[]}`), wantStatus: 400},
		{name: "missing segment", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"session_id":"s","format":"rrweb","events":[]}`), wantStatus: 400},
		{name: "negative segment", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"session_id":"s","segment":-1,"format":"rrweb","events":[]}`), wantStatus: 400},
		{name: "bad format", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"session_id":"s","segment":0,"format":"video","events":[]}`), wantStatus: 400},
		{name: "missing events", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"session_id":"s","segment":0,"format":"wireframe"}`), wantStatus: 400},
		{name: "events not array", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte(`{"session_id":"s","segment":0,"format":"rrweb","events":{}}`), wantStatus: 400},
		{name: "wire > 1 MiB", ct: "application/json", hdr: map[string]string{"x-trooper-key": "good"}, body: bytes.Repeat([]byte(" "), 1<<20+1), wantStatus: 413},
		{name: "decompressed > 10 MiB", ct: "application/json", enc: "gzip", hdr: map[string]string{"x-trooper-key": "good"}, body: gz([]byte(big)), wantStatus: 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &memSink{}
			lim := tt.limiter
			if lim == nil {
				lim = denyLimiter{}
			}
			srv := newTestServer(sink, lim, 5<<20, 900<<10)
			url := "/v1/replay"
			if tt.query != "" {
				url += "?" + tt.query
			}
			req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.ct)
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
			if tt.wantStatus != 200 {
				if len(sink.recs) != 0 {
					t.Fatal("nothing must be published on error")
				}
				return
			}
			if len(sink.recs) != 1 {
				t.Fatalf("records = %d", len(sink.recs))
			}
			r := sink.recs[0]
			if r.Topic != topics.Replay || string(r.Key) != "9" {
				t.Fatalf("topic/key %s/%s", r.Topic, r.Key)
			}
			h := map[string]string{}
			for _, x := range r.Headers {
				h[x.Key] = string(x.Value)
			}
			if h[topics.HeaderSessionID] != "3f2b9c1e-aa01-4b6e-9f00-1d2e3f4a5b6c" || h[topics.HeaderSegment] != "3" || h[topics.HeaderOrgID] != "7" || h[topics.HeaderReceivedAt] == "" {
				t.Fatalf("headers %v", h)
			}
			if got := gunzipAll(t, r.Value); string(got) != replaySeg {
				t.Fatalf("value = %q", got)
			}
		})
	}
}

func TestReplayCORSPreflight(t *testing.T) {
	srv := newTestServer(&memSink{}, denyLimiter{}, 1<<20, 900<<10)
	req := httptest.NewRequest(http.MethodOptions, "/v1/replay", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "https://shop.example.com" ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Content-Encoding") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	// /v1/profiles is server-only: no preflight
	req = httptest.NewRequest(http.MethodOptions, "/v1/profiles", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 405 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("profiles preflight: %d %v", rec.Code, rec.Header())
	}
}

func pprofBody(t *testing.T) []byte {
	t.Helper()
	f := &profile.Function{ID: 1, Name: "main.main"}
	l := &profile.Location{ID: 1, Line: []profile.Line{{Function: f}}}
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Function: []*profile.Function{f}, Location: []*profile.Location{l}, Sample: []*profile.Sample{{Value: []int64{1, 10}, Location: []*profile.Location{l}}}}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil { // gzip'ed, as runtime/pprof writes it
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProfiles(t *testing.T) {
	pp := pprofBody(t)
	folded := []byte("# trooper.route=/a trooper.method=GET\nmain;a 10\nmain;b 5\n")
	bigFolded := bytes.Repeat([]byte("main;sync.(*Mutex).Lock 1000\n"), 2000)
	const q = "service=api&env=prod&version=1.2.0&host=h1&runtime=go&start=1730000000000&end=1730000010000"
	tests := []struct {
		name       string
		query      string
		ct, enc    string
		hdr        map[string]string
		body       []byte
		limiter    RateLimiter
		wantStatus int
		wantValue  []byte // nil = body as sent
		wantFormat string
	}{
		{name: "pprof gzip'ed", query: q, ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: pp, wantStatus: 200, wantFormat: "pprof"},
		{name: "pprof bearer", query: q + "&format=pprof", ct: "application/octet-stream", hdr: map[string]string{"Authorization": "Bearer good"}, body: pp, wantStatus: 200, wantFormat: "pprof"},
		{name: "folded", query: q + "&format=folded&type=cpu&unit=nanoseconds", ct: "text/plain", hdr: map[string]string{"x-trooper-key": "good"}, body: folded, wantStatus: 200, wantFormat: "folded"},
		{name: "folded content-encoding gzip (tiny: smaller decompressed form is sent)", query: q + "&format=folded&type=alloc_space&unit=bytes", ct: "text/plain", enc: "gzip", wantValue: folded,
			hdr: map[string]string{"x-trooper-key": "good"}, body: gz(folded), wantStatus: 200, wantFormat: "folded"},
		{name: "folded content-encoding gzip (large: gzip'ed wire is sent)", query: q + "&format=folded&type=lock&unit=nanoseconds", ct: "text/plain", enc: "gzip",
			hdr: map[string]string{"x-trooper-key": "good"}, body: gz(bigFolded), wantStatus: 200, wantFormat: "folded"},
		{name: "gzip'ed pprof with content-encoding gzip", query: q, ct: "application/octet-stream", enc: "gzip",
			hdr: map[string]string{"x-trooper-key": "good"}, body: gz(pp), wantValue: pp, wantStatus: 200, wantFormat: "pprof"},
		{name: "folded decompressed > 32 MiB", query: q + "&format=folded&type=cpu&unit=nanoseconds", ct: "text/plain", enc: "gzip",
			hdr: map[string]string{"x-trooper-key": "good"}, body: gz(bytes.Repeat([]byte("a;b 1\n"), 6<<20)), wantStatus: 413},
		{name: "query key rejected", query: q + "&key=good", ct: "application/octet-stream", body: pp, wantStatus: 401},
		{name: "browser rejected", query: q, ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "browser", "Origin": "https://shop.example.com"}, body: pp, wantStatus: 403},
		{name: "rate limited", query: q, ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: pp, limiter: denyLimiter{true}, wantStatus: 429},
		{name: "missing service", query: "start=1&end=2", ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: pp, wantStatus: 400},
		{name: "missing start", query: "service=api&end=2", ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: pp, wantStatus: 400},
		{name: "end before start", query: "service=api&start=5&end=2", ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: pp, wantStatus: 400},
		{name: "bad runtime", query: "service=api&start=1&end=2&runtime=cobol", ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: pp, wantStatus: 400},
		{name: "folded without type", query: q + "&format=folded", ct: "text/plain", hdr: map[string]string{"x-trooper-key": "good"}, body: folded, wantStatus: 400},
		{name: "folded wrong unit", query: q + "&format=folded&type=cpu&unit=bytes", ct: "text/plain", hdr: map[string]string{"x-trooper-key": "good"}, body: folded, wantStatus: 400},
		{name: "unknown format", query: q + "&format=jfr", ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: pp, wantStatus: 400},
		{name: "garbage pprof", query: q, ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte("not a profile"), wantStatus: 400},
		{name: "bad folded line", query: q + "&format=folded&type=cpu&unit=nanoseconds", ct: "text/plain", hdr: map[string]string{"x-trooper-key": "good"}, body: []byte("main;a\n"), wantStatus: 400},
		{name: "empty", query: q, ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: nil, wantStatus: 400},
		{name: "wire > 4 MiB", query: q, ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: make([]byte, 4<<20+1), wantStatus: 413},
		{name: "pprof bomb > 32 MiB", query: q, ct: "application/octet-stream", hdr: map[string]string{"x-trooper-key": "good"}, body: gz(make([]byte, 33<<20)), wantStatus: 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &memSink{}
			lim := tt.limiter
			if lim == nil {
				lim = denyLimiter{}
			}
			srv := newTestServer(sink, lim, 5<<20, 900<<10)
			req := httptest.NewRequest(http.MethodPost, "/v1/profiles?"+tt.query, bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.ct)
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
			if tt.wantStatus != 200 {
				if len(sink.recs) != 0 {
					t.Fatal("nothing must be published on error")
				}
				return
			}
			if len(sink.recs) != 1 {
				t.Fatalf("records = %d", len(sink.recs))
			}
			r := sink.recs[0]
			want := tt.wantValue
			if want == nil {
				want = tt.body
			}
			if r.Topic != topics.Profiles || !bytes.Equal(r.Value, want) {
				t.Fatalf("topic %s, value %d bytes (want %d)", r.Topic, len(r.Value), len(want))
			}
			h := map[string]string{}
			for _, x := range r.Headers {
				h[x.Key] = string(x.Value)
			}
			if h[topics.HeaderService] != "api" || h[topics.HeaderEnv] != "prod" || h[topics.HeaderVersion] != "1.2.0" || h[topics.HeaderHost] != "h1" ||
				h[topics.HeaderRuntime] != "go" || h[topics.HeaderStart] != "1730000000000" || h[topics.HeaderEnd] != "1730000010000" ||
				h[topics.HeaderFormat] != tt.wantFormat || h[topics.HeaderProjectID] != "9" {
				t.Fatalf("headers %v", h)
			}
		})
	}
}
