// Package trooper is the TaskTrooper Telemetry distribution of OpenTelemetry for Go.
//
// It configures the global OTel TracerProvider, MeterProvider and LoggerProvider to export
// OTLP/HTTP protobuf to the TaskTrooper ingest gateway, and adds the trooper.* semantic
// conventions (route template, entry route, caller, consumer attempt/DLQ, sample rate).
//
// Framework integrations live in separate modules under
// github.com/tasktrooper/telemetry-go/integrations/<name>.
package trooper

import "go.opentelemetry.io/otel/attribute"

// Version is the version of the trooper-go distribution.
const Version = "0.1.0"

// DistroName is reported as telemetry.distro.name.
const DistroName = "trooper-go"

// ScopeName is the instrumentation scope used by spans created by this module.
const ScopeName = "github.com/tasktrooper/telemetry-go"

// Attribute keys defined by the TaskTrooper semantic conventions (.ai/contracts.md §4).
const (
	// AttrRoute is the normalized route template of a server span (e.g. /v1/orders/:id).
	AttrRoute = attribute.Key("trooper.route")
	// AttrEntryRoute is the route (or consumer process name) of the entry span, on every span of a request.
	AttrEntryRoute = attribute.Key("trooper.entry.route")
	// AttrEntryMethod is the HTTP/RPC method of the entry span, on every span of a request.
	AttrEntryMethod = attribute.Key("trooper.entry.method")
	// AttrProcess is the consumer process name on consumer spans.
	AttrProcess = attribute.Key("trooper.process")
	// AttrCaller is the calling service name on server/consumer entry spans (from baggage tt.svc).
	AttrCaller = attribute.Key("trooper.caller")
	// AttrConsumerAttempt is the delivery attempt (1 = first delivery).
	AttrConsumerAttempt = attribute.Key("trooper.consumer.attempt")
	// AttrConsumerDLQ marks a message that was sent to a dead letter queue.
	AttrConsumerDLQ = attribute.Key("trooper.consumer.dlq")
	// AttrSampleRate is the head sampling probability, set on entry spans.
	AttrSampleRate = attribute.Key("trooper.sample_rate")

	// BaggageServiceKey is the W3C baggage member carrying the calling service name.
	BaggageServiceKey = "tt.svc"
)
