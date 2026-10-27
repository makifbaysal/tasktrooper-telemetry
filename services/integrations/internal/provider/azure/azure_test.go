package azure

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/fakes"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

func newTest(t *testing.T, f *fakes.Azure, cfg, secret string) *Provider {
	t.Helper()
	p, err := New([]byte(cfg), []byte(secret), Options{LoginURL: f.URL, ARMURL: f.URL, BaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoverAndCollectAgainstFakeAzure(t *testing.T) {
	ts := time.Date(2026, 10, 25, 12, 4, 0, 0, time.UTC)
	f := fakes.NewAzure(t, ts)
	defer f.Close()
	p := newTest(t, f, `{"tenantId":"tenant-1","clientId":"client-1"}`, `{"clientSecret":"s3cret-value"}`)
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, fn := range inv.Functions {
		mem := 0
		if fn.MemoryMB != nil {
			mem = *fn.MemoryMB
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%v|%s|%v", fn.Slug, fn.Name, fn.App, fn.Service, fn.Region, fn.Runtime, mem,
			fn.Triggers, fn.URL, fn.Meta["plan"]))
	}
	want := []string{
		"azure-orders-func|orders-func|orders-func|orders|westeurope|python 3.11|0|[http schedule]|https://orders-func.azurewebsites.net|Consumption",
		"azure-billing-flex|billing-flex|billing-flex|billing-flex|northeurope|node 20|2048|[eventhub:invoices]|https://billing-flex.azurewebsites.net|Flex",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("functions\n got %v\nwant %v", got, want)
	}
	fns := fmt.Sprint(inv.Functions[0].Meta["functions"])
	if fns != "[map[name:cleanup trigger:schedule] map[disabled:true name:consume trigger:queue:orders] map[name:orders-http trigger:http]]" {
		t.Errorf("meta.functions %s", fns)
	}
	if inv.Functions[0].Meta["resourceGroup"] != "rg-orders" || inv.Functions[0].Meta["subscriptionId"] != "sub-1" {
		t.Errorf("meta %v", inv.Functions[0].Meta)
	}

	res, err := p.Collect(context.Background(), inv, ts.Add(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]float64{}
	for _, s := range res.Samples {
		if s.Attrs["faas.name"] == "" || s.Region == "" {
			t.Errorf("sample %+v", s)
		}
		vals[fmt.Sprintf("%s|%s|%s", s.Attrs["trooper.faas.id"], s.Name, s.TS.Format("15:04"))] = s.Value
	}
	exp := map[string]float64{
		"azure-orders-func|trooper.faas.invocations|12:04":      600,
		"azure-orders-func|trooper.faas.invocations|12:03":      300,
		"azure-orders-func|trooper.faas.execution_units|12:04":  9_830_400,
		"azure-orders-func|trooper.faas.errors|12:04":           6,
		"azure-orders-func|trooper.faas.duration.avg|12:04":     125,
		"azure-orders-func|trooper.faas.duration.max|12:04":     2500,
		"azure-orders-func|trooper.faas.memory_bytes|12:04":     268_435_456,
		"azure-billing-flex|trooper.faas.invocations|12:04":     42, // on-demand + always-ready
		"azure-billing-flex|trooper.faas.execution_units|12:04": 1000,
		"azure-billing-flex|trooper.faas.errors|12:04":          0,
	}
	for k, v := range exp {
		if g, ok := vals[k]; !ok || g != v {
			t.Errorf("%s = %v (ok=%v), want %v", k, g, ok, v)
		}
	}
	for k := range vals {
		if strings.Contains(k, "throttles") || strings.Contains(k, "concurrency") || strings.HasPrefix(k, "azure-billing-flex|trooper.faas.memory_bytes") {
			t.Errorf("unexpected %s", k)
		}
	}
	// Second collect: MemoryWorkingSet is remembered as unsupported for billing-flex → one call per app.
	before := f.MetricCalls
	if _, err := p.Collect(context.Background(), inv, ts.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if f.MetricCalls-before != 2 {
		t.Errorf("metric calls %d", f.MetricCalls-before)
	}
	if f.TokenCalls != 1 {
		t.Errorf("token must be cached: %d token calls", f.TokenCalls)
	}
}

func TestTokenRefreshAndBadSecret(t *testing.T) {
	f := fakes.NewAzure(t, time.Now())
	defer f.Close()
	f.Expire = 1 // expires immediately (refresh margin = half the lifetime)
	p := newTest(t, f, `{"tenantId":"tenant-1","clientId":"client-1","subscriptionIds":["sub-1"],"resourceGroups":["rg-billing"]}`, "s3cret-value\n")
	inv, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Functions) != 1 || inv.Functions[0].Name != "billing-flex" {
		t.Fatalf("resource group filter: %+v", inv.Functions)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := p.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.TokenCalls < 2 {
		t.Errorf("token not refreshed: %d", f.TokenCalls)
	}

	bad := newTest(t, f, `{"tenantId":"tenant-1","clientId":"client-1"}`, "wrong-secret")
	_, err = bad.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "wrong-secret") || strings.Contains(err.Error(), "Trace ID") {
		t.Fatalf("bad secret error %v", err)
	}
}

func TestPartialSubscriptionError(t *testing.T) {
	f := fakes.NewAzure(t, time.Now())
	defer f.Close()
	p := newTest(t, f, `{"tenantId":"tenant-1","clientId":"client-1","subscriptionIds":["sub-1","sub-2"]}`, "s3cret-value")
	inv, err := p.Discover(context.Background())
	if inv == nil || len(inv.Functions) != 2 || err == nil || !strings.Contains(err.Error(), "sub-2") {
		t.Fatalf("inv %v err %v", inv, err)
	}
}

func TestNewValidation(t *testing.T) {
	for _, tt := range []struct{ cfg, secret string }{
		{`{"clientId":"c"}`, "s"}, {`{"tenantId":"t","clientId":"c"}`, ""}, {`{"tenantId":"t","clientId":"c"}`, `{"x":1}`}, {`[`, "s"},
	} {
		if _, err := New([]byte(tt.cfg), []byte(tt.secret), Options{}); err == nil {
			t.Errorf("%s / %q: want error", tt.cfg, tt.secret)
		}
	}
}

func TestTriggerAndRuntime(t *testing.T) {
	b := func(kv ...string) []map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return []map[string]any{{"type": "http", "direction": "out"}, m}
	}
	tests := map[string][]map[string]any{
		"http":                b("type", "httpTrigger"),
		"schedule":            b("type", "timerTrigger"),
		"servicebus:payments": b("type", "serviceBusTrigger", "topicName", "payments"),
		"servicebus":          b("type", "serviceBusTrigger", "queueName", "%QUEUE%"),
		"cosmosdb:carts":      b("type", "cosmosDBTrigger", "containerName", "carts"),
		"blob":                b("type", "blobTrigger", "path", "uploads/{name}"),
		"signalr":             b("type", "signalRTrigger"),
		"":                    b("type", "queue"),
	}
	for want, in := range tests {
		if got := Trigger(in); got != want {
			t.Errorf("%v → %q want %q", in, got, want)
		}
	}
	for in, want := range map[webProps]string{
		{LinuxFxVersion: "DOTNET-ISOLATED|8.0"}: "dotnet-isolated 8.0",
		{LinuxFxVersion: "DOCKER|img"}:          "container",
		{NodeVersion: "~20"}:                    "node 20",
		{NetFrameworkVersion: "v8.0"}:           "dotnet 8.0",
		{}:                                      "",
	} {
		if got := Runtime(webConfig{Props: in}); got != want {
			t.Errorf("%+v → %q want %q", in, got, want)
		}
	}
	if Plan("ElasticPremium") != "Premium" || Plan("Standard") != "Dedicated" || Region("West Europe") != "westeurope" {
		t.Error("plan/region")
	}
	_ = model.FaaSAttrs
}
