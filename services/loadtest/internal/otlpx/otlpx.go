// Package otlpx exports runner metrics to ingest-gw as OTLP/HTTP protobuf (contracts §14):
// trooper.loadtest.requests / .errors (delta sums), trooper.loadtest.duration_ms (delta histogram) and
// trooper.loadtest.vus (gauge); resource service.name=loadtest, points carry trooper.loadtest.run / .step / .region.
package otlpx

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tasktrooper/telemetry/services/loadtest/internal/engine"
)

// Bounds are the explicit histogram bounds (ms) of trooper.loadtest.duration_ms.
var Bounds = []float64{5, 10, 25, 50, 75, 100, 150, 250, 400, 500, 750, 1000, 1500, 2500, 5000, 10000}

// Exporter posts to <base>/v1/metrics.
type Exporter struct {
	URL string // full /v1/metrics URL
	// Auth: InternalToken + Project (x-trooper-internal / x-trooper-project, contracts §16) or Key (x-trooper-key).
	InternalToken string
	Project       int64
	Key           string
	RunID         string
	Region        string
	Version       string
	HTTP          *http.Client
}

// Config resolves the exporter target from TT_LOADTEST_INGEST_URL / TT_DSN. It returns nil when neither is set.
func Config(ingestURL, dsn, internalToken string, project int64) (*Exporter, error) {
	e := &Exporter{InternalToken: internalToken, Project: project}
	if dsn != "" {
		u, err := url.Parse(strings.TrimSpace(dsn))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("TT_DSN: want http(s)://<key>@host[:port]/<project>")
		}
		if u.User != nil {
			e.Key = u.User.Username()
		}
		e.URL = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/v1/metrics"}).String()
	}
	if ingestURL != "" {
		e.URL = strings.TrimRight(ingestURL, "/") + "/v1/metrics"
	}
	if e.URL == "" {
		return nil, nil
	}
	if e.InternalToken == "" && e.Key == "" {
		return nil, errors.New("otlp: set TT_INTERNAL_TOKEN (internal credential) or a key in TT_DSN")
	}
	return e, nil
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// Build turns one tick window into an export request.
func (e *Exporter) Build(w *engine.Stats, vus float64, from, to time.Time) *colmetricspb.ExportMetricsServiceRequest {
	start, end := uint64(from.UnixNano()), uint64(to.UnixNano())
	base := func(step string) []*commonpb.KeyValue {
		a := []*commonpb.KeyValue{kv("trooper.loadtest.run", e.RunID)}
		if e.Region != "" {
			a = append(a, kv("trooper.loadtest.region", e.Region))
		}
		if step != "" {
			a = append(a, kv("trooper.loadtest.step", step))
		}
		return a
	}
	sum := func(name string) *metricspb.Metric {
		return &metricspb.Metric{Name: name, Unit: "1", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, IsMonotonic: true}}}
	}
	reqs, errs := sum("trooper.loadtest.requests"), sum("trooper.loadtest.errors")
	dur := &metricspb.Metric{Name: "trooper.loadtest.duration_ms", Unit: "ms", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA}}}
	steps := make([]string, 0, len(w.Steps))
	for k := range w.Steps {
		steps = append(steps, k)
	}
	sort.Strings(steps)
	for _, k := range steps {
		c := w.Steps[k]
		reqs.GetSum().DataPoints = append(reqs.GetSum().DataPoints, &metricspb.NumberDataPoint{StartTimeUnixNano: start, TimeUnixNano: end,
			Attributes: base(k), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: float64(c.Requests)}})
		errs.GetSum().DataPoints = append(errs.GetSum().DataPoints, &metricspb.NumberDataPoint{StartTimeUnixNano: start, TimeUnixNano: end,
			Attributes: base(k), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: float64(c.Errors)}})
		counts := make([]uint64, len(Bounds)+1)
		for b, n := range c.Hist.B {
			v := bucketValue(b)
			i := sort.SearchFloat64s(Bounds, v) // first bound ≥ v
			counts[i] += n
		}
		mx := c.Hist.MaxMs
		dur.GetHistogram().DataPoints = append(dur.GetHistogram().DataPoints, &metricspb.HistogramDataPoint{StartTimeUnixNano: start, TimeUnixNano: end,
			Attributes: base(k), Count: c.Hist.Count, Sum: &c.Hist.SumMs, Max: &mx, BucketCounts: counts, ExplicitBounds: Bounds})
	}
	vu := &metricspb.Metric{Name: "trooper.loadtest.vus", Unit: "1", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
		DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: end, Attributes: base(""), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: vus}}}}}}
	metrics := []*metricspb.Metric{vu}
	if len(steps) > 0 {
		metrics = append(metrics, reqs, errs, dur)
	}
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "loadtest"), kv("service.version", e.Version),
			kv("trooper.loadtest.run", e.RunID)}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "github.com/tasktrooper/telemetry/services/loadtest", Version: e.Version},
			Metrics: metrics}},
	}}}
}

// bucketValue is the representative value (ms) of an engine histogram bucket.
func bucketValue(b int) float64 {
	if b == 0 {
		return 0.001
	}
	return math.Pow(1.02, float64(b)) / 1000
}

// Export sends one window (2 attempts; telemetry is best effort and must not stall the run).
func (e *Exporter) Export(ctx context.Context, w *engine.Stats, vus float64, from, to time.Time) error {
	raw, err := proto.Marshal(e.Build(w, vus, from, to))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	hc := e.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	var last error
	for i := 0; i < 2; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "gzip")
		if e.InternalToken != "" {
			req.Header.Set("x-trooper-internal", e.InternalToken)
			req.Header.Set("x-trooper-project", strconv.FormatInt(e.Project, 10))
		} else {
			req.Header.Set("x-trooper-key", e.Key)
		}
		resp, err := hc.Do(req)
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		last = fmt.Errorf("otlp export: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}
