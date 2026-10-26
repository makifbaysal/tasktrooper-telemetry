// Package otlptest is an in-process OTLP/HTTP metrics receiver for tests.
package otlptest

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Point is one received data point, flattened.
type Point struct {
	Metric      string
	Kind        string // gauge | sum
	Delta       bool   // sums only
	Monotonic   bool
	Value       float64
	Attrs       map[string]string
	ServiceName string
}

// Server records every request to /v1/metrics.
type Server struct {
	*httptest.Server
	mu     sync.Mutex
	points []Point
	keys   []string
	reqs   int
}

// New starts the receiver.
func New() *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/metrics" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = zr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(b, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.reqs++
	s.keys = append(s.keys, r.Header.Get("x-trooper-key"))
	for _, rm := range req.ResourceMetrics {
		svc := attrs(rm.GetResource().GetAttributes())["service.name"]
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				s.points = append(s.points, flatten(m, svc)...)
			}
		}
	}
	s.mu.Unlock()
	out, _ := proto.Marshal(&colmetricspb.ExportMetricsServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = io.Copy(w, bytes.NewReader(out))
}

func flatten(m *metricspb.Metric, svc string) []Point {
	var out []Point
	num := func(dp *metricspb.NumberDataPoint) float64 {
		if v, ok := dp.Value.(*metricspb.NumberDataPoint_AsInt); ok {
			return float64(v.AsInt)
		}
		return dp.GetAsDouble()
	}
	switch d := m.Data.(type) {
	case *metricspb.Metric_Gauge:
		for _, dp := range d.Gauge.DataPoints {
			out = append(out, Point{Metric: m.Name, Kind: "gauge", Value: num(dp), Attrs: attrs(dp.Attributes), ServiceName: svc})
		}
	case *metricspb.Metric_Sum:
		for _, dp := range d.Sum.DataPoints {
			out = append(out, Point{Metric: m.Name, Kind: "sum", Value: num(dp), Attrs: attrs(dp.Attributes), ServiceName: svc,
				Delta:     d.Sum.AggregationTemporality == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
				Monotonic: d.Sum.IsMonotonic})
		}
	}
	return out
}

func attrs(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[kv.Key] = kv.Value.GetStringValue()
	}
	return out
}

// Points returns a copy of everything received.
func (s *Server) Points() []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Point(nil), s.points...)
}

// Keys returns the x-trooper-key header of every request.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

// Find returns points of metric whose attributes include all of match.
func (s *Server) Find(metric string, match map[string]string) []Point {
	var out []Point
	for _, p := range s.Points() {
		if p.Metric != metric {
			continue
		}
		ok := true
		for k, v := range match {
			if p.Attrs[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, p)
		}
	}
	return out
}
