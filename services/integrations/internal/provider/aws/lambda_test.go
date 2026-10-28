package aws

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/fakes"
)

func lambdaFixture(name string, d map[string]string) (float64, bool) {
	v, ok := map[string]float64{
		"Invocations/Sum/orders-api":                     1200,
		"Errors/Sum/orders-api":                          12,
		"Throttles/Sum/orders-api":                       0,
		"Duration/Average/orders-api":                    85.5,
		"Duration/Maximum/orders-api":                    910,
		"Duration/p95/orders-api":                        240,
		"Duration/p99/orders-api":                        700,
		"ConcurrentExecutions/Maximum/orders-api":        14,
		"Invocations/Sum/orders-worker":                  300,
		"IteratorAge/Maximum/orders-worker":              4200,
		"IteratorAge/Maximum/orders-api":                 999, // not queried: no stream trigger
		"ApproximateNumberOfMessagesVisible/Maximum/xxx": 1,
	}[name+"/"+d["_stat"]+"/"+d["FunctionName"]]
	return v, ok
}

func TestLambdaDiscoverAndCollect(t *testing.T) {
	ts := time.Date(2026, 10, 25, 12, 4, 0, 0, time.UTC)
	f := fakes.NewAWS(t, ts, lambdaFixture)
	defer f.Close()
	p, err := New(context.Background(), []byte(`{"regions":["eu-central-1"],"services":["lambda"]}`), nil, Options{Endpoint: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Databases)+len(inv.Brokers) != 0 || inv.Count() != 3 {
		t.Fatalf("only lambda is enabled: %d dbs %d brokers count %d", len(inv.Databases), len(inv.Brokers), inv.Count())
	}
	var got []string
	for _, fn := range inv.Functions {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%d|%d|%s|%v|%s", fn.Slug, fn.Name, fn.Service, fn.Runtime, *fn.MemoryMB, *fn.TimeoutS, fn.Arch, fn.Triggers, fn.URL))
	}
	want := []string{
		"aws-eu-central-1-orders-api|orders-api|orders|python3.12|512|30|arm64|[api http]|https://abc.lambda-url.eu-central-1.on.aws/",
		"aws-eu-central-1-orders-worker|orders-worker|orders-worker|nodejs20.x|1024|60|x86_64|[kinesis:clicks sqs:orders]|",
		"aws-eu-central-1-nightly-report|nightly-report|nightly-report|container|2048|900|x86_64|[schedule:nightly-schedule]|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("functions\n got %v\nwant %v", got, want)
	}
	api := inv.Functions[0]
	if api.Meta["arn"] != "arn:aws:lambda:eu-central-1:1:function:orders-api" || api.Meta["otel"] != true || api.Region != "eu-central-1" {
		t.Errorf("meta %v", api.Meta)
	}
	if tags, _ := api.Meta["tags"].(map[string]string); tags["team"] != "checkout" {
		t.Errorf("tags %v", api.Meta["tags"])
	}
	if fmt.Sprint(api.Meta) != "" && strings.Contains(fmt.Sprint(api.Meta), "do-not-store") {
		t.Fatal("environment values must never be stored")
	}
	if inv.Functions[1].Meta["reservedConcurrency"] != int32(50) {
		t.Errorf("reserved concurrency %v", inv.Functions[1].Meta)
	}

	res, err := p.Collect(context.Background(), inv, ts.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]float64{}
	for _, s := range res.Samples {
		if s.Region != "eu-central-1" || s.Attrs["faas.name"] == "" {
			t.Errorf("sample %+v", s)
		}
		vals[fmt.Sprintf("%s|%s|%s", s.Attrs["trooper.faas.id"], s.Name, s.TS.Format("15:04"))] = s.Value
	}
	exp := map[string]float64{
		"aws-eu-central-1-orders-api|trooper.faas.invocations|12:04":        1200,
		"aws-eu-central-1-orders-api|trooper.faas.invocations|12:03":        -1, // every point of the window is sent
		"aws-eu-central-1-orders-api|trooper.faas.errors|12:04":             12,
		"aws-eu-central-1-orders-api|trooper.faas.throttles|12:04":          0,
		"aws-eu-central-1-orders-api|trooper.faas.duration.avg|12:04":       85.5,
		"aws-eu-central-1-orders-api|trooper.faas.duration.max|12:04":       910,
		"aws-eu-central-1-orders-api|trooper.faas.duration.p95|12:04":       240,
		"aws-eu-central-1-orders-api|trooper.faas.duration.p99|12:04":       700,
		"aws-eu-central-1-orders-api|trooper.faas.concurrency|12:04":        14,
		"aws-eu-central-1-orders-worker|trooper.faas.invocations|12:04":     300,
		"aws-eu-central-1-orders-worker|trooper.faas.iterator_age_ms|12:04": 4200,
	}
	for k, v := range exp {
		if g, ok := vals[k]; !ok || g != v {
			t.Errorf("%s = %v (ok=%v), want %v", k, g, ok, v)
		}
	}
	if _, ok := vals["aws-eu-central-1-orders-api|trooper.faas.iterator_age_ms|12:04"]; ok {
		t.Error("iterator age is only read for stream triggers")
	}
	// 3 functions × 8 queries + 1 IteratorAge (orders-worker).
	if f.Queries != 25 {
		t.Errorf("queries %d", f.Queries)
	}
}

func TestExplicitServicesWithoutLambda(t *testing.T) {
	f := fakes.NewAWS(t, time.Now(), func(string, map[string]string) (float64, bool) { return 0, false })
	defer f.Close()
	p, err := New(context.Background(), []byte(`{"regions":["eu-central-1"],"services":["sqs"]}`), nil, Options{Endpoint: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Functions) != 0 {
		t.Fatalf("lambda must stay off when services is explicit: %d", len(inv.Functions))
	}
}

func TestEventSourceAndPolicyTriggers(t *testing.T) {
	tests := []struct {
		arn    string
		want   string
		stream bool
	}{
		{"arn:aws:sqs:eu-central-1:1:orders.fifo", "sqs:orders.fifo", false},
		{"arn:aws:kinesis:eu-central-1:1:stream/clicks", "kinesis:clicks", true},
		{"arn:aws:dynamodb:eu-central-1:1:table/carts/stream/2026-01-01T00:00:00.000", "dynamodb:carts", true},
		{"arn:aws:kafka:eu-central-1:1:cluster/events/abc-1", "kafka:events", true},
		{"arn:aws:mq:eu-central-1:1:broker:rabbit:b-123", "mq:rabbit", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got, s := EventSourceTrigger(tt.arn, false); got != tt.want || s != tt.stream {
			t.Errorf("%s → %q %v", tt.arn, got, s)
		}
	}
	if got, _ := EventSourceTrigger("", true); got != "kafka" {
		t.Errorf("self-managed kafka → %q", got)
	}
	pol := `{"Statement":[
	 {"Effect":"Allow","Principal":{"Service":["s3.amazonaws.com"]},"Condition":{"ArnLike":{"AWS:SourceArn":"arn:aws:s3:::uploads"}}},
	 {"Effect":"Allow","Principal":{"Service":"sns.amazonaws.com"},"Condition":{"ArnLike":{"AWS:SourceArn":"arn:aws:sns:eu-central-1:1:alerts"}}},
	 {"Effect":"Allow","Principal":{"Service":"events.amazonaws.com"},"Condition":{"ArnLike":{"AWS:SourceArn":"arn:aws:events:eu-central-1:1:rule/order-created"}}},
	 {"Effect":"Deny","Principal":{"Service":"apigateway.amazonaws.com"}}]}`
	if got := fmt.Sprint(PolicyTriggers(pol)); got != "[s3:uploads sns:alerts eventbridge:order-created]" {
		t.Errorf("policy triggers %s", got)
	}
	if PolicyTriggers("not json") != nil {
		t.Error("bad policy")
	}
}
