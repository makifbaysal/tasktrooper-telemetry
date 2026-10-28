package otlpx

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/engine"
)

func TestExportInternalCredential(t *testing.T) {
	var got colmetricspb.ExportMetricsServiceRequest
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		zr, _ := gzip.NewReader(r.Body)
		b, _ := io.ReadAll(zr)
		if err := proto.Unmarshal(b, &got); err != nil {
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	e, err := Config(srv.URL, "", "secret", 9501)
	if err != nil || e == nil {
		t.Fatal(err)
	}
	e.RunID, e.Region = "77", "eu-central"
	w := engine.NewStats()
	w.Requests, w.Errors = 3, 1
	w.Steps["GET /a"] = &engine.Counts{Requests: 3, Errors: 1}
	for _, ms := range []float64{3, 40, 2000} {
		w.Steps["GET /a"].Hist.Add(ms)
	}
	now := time.Now()
	if err := e.Export(context.Background(), w, 12, now.Add(-5*time.Second), now); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("x-trooper-internal") != "secret" || hdr.Get("x-trooper-project") != "9501" || hdr.Get("x-trooper-key") != "" {
		t.Fatalf("headers: %v", hdr)
	}
	rm := got.ResourceMetrics[0]
	if rm.Resource.Attributes[0].GetValue().GetStringValue() != "loadtest" {
		t.Fatalf("service.name: %v", rm.Resource.Attributes)
	}
	names := map[string]bool{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		names[m.Name] = true
		if m.Name == "trooper.loadtest.duration_ms" {
			dp := m.GetHistogram().DataPoints[0]
			var sum uint64
			for _, c := range dp.BucketCounts {
				sum += c
			}
			if dp.Count != 3 || sum != 3 || dp.BucketCounts[0] != 1 || dp.BucketCounts[3] != 1 || dp.BucketCounts[13] != 1 {
				t.Fatalf("histogram: %v", dp)
			}
			attrs := map[string]string{}
			for _, a := range dp.Attributes {
				attrs[a.Key] = a.GetValue().GetStringValue()
			}
			if attrs["trooper.loadtest.run"] != "77" || attrs["trooper.loadtest.step"] != "GET /a" || attrs["trooper.loadtest.region"] != "eu-central" {
				t.Fatalf("attrs: %v", attrs)
			}
		}
		if m.Name == "trooper.loadtest.requests" && m.GetSum().DataPoints[0].GetAsDouble() != 3 {
			t.Fatalf("requests: %v", m)
		}
	}
	for _, n := range []string{"trooper.loadtest.requests", "trooper.loadtest.errors", "trooper.loadtest.duration_ms", "trooper.loadtest.vus"} {
		if !names[n] {
			t.Fatalf("missing %s in %v", n, names)
		}
	}
}

func TestConfigDSN(t *testing.T) {
	e, err := Config("", "http://tt_dev_ingest_key@localhost:4318/1", "", 1)
	if err != nil || e.URL != "http://localhost:4318/v1/metrics" || e.Key != "tt_dev_ingest_key" {
		t.Fatalf("%+v %v", e, err)
	}
	if e, _ := Config("", "", "", 1); e != nil {
		t.Fatal("no target → nil exporter")
	}
	if _, err := Config("http://x", "", "", 1); err == nil {
		t.Fatal("no credential must fail")
	}
}
