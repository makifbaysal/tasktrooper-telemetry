package flatten

import (
	"math"
	"testing"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestMessagingSystem(t *testing.T) {
	for in, want := range map[string]string{
		"kafka": "kafka", "Kafka": "kafka", "rabbitmq": "rabbitmq", "AWS.SQS": "aws_sqs", "aws_sqs": "aws_sqs", "sqs": "aws_sqs",
		"gcp_pubsub": "gcp_pubsub", "google-pubsub": "gcp_pubsub", "pubsub": "gcp_pubsub", "servicebus": "servicebus",
		"ActiveMQ": "activemq", "my.custom-bus": "my_custom_bus", "": "",
	} {
		if got := MessagingSystem(in); got != want {
			t.Errorf("MessagingSystem(%q)=%q want %q", in, got, want)
		}
	}
	r := Spans(tn, traces("worker", span(1, 0, consumer, attrs("messaging.system", "AWS.SQS", "messaging.destination.name", "emails"))), nil)[0]
	if r.MessagingSystem != "aws_sqs" || r.Topic != "emails" {
		t.Errorf("span messaging: %+v", r)
	}
	r = Spans(tn, traces("api", span(1, 0, server, attrs("http.route", "/a"))), nil)[0]
	if r.MessagingSystem != "" {
		t.Errorf("non-messaging span got %q", r.MessagingSystem)
	}
}

// Cloud integrations (trooper-cloud) put the entity attributes on the resource; topic metrics and queue age.
func TestBrokerTopicsAndOldestAge(t *testing.T) {
	gauge := func(name string, v float64, a []*commonpb.KeyValue) *metricspb.Metric {
		return &metricspb.Metric{Name: name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			TimeUnixNano: 1_700_000_030_000_000_000, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}, Attributes: a}}}}}
	}
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: attrs("service.name", "trooper-cloud", "trooper.integration.id", "4",
			"cloud.provider", "aws", "cloud.region", "eu-west-1", "trooper.broker.cluster", "aws-orders-sqs", "messaging.system", "aws_sqs")},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			gauge("trooper.broker.lag", 42, attrs("messaging.destination.name", "emails")),
			gauge("trooper.broker.oldest_age_s", 310, attrs("messaging.destination.name", "emails")),
			gauge("trooper.broker.topic.in_rate", 12.5, attrs("messaging.destination.name", "emails")),
			gauge("trooper.broker.topic.size_bytes", 2048, attrs("messaging.destination.name", "emails")),
			gauge("trooper.broker.topic.partitions", 12, attrs("trooper.broker.cluster", "k1", "messaging.destination.name", "orders")),
		}}},
	}}}
	b := NewMetricsBatch()
	b.Add(tn, req)
	if len(b.BrokerLag) != 1 || len(b.BrokerTopics) != 2 || len(b.Metrics) != 0 {
		t.Fatalf("lag=%d topics=%d custom=%d", len(b.BrokerLag), len(b.BrokerTopics), len(b.Metrics))
	}
	for k, r := range b.BrokerLag {
		if k.Cluster != "aws-orders-sqs" || k.Topic != "emails" || k.Group != "" || r.V[0] != 42 || r.V[6] != 310 || !math.IsInf(r.V[1], -1) {
			t.Errorf("lag: %+v %v", k, r.V)
		}
		if n := len(r.Values()); n != countCols(BrokerLagColumns) {
			t.Errorf("lag values=%d cols=%d", n, countCols(BrokerLagColumns))
		}
	}
	for k, r := range b.BrokerTopics {
		switch k.Topic {
		case "emails":
			if k.Cluster != "aws-orders-sqs" || r.V[4] != 12.5 || r.V[6] != 2048 || !math.IsInf(r.V[0], -1) {
				t.Errorf("emails: %+v %v", k, r.V)
			}
		case "orders":
			if k.Cluster != "k1" || r.V[0] != 12 { // data-point attribute wins over the resource
				t.Errorf("orders: %+v %v", k, r.V)
			}
		default:
			t.Errorf("unexpected topic %q", k.Topic)
		}
		if n := len(r.Values()); n != countCols(BrokerTopicColumns) {
			t.Errorf("topic values=%d cols=%d", n, countCols(BrokerTopicColumns))
		}
	}
	if b.Len() != 3 {
		t.Errorf("Len=%d", b.Len())
	}
	b.Reset()
	if b.Len() != 0 {
		t.Errorf("Reset: Len=%d", b.Len())
	}
}

func countCols(s string) int {
	n := 1
	for _, c := range s {
		if c == ',' {
			n++
		}
	}
	return n
}
