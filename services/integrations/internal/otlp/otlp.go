// Package otlp sends trooper-cloud samples to ingest-gw as OTLP/HTTP protobuf gauges (contracts §2, §5).
//
// Unlike trooper-agent (OTel SDK, one resource), every request here is built by hand: each integration
// carries its own resource (trooper.integration.id, cloud.provider, cloud.region) and data points keep the
// cloud provider's timestamp, so a CloudWatch value for 12:04 lands in the 12:04 minute even if read at 12:06.
package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// Sender posts to <base>/v1/metrics. With InternalToken it authenticates with the internal service credential
// (x-trooper-internal + x-trooper-project, contracts §16); otherwise, or when ingest-gw rejects the credential with
// 401, it falls back to the per-project ingest key.
type Sender struct {
	MetricsURL string
	// InternalToken is TT_INTERNAL_TOKEN (shared with ingest-gw); empty → ingest keys only.
	InternalToken string
	// Keys maps project id → ingest key (TT_CLOUD_INGEST_KEYS; TT_DSN's key for its project).
	Keys    map[int64]string
	Version string
	HTTP    *http.Client
	// MaxAttempts on 429/5xx (default 5).
	MaxAttempts int
}

// Target identifies the resource of a batch.
type Target struct {
	ProjectID     int64
	IntegrationID int64
	Provider      string
	Name          string
}

// ParseDSN parses TT_DSN (`<scheme>://<key>@<host>[:port]/<project_id>`) into the metrics URL, key and project.
func ParseDSN(s string) (metricsURL, key string, project int64, err error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", "", 0, fmt.Errorf("dsn: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", 0, errors.New("dsn: want http(s)://<key>@host[:port]/<project>")
	}
	if u.User != nil {
		key = u.User.Username()
	}
	project, _ = strconv.ParseInt(strings.Trim(u.Path, "/"), 10, 64)
	base := url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/v1/metrics"}
	return base.String(), key, project, nil
}

// ParseKeys parses `projectId:key,projectId:key`.
func ParseKeys(s string) (map[int64]string, error) {
	out := map[int64]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, k, ok := strings.Cut(part, ":")
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if !ok || err != nil || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("TT_CLOUD_INGEST_KEYS: bad entry %q (want projectId:key)", redact(part))
		}
		out[id] = strings.TrimSpace(k)
	}
	return out, nil
}

func redact(s string) string {
	if p, _, ok := strings.Cut(s, ":"); ok {
		return p + ":***"
	}
	return "***"
}

// ErrNoKey means no ingest key is configured for the project.
var ErrNoKey = errors.New("no ingest key for project (set TT_CLOUD_INGEST_KEYS=<projectId>:<key>)")

// Build turns samples into one export request: one ResourceMetrics per region, one gauge per metric name.
func Build(t Target, version string, samples []model.Sample) *colmetricspb.ExportMetricsServiceRequest {
	byRegion := map[string][]model.Sample{}
	for _, s := range samples {
		byRegion[s.Region] = append(byRegion[s.Region], s)
	}
	regions := make([]string, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	req := &colmetricspb.ExportMetricsServiceRequest{}
	for _, region := range regions {
		res := []*commonpb.KeyValue{
			kv("service.name", "trooper-cloud"),
			kv("service.version", version),
			kv("trooper.integration.id", strconv.FormatInt(t.IntegrationID, 10)),
			kv("trooper.integration.name", t.Name),
			kv("cloud.provider", t.Provider),
		}
		if region != "" {
			res = append(res, kv("cloud.region", region))
		}
		metrics := map[string]*metricspb.Metric{}
		var order []string
		for _, s := range byRegion[region] {
			m := metrics[s.Name]
			if m == nil {
				m = &metricspb.Metric{Name: s.Name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{}}}
				metrics[s.Name] = m
				order = append(order, s.Name)
			}
			g := m.GetGauge()
			g.DataPoints = append(g.DataPoints, &metricspb.NumberDataPoint{
				TimeUnixNano: uint64(s.TS.UnixNano()),
				Attributes:   attrs(s.Attrs),
				Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: s.Value},
			})
		}
		sm := &metricspb.ScopeMetrics{Scope: &commonpb.InstrumentationScope{Name: "github.com/tasktrooper/telemetry/services/integrations", Version: version}}
		for _, n := range order {
			sm.Metrics = append(sm.Metrics, metrics[n])
		}
		req.ResourceMetrics = append(req.ResourceMetrics, &metricspb.ResourceMetrics{
			Resource:     &resourcepb.Resource{Attributes: res},
			ScopeMetrics: []*metricspb.ScopeMetrics{sm},
		})
	}
	return req
}

// Send exports samples for one integration; chunks of 5000 points.
func (s *Sender) Send(ctx context.Context, t Target, samples []model.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	key := s.Keys[t.ProjectID]
	if key == "" && s.InternalToken == "" {
		return ErrNoKey
	}
	const chunk = 5000
	for i := 0; i < len(samples); i += chunk {
		j := min(i+chunk, len(samples))
		req := Build(t, s.Version, samples[i:j])
		var err error
		if s.InternalToken != "" {
			err = s.post(ctx, internalAuth(s.InternalToken, t.ProjectID), req)
			var he *httpErr
			if errors.As(err, &he) && he.code == http.StatusUnauthorized && key != "" {
				err = s.post(ctx, keyAuth(key), req) // ingest-gw without (or with another) TT_INTERNAL_TOKEN
			}
		} else {
			err = s.post(ctx, keyAuth(key), req)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// authFn sets the authentication headers of one request.
type authFn func(h http.Header)

func keyAuth(key string) authFn { return func(h http.Header) { h.Set("x-trooper-key", key) } }

func internalAuth(token string, project int64) authFn {
	return func(h http.Header) {
		h.Set("x-trooper-internal", token)
		h.Set("x-trooper-project", strconv.FormatInt(project, 10))
	}
}

func (s *Sender) post(ctx context.Context, auth authFn, req *colmetricspb.ExportMetricsServiceRequest) error {
	raw, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	body := buf.Bytes()
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	attempts := s.MaxAttempts
	if attempts <= 0 {
		attempts = 5
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			d := time.Second << (i - 1)
			d += time.Duration(rand.Int64N(int64(d / 2)))
			if ra := retryAfter(last); ra > 0 {
				d = ra
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(d, 60*time.Second)):
			}
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.MetricsURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		r.Header.Set("Content-Type", "application/x-protobuf")
		r.Header.Set("Content-Encoding", "gzip")
		auth(r.Header)
		resp, err := hc.Do(r)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = fmt.Errorf("otlp export: %w", err)
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		switch {
		case resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = &httpErr{code: resp.StatusCode, msg: string(msg), retryAfter: resp.Header.Get("Retry-After")}
		default:
			return &httpErr{code: resp.StatusCode, msg: string(msg)}
		}
	}
	return last
}

type httpErr struct {
	code       int
	msg        string
	retryAfter string
}

func (e *httpErr) Error() string {
	return fmt.Sprintf("otlp export: HTTP %d: %s", e.code, strings.TrimSpace(e.msg))
}

func retryAfter(err error) time.Duration {
	var he *httpErr
	if errors.As(err, &he) && he.retryAfter != "" {
		if s, err := strconv.Atoi(he.retryAfter); err == nil {
			return time.Duration(s) * time.Second
		}
	}
	return 0
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func attrs(m map[string]string) []*commonpb.KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*commonpb.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, kv(k, m[k]))
	}
	return out
}
