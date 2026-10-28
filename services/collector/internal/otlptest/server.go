// Package otlptest is an in-process OTLP/HTTP metrics + logs receiver for tests.
package otlptest

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
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
	// Resource holds every resource attribute.
	Resource map[string]string
}

// LogRecord is one received log record, flattened.
type LogRecord struct {
	Body, SeverityText string
	Severity           int32
	TimeUnixNano       uint64
	Attrs, Resource    map[string]string
}

// Server records every request to /v1/metrics and /v1/logs.
type Server struct {
	*httptest.Server
	mu     sync.Mutex
	points []Point
	logs   []LogRecord
	keys   []string
	reqs   int
}

// New starts the receiver.
func New() *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil, false
		}
		body = zr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return b, true
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	var req collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(b, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.reqs++
	s.keys = append(s.keys, r.Header.Get("x-trooper-key"))
	for _, rl := range req.ResourceLogs {
		res := attrs(rl.GetResource().GetAttributes())
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				s.logs = append(s.logs, LogRecord{Body: anyString(lr.Body), SeverityText: lr.SeverityText,
					Severity: int32(lr.SeverityNumber), TimeUnixNano: lr.TimeUnixNano, Attrs: attrs(lr.Attributes), Resource: res})
			}
		}
	}
	s.mu.Unlock()
	out, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// Logs returns a copy of every received log record.
func (s *Server) Logs() []LogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LogRecord(nil), s.logs...)
}

func anyString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	}
	return ""
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/logs" && r.Method == http.MethodPost {
		s.handleLogs(w, r)
		return
	}
	if r.URL.Path != "/v1/metrics" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	b, ok := readBody(w, r)
	if !ok {
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
		res := attrs(rm.GetResource().GetAttributes())
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, p := range flatten(m, res["service.name"]) {
					p.Resource = res
					s.points = append(s.points, p)
				}
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
		out[kv.Key] = anyString(kv.Value)
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
