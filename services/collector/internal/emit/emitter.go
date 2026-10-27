package emit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Options configure the OTLP exporter.
type Options struct {
	// MetricsURL is the full OTLP/HTTP endpoint (…/v1/metrics).
	MetricsURL string
	// Key goes into the x-trooper-key header.
	Key string
	// Interval is the export interval (default 15s, matching the scrape interval).
	Interval time.Duration
	Version  string
	Logger   *slog.Logger
	// Reader overrides the periodic OTLP reader (tests).
	Reader sdkmetric.Reader
	// Resource adds resource attributes (host / cluster modes: host.name, k8s.cluster.name, …).
	Resource []attribute.KeyValue
}

// Emitter holds the latest gauge snapshot per target and the delta counters.
// A target's snapshot is replaced on every scrape, so series a target stops reporting
// (dropped query, deleted queue) disappear from the next export.
type Emitter struct {
	provider *sdkmetric.MeterProvider
	log      *slog.Logger
	gauges   map[string]metric.Float64ObservableGauge
	counters map[string]metric.Float64Counter
	reg      metric.Registration

	mu        sync.Mutex
	snapshots map[string][]Sample
	unknown   map[string]bool
}

// Temporality: sums are delta (each export carries what happened since the previous one),
// gauges are unaffected.
func temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	switch k {
	case sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindObservableCounter, sdkmetric.InstrumentKindHistogram:
		return metricdata.DeltaTemporality
	}
	return metricdata.CumulativeTemporality
}

// New builds the meter provider + exporter and registers every contract instrument.
func New(ctx context.Context, o Options) (*Emitter, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = 15 * time.Second
	}
	reader := o.Reader
	if reader == nil {
		exp, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(o.MetricsURL),
			otlpmetrichttp.WithHeaders(map[string]string{"x-trooper-key": o.Key}),
			otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
			otlpmetrichttp.WithTemporalitySelector(temporality),
			otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
				Enabled: true, InitialInterval: time.Second, MaxInterval: 60 * time.Second, MaxElapsedTime: 2 * time.Minute,
			}),
		)
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		reader = sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(o.Interval), sdkmetric.WithTimeout(30*time.Second))
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(append([]attribute.KeyValue{
		semconv.ServiceName("trooper-agent"),
		semconv.ServiceVersion(o.Version),
	}, o.Resource...)...))
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	e := &Emitter{
		provider:  mp,
		log:       o.Logger,
		gauges:    map[string]metric.Float64ObservableGauge{},
		counters:  map[string]metric.Float64Counter{},
		snapshots: map[string][]Sample{},
		unknown:   map[string]bool{},
	}
	m := mp.Meter("github.com/tasktrooper/telemetry/services/collector")
	obs := make([]metric.Observable, 0, len(GaugeNames))
	for _, n := range GaugeNames {
		g, err := m.Float64ObservableGauge(n, metric.WithUnit(units[n]))
		if err != nil {
			return nil, err
		}
		e.gauges[n] = g
		obs = append(obs, g)
	}
	for _, n := range CounterNames {
		c, err := m.Float64Counter(n, metric.WithUnit(units[n]))
		if err != nil {
			return nil, err
		}
		e.counters[n] = c
	}
	e.reg, err = m.RegisterCallback(e.observe, obs...)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Emitter) observe(_ context.Context, o metric.Observer) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, samples := range e.snapshots {
		for _, s := range samples {
			g, ok := e.gauges[s.Name]
			if !ok {
				e.warnUnknown(s.Name)
				continue
			}
			o.ObserveFloat64(g, s.Value, metric.WithAttributeSet(s.Attrs))
		}
	}
	return nil
}

func (e *Emitter) warnUnknown(name string) {
	if !e.unknown[name] {
		e.unknown[name] = true
		e.log.Warn("dropping unknown metric", "metric", name)
	}
}

// Publish stores target's gauges (replacing the previous snapshot) and adds its counts.
func (e *Emitter) Publish(ctx context.Context, target string, r Result) {
	e.mu.Lock()
	e.snapshots[target] = r.Gauges
	for _, s := range r.Counts {
		if _, ok := e.counters[s.Name]; !ok {
			e.warnUnknown(s.Name)
		}
	}
	e.mu.Unlock()
	for _, s := range r.Counts {
		if c, ok := e.counters[s.Name]; ok && s.Value > 0 {
			c.Add(ctx, s.Value, metric.WithAttributeSet(s.Attrs))
		}
	}
}

// Remove drops a target's snapshot (target deleted from config).
func (e *Emitter) Remove(target string) {
	e.mu.Lock()
	delete(e.snapshots, target)
	e.mu.Unlock()
}

// Snapshot returns a copy of target's current gauges (tests, --once output).
func (e *Emitter) Snapshot(target string) []Sample {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Sample(nil), e.snapshots[target]...)
}

// Flush exports immediately.
func (e *Emitter) Flush(ctx context.Context) error { return e.provider.ForceFlush(ctx) }

// Shutdown flushes and stops the exporter.
func (e *Emitter) Shutdown(ctx context.Context) error {
	if e.reg != nil {
		_ = e.reg.Unregister()
	}
	return e.provider.Shutdown(ctx)
}

// Attr helpers.

// DBAttrs are the attributes every trooper.db.* gauge carries.
func DBAttrs(id, name, system, node, role string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrDBID, id),
		attribute.String(AttrDBName, name),
		attribute.String(AttrDBSystem, system),
		attribute.String(AttrDBNode, node),
		attribute.String(AttrDBRole, role),
	}
}

// LagAttrs are the attributes of trooper.broker.lag & co.
func LagAttrs(cluster, system, destination, group, partition string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrCluster, cluster),
		attribute.String(AttrMsgSystem, system),
		attribute.String(AttrDestination, destination),
		attribute.String(AttrGroup, group),
		attribute.String(AttrPartition, partition),
	}
}

// GroupAttrs are the attributes of trooper.broker.group.*.
func GroupAttrs(cluster, system, group, state string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrCluster, cluster),
		attribute.String(AttrMsgSystem, system),
		attribute.String(AttrGroup, group),
		attribute.String(AttrGroupState, state),
	}
}

// NodeAttrs are the attributes of trooper.broker.node.*.
func NodeAttrs(cluster, system, node string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrCluster, cluster),
		attribute.String(AttrMsgSystem, system),
		attribute.String(AttrBrokerNode, node),
	}
}

// TopicAttrs are the attributes of trooper.broker.topic.*.
func TopicAttrs(cluster, system, destination string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrCluster, cluster),
		attribute.String(AttrMsgSystem, system),
		attribute.String(AttrDestination, destination),
	}
}

// UpAttrs are the attributes of trooper.collector.up.
func UpAttrs(target, kind string) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String(AttrTarget, target), attribute.String(AttrTargetKind, kind)}
}
