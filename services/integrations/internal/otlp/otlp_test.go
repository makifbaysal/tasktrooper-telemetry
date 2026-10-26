package otlp

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

func TestParseKeys(t *testing.T) {
	tests := []struct {
		in      string
		want    map[int64]string
		wantErr bool
	}{
		{"", map[int64]string{}, false},
		{"1:a, 9301:b", map[int64]string{1: "a", 9301: "b"}, false},
		{"x:a", nil, true},
		{"1:", nil, true},
	}
	for _, tt := range tests {
		got, err := ParseKeys(tt.in)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%q: err %v", tt.in, err)
		}
		if err == nil && len(got) != len(tt.want) {
			t.Fatalf("%q: got %v", tt.in, got)
		}
		for k, v := range tt.want {
			if got[k] != v {
				t.Fatalf("%q: got %v", tt.in, got)
			}
		}
	}
}

func TestParseDSN(t *testing.T) {
	u, k, p, err := ParseDSN("http://tt_dev_ingest_key@localhost:4318/1")
	if err != nil || u != "http://localhost:4318/v1/metrics" || k != "tt_dev_ingest_key" || p != 1 {
		t.Fatalf("%s %s %d %v", u, k, p, err)
	}
}

func TestSendBuildsResourcePerRegionAndRetries(t *testing.T) {
	var calls atomic.Int32
	var got colmetricspb.ExportMetricsServiceRequest
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		key = r.Header.Get("x-trooper-key")
		zr, _ := gzip.NewReader(r.Body)
		b, _ := io.ReadAll(zr)
		_ = proto.Unmarshal(b, &got)
	}))
	defer srv.Close()
	s := &Sender{MetricsURL: srv.URL + "/v1/metrics", Keys: map[int64]string{7: "k7"}, Version: "test"}
	ts := time.Unix(1_700_000_040, 0)
	err := s.Send(context.Background(), Target{ProjectID: 7, IntegrationID: 3, Provider: "aws"}, []model.Sample{
		{Name: model.DBCPU, Value: 0.5, TS: ts, Region: "eu-central-1", Attrs: map[string]string{"trooper.db.id": "aws-a"}},
		{Name: model.DBCPU, Value: 0.6, TS: ts, Region: "eu-central-1", Attrs: map[string]string{"trooper.db.id": "aws-b"}},
		{Name: model.BrokerLag, Value: 9, TS: ts, Region: "us-east-1", Attrs: map[string]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || key != "k7" {
		t.Fatalf("calls %d key %q", calls.Load(), key)
	}
	if len(got.ResourceMetrics) != 2 {
		t.Fatalf("resources: %d", len(got.ResourceMetrics))
	}
	rm := got.ResourceMetrics[0]
	ra := map[string]string{}
	for _, a := range rm.Resource.Attributes {
		ra[a.Key] = a.Value.GetStringValue()
	}
	if ra["service.name"] != "trooper-cloud" || ra["cloud.region"] != "eu-central-1" || ra["trooper.integration.id"] != "3" || ra["cloud.provider"] != "aws" {
		t.Fatalf("resource attrs %v", ra)
	}
	dps := rm.ScopeMetrics[0].Metrics[0].GetGauge().DataPoints
	if len(dps) != 2 || dps[0].TimeUnixNano != uint64(ts.UnixNano()) {
		t.Fatalf("dps %v", dps)
	}
	if err := s.Send(context.Background(), Target{ProjectID: 8}, []model.Sample{{Name: "x"}}); err != ErrNoKey {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
}
