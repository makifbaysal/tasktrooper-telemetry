package trooper

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// HeartbeatMetric is the SDK presence signal (.ai/contracts.md §4): a gauge with value 1 and no attributes
// (service, version, environment and instance are on the resource), exported every metric interval even when
// the service has no traffic, so the service shows up in the dashboard as soon as the SDK is started.
const HeartbeatMetric = "trooper.sdk.heartbeat"

// firstExportDelay is when the first metric export happens after Start (contract: ≤ 10 s), instead of after a
// full MetricInterval. A variable for tests.
var firstExportDelay = 5 * time.Second

// startHeartbeat registers the heartbeat gauge on mp and schedules one early export. The returned function
// cancels the early export (call it before mp.Shutdown). Errors are swallowed (counted as component heartbeat).
func startHeartbeat(mp *sdkmetric.MeterProvider, interval, timeout time.Duration) func(context.Context) error {
	stop := func(context.Context) error { return nil }
	_, err := mp.Meter(MeterName).Int64ObservableGauge(HeartbeatMetric,
		metric.WithDescription("1 while the trooper SDK is running (presence signal)"), metric.WithUnit("1"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1)
			return nil
		}))
	if err != nil {
		self.countError("heartbeat")
		return stop
	}
	if interval <= firstExportDelay {
		return stop // the periodic reader exports soon enough
	}
	t := time.AfterFunc(firstExportDelay, func() {
		defer func() {
			if recover() != nil {
				self.countError("heartbeat")
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = mp.ForceFlush(ctx) // export failures are counted by countingMetricExporter
	})
	return func(context.Context) error {
		t.Stop()
		return nil
	}
}

// instanceID returns a random UUIDv4 string for service.instance.id ("" if the system has no randomness).
func instanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
