package vercel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
)

var integ = &Integration{ID: 4, ProjectID: 1, Name: "vercel-shop", Secret: []byte("s3cret"),
	Projects: map[string]string{"my-app": "shop-web", "Qmc52npNy86S8VV4Mt8a8dP1LEkRNbgosW3pBCQytkcgf2": "shop-frontend", "prj_shop": "shop-api"}}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func attrs(kvs []*commonpb.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			m[kv.GetKey()] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			m[kv.GetKey()] = strconv.FormatInt(v.IntValue, 10)
		}
	}
	return m
}

func TestSignature(t *testing.T) {
	body := []byte(`[{"id":"1"}]`)
	sig := Sign([]byte("s3cret"), body)
	if len(sig) != 40 {
		t.Fatalf("sha1 hex length=%d", len(sig))
	}
	cases := []struct {
		name   string
		secret string
		body   []byte
		sig    string
		ok     bool
	}{
		{"valid", "s3cret", body, sig, true},
		{"uppercase hex", "s3cret", body, strings.ToUpper(sig), true},
		{"wrong secret", "other", body, sig, false},
		{"tampered body", "s3cret", []byte(`[{"id":"2"}]`), sig, false},
		{"missing", "s3cret", body, "", false},
		{"not hex", "s3cret", body, "zz", false},
		{"no secret configured", "", body, Sign(nil, body), false},
	}
	for _, c := range cases {
		if got := VerifySignature([]byte(c.secret), c.body, c.sig); got != c.ok {
			t.Errorf("%s: got %v want %v", c.name, got, c.ok)
		}
	}
}

func TestRoute(t *testing.T) {
	for in, want := range map[string]string{
		"/api/orders/[id]":        "/api/orders/:id",
		"/blog/[...slug]":         "/blog/*slug",
		"/docs/[[...slug]]":       "/docs/*slug",
		"/dynamic/[route].json":   "/dynamic/:route.json",
		"/api/orders/123?page=1":  "/api/orders/:id",
		"/":                       "/",
		"":                        "",
		"/products/red-shoe":      "/products/red-shoe",
		"/dashboard/[team]/[id]/": "/dashboard/:team/:id",
	} {
		if got := Route(in); got != want {
			t.Errorf("Route(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLogDrain(t *testing.T) {
	res, err := Convert(integ, fixture(t, "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[KindLog] != 4 || res.Logs == nil || len(res.RUM) != 0 || len(res.Traces) != 0 {
		t.Fatalf("counts=%v", res.Counts)
	}
	byBody := map[string]*logspb.LogRecord{}
	svcOf := map[string]map[string]string{}
	for _, rl := range res.Logs.ResourceLogs {
		ra := attrs(rl.GetResource().GetAttributes())
		for _, lr := range rl.ScopeLogs[0].LogRecords {
			b := lr.GetBody().GetStringValue()
			byBody[b] = lr
			svcOf[b] = ra
		}
	}
	// Lambda error log with request info, trace context and mapped service.
	lr := byBody["order 42 failed"]
	if lr == nil {
		t.Fatalf("bodies: %v", keys(byBody))
	}
	a := attrs(lr.Attributes)
	if lr.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_ERROR || hex.EncodeToString(lr.TraceId) != "1b02cd14bb8642fd092bc23f54c7ffcd" ||
		hex.EncodeToString(lr.SpanId) != "f24e8631bd11faa7" || lr.TimeUnixNano != 1573817250283*1e6 {
		t.Errorf("record: %+v", lr)
	}
	want := map[string]string{"vercel.source": "lambda", "vercel.deployment_id": "dpl_233NRGRjVZX1caZrXWtz5g1TAksD", "http.route": "/api/orders/:id",
		"trooper.entry.route": "/api/orders/:id", "http.response.status_code": "500", "http.request.method": "GET", "url.path": "/api/orders/123?page",
		"vercel.request_id": "643af4e3-975a-4cc7-9e7a-1eda11539d90", "vercel.region": "sfo1", "vercel.cache": "MISS"}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("attr %s=%q want %q", k, a[k], v)
		}
	}
	for k, v := range a {
		if strings.Contains(v, "120.75.16.101") || strings.Contains(k, "user_agent") {
			t.Errorf("PII leaked: %s=%s", k, v)
		}
	}
	ra := svcOf["order 42 failed"]
	// No projectName on this entry and the id is not in projectMap → the project id; entries with projectName "my-app" map to shop-web.
	if ra["service.name"] != "gdufoJxB6b9b1fEqr1jUtFkyavUU" {
		t.Errorf("service: %v", ra)
	}
	if svcOf["Build completed successfully"]["service.name"] != "shop-web" || svcOf["Build completed successfully"]["cloud.provider"] != "vercel" ||
		svcOf["Build completed successfully"]["trooper.integration.id"] != "4" {
		t.Errorf("build resource: %v", svcOf["Build completed successfully"])
	}
	if ra["deployment.environment.name"] != "production" || ra["service.version"] != "dpl_233NRGRjVZX1caZrXWtz5g1TAksD" {
		t.Errorf("resource: %v", ra)
	}
	// Static request without a message gets a synthesized body.
	if st := byBody["GET /_next/static/chunk.js?v 200"]; st == nil || attrs(st.Attributes)["http.response.body.size"] != "1024" {
		t.Errorf("static: %v", keys(byBody))
	}
	fw := byBody["blocked"]
	if fw == nil || fw.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_WARN || attrs(fw.Attributes)["vercel.waf_action"] != "deny" ||
		svcOf["blocked"]["service.name"] != "other" {
		t.Errorf("firewall: %+v %v", fw, svcOf["blocked"])
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestLogDrainNDJSONEqualsArray(t *testing.T) {
	arr := fixture(t, "logs.json")
	var nd bytes.Buffer
	entries, err := split(arr)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		var c bytes.Buffer
		_ = json.Compact(&c, e)
		nd.Write(c.Bytes())
		nd.WriteByte('\n')
	}
	a, _ := Convert(integ, arr)
	b, err := Convert(integ, nd.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if a.Counts[KindLog] != b.Counts[KindLog] || len(a.Logs.ResourceLogs) != len(b.Logs.ResourceLogs) {
		t.Errorf("array %v vs ndjson %v", a.Counts, b.Counts)
	}
}

func TestSpeedInsightsDrain(t *testing.T) {
	res, err := Convert(integ, fixture(t, "speed.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[KindSpeed] != 3 || len(res.RUM) != 2 {
		t.Fatalf("counts=%v batches=%d", res.Counts, len(res.RUM))
	}
	var events []*rumpb.RumEvent
	for _, b := range res.RUM {
		if b.Resource.App != "shop-frontend" || b.Resource.Platform != rumpb.Platform_PLATFORM_WEB || b.Resource.Attributes["cloud.provider"] != "vercel" {
			t.Errorf("resource: %+v", b.Resource)
		}
		events = append(events, b.Events...)
	}
	byName := map[string]*rumpb.RumEvent{}
	for _, e := range events {
		if e.Type != rumpb.EventType_VITAL {
			t.Errorf("type %v", e.Type)
		}
		byName[e.GetVital().GetName()] = e
	}
	cls := byName["CLS"]
	if cls.ViewName != "/dashboard/:id" || cls.GetVital().GetValue() != 0.1 || cls.GetVital().GetRating() != "good" || cls.Attributes["geo.country"] != "TR" ||
		cls.Connection != "4g" || cls.TsMs != 1694705400000 || cls.AnonymousId == "" || cls.AnonymousId == "12345" || cls.SessionId == "" {
		t.Errorf("CLS: %+v", cls)
	}
	lcp := byName["LCP"]
	if lcp.ViewName != "/home" || lcp.GetVital().GetValue() != 2650 || lcp.GetVital().GetRating() != "needs-improvement" {
		t.Errorf("LCP: %+v", lcp)
	}
	if byName["INP"].SessionId != lcp.SessionId || byName["INP"].AnonymousId != lcp.AnonymousId {
		t.Error("same device, same day → same session")
	}
	if cls.AnonymousId == lcp.AnonymousId {
		t.Error("different devices must not share ids")
	}
	// Seconds mode.
	sec := *integ
	sec.VitalsInS = true
	res2, _ := Convert(&sec, []byte(`{"schema":"vercel.speed_insights.v1","projectId":"x","metricType":"LCP","value":2.5,"path":"/a"}`))
	if v := res2.RUM[0].Events[0].GetVital().GetValue(); v != 2500 {
		t.Errorf("seconds mode LCP=%v", v)
	}
}

func TestWebAnalyticsDrain(t *testing.T) {
	res, err := Convert(integ, fixture(t, "analytics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[KindAnalytics] != 2 || len(res.RUM) != 1 {
		t.Fatalf("counts=%v batches=%d", res.Counts, len(res.RUM))
	}
	b := res.RUM[0]
	if b.Resource.App != "shop-frontend" || b.Resource.Version != "dpl_2YZ" || b.Resource.Device.GetType() != "desktop" || b.Resource.Browser.GetName() != "Firefox" {
		t.Errorf("resource: %+v", b.Resource)
	}
	pv, ev := b.Events[0], b.Events[1]
	if pv.Type != rumpb.EventType_VIEW_START || pv.ViewName != "/products/:id" || pv.GetView().GetUrl() != "https://example.com/products/123?utm_source&utm_medium" ||
		pv.GetView().GetReferrer() != "https://google.com/search?q" || pv.ViewId == "" || pv.Attributes["geo.country"] != "DE" {
		t.Errorf("pageview: %+v", pv)
	}
	if ev.Type != rumpb.EventType_CUSTOM || ev.GetCustom().GetName() != "button_click" || ev.GetCustom().GetProps()["button"] != "signup" ||
		ev.GetCustom().GetProps()["n"] != "2" || ev.ViewName != "/signup" {
		t.Errorf("event: %+v", ev)
	}
	if pv.SessionId != ev.SessionId || pv.AnonymousId != ev.AnonymousId || pv.AnonymousId == "67890" {
		t.Error("hashed ids must be stable per device and never the raw device id")
	}
	// The salt is the TaskTrooper project: another project gets other ids.
	other := *integ
	other.ProjectID = 99
	res2, _ := Convert(&other, fixture(t, "analytics.json"))
	if res2.RUM[0].Events[0].AnonymousId == pv.AnonymousId {
		t.Error("ids must be salted per project")
	}
}

func TestTraceDrainJSON(t *testing.T) {
	res, err := Convert(integ, fixture(t, "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[KindTrace] != 1 || len(res.Traces) != 1 {
		t.Fatalf("counts=%v", res.Counts)
	}
	rs := res.Traces[0].ResourceSpans[0]
	ra := attrs(rs.Resource.Attributes)
	if ra["service.name"] != "shop-api" || ra["cloud.provider"] != "vercel" || ra["trooper.integration.id"] != "4" {
		t.Errorf("resource: %v", ra)
	}
	spans := rs.ScopeSpans[0].Spans
	if spans[0].Kind != tracepb.Span_SPAN_KIND_SERVER || spans[1].Kind != tracepb.Span_SPAN_KIND_CLIENT ||
		hex.EncodeToString(spans[0].TraceId) != "7bba9f33312b3dbb8b2c2c62bb7abe2d" || hex.EncodeToString(spans[1].ParentSpanId) != "086e83747d0e381e" ||
		spans[0].Status.GetCode() != tracepb.Status_STATUS_CODE_ERROR || spans[0].EndTimeUnixNano-spans[0].StartTimeUnixNano != 150_000_000 {
		t.Errorf("spans: %+v", spans)
	}
}

func TestMixedAndBadBodies(t *testing.T) {
	body := append(append(fixture(t, "traces.json"), '\n'), []byte(`{"source":"lambda","projectName":"my-app","message":"hi","timestamp":1}`+"\n")...)
	res, err := Convert(integ, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[KindTrace] != 1 || res.Counts[KindLog] != 1 {
		t.Errorf("mixed counts=%v", res.Counts)
	}
	if _, err := Convert(integ, []byte("  ")); err != ErrEmpty {
		t.Errorf("empty: %v", err)
	}
	if _, err := Convert(integ, []byte("[{")); err == nil {
		t.Error("broken JSON must fail")
	}
}

func BenchmarkLogDrain(b *testing.B) {
	body, _ := os.ReadFile("testdata/logs.json")
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Convert(integ, body)
	}
}
