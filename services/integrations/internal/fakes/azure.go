package fakes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Azure is a fake Entra ID token endpoint + ARM (Microsoft.Web/sites, functions, Azure Monitor metrics) with
// recorded-shape responses. Point TT_AZURE_LOGIN_URL and TT_AZURE_ARM_URL (or azure.Options) at URL.
//
// Fixtures: subscription sub-1 (sub-2 is disabled) with Function Apps
//   - orders-func  (Consumption, Linux, Python 3.11, West Europe, tag tasktrooper.service=orders; functions
//     orders-http [http], cleanup [timer], consume [queue orders, disabled])
//   - billing-flex (Flex Consumption, node 20, 2048 MB, resource group rg-billing; function invoices [eventHub])
//
// plus a web app and a Logic App (filtered out). billing-flex rejects MemoryWorkingSet with 400 (exercises the
// per-metric fallback). Metric values: see AzureMetric.
//
// Application Insights (contracts §22): component orders-ai (rg-orders) is linked to orders-func through a
// hidden-link tag (different casing than the site id); stray-ai links to a site that does not exist; billing-flex
// has no component. POST orders-ai/query answers the per-function KQL with AIRows. App settings: orders-func has
// TASKTROOPER_SERVICE=orders-svc (plus a secret that must never be stored), billing-flex answers 403.
type Azure struct {
	*httptest.Server
	t            testing.TB
	ts           time.Time
	ClientSecret string

	mu          sync.Mutex
	TokenCalls  int
	MetricCalls int
	QueryCalls  int
	// QueryStatus makes App Insights queries fail with this HTTP status (0 = answer normally).
	QueryStatus int
	// SettingsCalls counts appsettings/list calls.
	SettingsCalls int
	// Expire makes the next tokens expire after this many seconds (default 3600).
	Expire int
}

// AzureMetric is the fixture value of (app, metric, aggregation) at the fake's minute; the minute before has half.
func AzureMetric(app, metric, agg string) (float64, bool) {
	v, ok := map[string]float64{
		"orders-func/FunctionExecutionCount/total":             600,
		"orders-func/FunctionExecutionUnits/total":             9_830_400, // MB·ms
		"orders-func/Http5xx/total":                            6,
		"orders-func/HttpResponseTime/average":                 0.125, // seconds
		"orders-func/HttpResponseTime/maximum":                 2.5,
		"orders-func/MemoryWorkingSet/average":                 268_435_456,
		"billing-flex/OnDemandFunctionExecutionCount/total":    40,
		"billing-flex/AlwaysReadyFunctionExecutionCount/total": 2,
		"billing-flex/OnDemandFunctionExecutionUnits/total":    1000,
		"billing-flex/Http5xx/total":                           0,
	}[app+"/"+metric+"/"+agg]
	return v, ok
}

// AIRows are the App Insights query rows relative to the fake's minute ts (offset in minutes):
// [offset, operation_Name, cloud_RoleName, invocations, errors, avg, p95, p99, max]. orders-http has two role
// names at ts (merged: 200 invocations, 4 errors, avg 50, p95 200, p99 260, max 900); offset +1 is the
// incomplete current minute; "GET /health" matches no function.
var AIRows = [][]any{
	{-1, "orders-http", "orders-api", 100, 2, 50.0, 120.0, 300.0, 450.0},
	{0, "orders-http", "orders-api", 180, 3, 40.0, 110.0, 250.0, 900.0},
	{0, "orders-http", "orders-func", 20, 1, 140.0, 200.0, 260.0, 600.0},
	{0, "cleanup", "orders-api", 1, 0, 1500.5, 1500.5, 1500.5, 1500.5},
	{0, "GET /health", "orders-api", 9, 0, 1.0, 1.0, 1.0, 1.0},
	{1, "orders-http", "orders-api", 5, 0, 10.0, 10.0, 10.0, 10.0},
}

// NewAzure starts the fake. Datapoints are stamped ts and ts-1m.
func NewAzure(t testing.TB, ts time.Time) *Azure {
	f := &Azure{t: t, ts: ts, ClientSecret: "s3cret-value"}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

const sitePrefix = "/subscriptions/sub-1/resourceGroups/"

func siteJSON(rg, name, kind, sku, loc, extra string) string {
	return fmt.Sprintf(`{"id":"%s%s/providers/Microsoft.Web/sites/%s","name":"%s","kind":"%s","location":"%s","type":"Microsoft.Web/sites"%s,
	 "properties":{"state":"Running","defaultHostName":"%s.azurewebsites.net","resourceGroup":"%s","sku":"%s","reserved":true,
	 "serverFarmId":"/subscriptions/sub-1/resourceGroups/%s/providers/Microsoft.Web/serverfarms/plan-%s"%s}}`,
		sitePrefix, rg, name, name, kind, loc, map[bool]string{true: `,"tags":{"tasktrooper.service":"orders","team":"checkout"}`}[name == "orders-func"],
		name, rg, sku, rg, name, extra)
}

func (f *Azure) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	if strings.HasSuffix(p, "/oauth2/v2.0/token") {
		_ = r.ParseForm()
		if r.Method != http.MethodPost || r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != "client-1" ||
			r.PostForm.Get("scope") != "https://management.azure.com/.default" || !strings.HasPrefix(p, "/tenant-1/") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"AADSTS900144: bad request"}`))
			return
		}
		if r.PostForm.Get("client_secret") != f.ClientSecret {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.\r\nTrace ID: x"}`))
			return
		}
		f.mu.Lock()
		f.TokenCalls++
		n, exp := f.TokenCalls, f.Expire
		f.mu.Unlock()
		if exp == 0 {
			exp = 3600
		}
		_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":%d,"access_token":"tok-%d"}`, exp, n)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthenticationFailed","message":"no token"}}`))
		return
	}
	if r.URL.Query().Get("api-version") == "" {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"MissingApiVersionParameter","message":"api-version"}}`))
		return
	}
	switch {
	case p == "/subscriptions":
		_, _ = w.Write([]byte(`{"value":[{"subscriptionId":"sub-1","state":"Enabled"},{"subscriptionId":"sub-2","state":"Disabled"}]}`))
	case p == "/subscriptions/sub-1/providers/Microsoft.Web/sites":
		if r.URL.Query().Get("$skiptoken") == "" {
			_, _ = fmt.Fprintf(w, `{"value":[%s,%s],"nextLink":"%s/subscriptions/sub-1/providers/Microsoft.Web/sites?api-version=2023-12-01&$skiptoken=2"}`,
				siteJSON("rg-orders", "orders-func", "functionapp,linux", "Dynamic", "West Europe", `,"siteConfig":{"linuxFxVersion":"Python|3.11"}`),
				siteJSON("rg-orders", "orders-web", "app,linux", "PremiumV3", "West Europe", ""), f.URL)
			return
		}
		_, _ = fmt.Fprintf(w, `{"value":[%s,%s]}`, f.flexSite(), siteJSON("rg-billing", "billing-logic", "functionapp,workflowapp", "WorkflowStandard", "North Europe", ""))
	case p == "/subscriptions/sub-1/resourceGroups/rg-billing/providers/Microsoft.Web/sites":
		_, _ = fmt.Fprintf(w, `{"value":[%s]}`, f.flexSite())
	case p == "/subscriptions/sub-1/providers/Microsoft.Insights/components":
		_, _ = fmt.Fprintf(w, `{"value":[
		 {"id":"%[1]srg-orders/providers/microsoft.insights/components/orders-ai","name":"orders-ai","type":"microsoft.insights/components","location":"westeurope","kind":"web",
		  "tags":{"hidden-link:/subscriptions/sub-1/resourcegroups/RG-ORDERS/providers/microsoft.web/sites/orders-func":"Resource","team":"checkout"},
		  "properties":{"AppId":"app-guid-1","InstrumentationKey":"ikey","ConnectionString":"InstrumentationKey=ikey"}},
		 {"id":"%[1]srg-old/providers/microsoft.insights/components/stray-ai","name":"stray-ai","location":"westeurope",
		  "tags":{"hidden-link:/subscriptions/sub-1/resourceGroups/rg-old/providers/Microsoft.Web/sites/gone":"Resource"}}]}`, sitePrefix)
	case p == "/subscriptions/sub-1/resourceGroups/rg-billing/providers/Microsoft.Insights/components":
		_, _ = w.Write([]byte(`{"value":[]}`))
	case p == "/subscriptions/sub-2/providers/Microsoft.Insights/components":
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"no access"}}`))
	case strings.HasSuffix(p, "/microsoft.insights/components/orders-ai/query"):
		f.query(w, r)
	case strings.HasSuffix(p, "/config/appsettings/list"):
		f.mu.Lock()
		f.SettingsCalls++
		f.mu.Unlock()
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if strings.Contains(p, "/orders-func/") {
			_, _ = w.Write([]byte(`{"name":"appsettings","properties":{"FUNCTIONS_WORKER_RUNTIME":"python","TASKTROOPER_SERVICE":"orders-svc","DB_PASSWORD":"hunter2"}}`))
			return
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"does not have authorization to perform action 'Microsoft.Web/sites/config/list/action'"}}`))
	case p == "/subscriptions/sub-2/providers/Microsoft.Web/sites":
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"no access"}}`))
	case strings.HasSuffix(p, "/functions"):
		switch {
		case strings.Contains(p, "/orders-func/"):
			_, _ = w.Write([]byte(`{"value":[
			 {"name":"orders-func/orders-http","properties":{"name":"orders-http","language":"python","config":{"bindings":[{"type":"httpTrigger","direction":"in","authLevel":"function"},{"type":"http","direction":"out"}]}}},
			 {"name":"orders-func/cleanup","properties":{"name":"cleanup","config":{"bindings":[{"type":"timerTrigger","schedule":"0 */5 * * * *"}]}}},
			 {"name":"orders-func/consume","properties":{"name":"consume","isDisabled":true,"config":{"bindings":[{"type":"queueTrigger","queueName":"orders"}]}}}]}`))
		case strings.Contains(p, "/billing-flex/"):
			_, _ = w.Write([]byte(`{"value":[{"name":"billing-flex/invoices","properties":{"name":"invoices","config":{"bindings":[{"type":"eventHubTrigger","eventHubName":"invoices"}]}}}]}`))
		default:
			w.WriteHeader(404)
		}
	case strings.HasSuffix(p, "/config/web"):
		_, _ = w.Write([]byte(`{"properties":{"linuxFxVersion":"Node|20"}}`))
	case strings.HasSuffix(p, "/providers/microsoft.insights/metrics"):
		f.metrics(w, r)
	default:
		f.t.Errorf("unexpected azure path %s", p)
		w.WriteHeader(404)
	}
}

// query answers the App Insights ARM proxy (POST {component}/query, api-version 2018-04-20) with AIRows.
func (f *Azure) query(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.QueryCalls++
	status := f.QueryStatus
	f.mu.Unlock()
	var body struct {
		Query    string `json:"query"`
		Timespan string `json:"timespan"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.Method != http.MethodPost || r.URL.Query().Get("api-version") != "2018-04-20" || !strings.HasPrefix(body.Query, "requests") ||
		!strings.Contains(body.Query, "operation_Name, cloud_RoleName") || body.Timespan != "PT15M" {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"BadArgumentError","message":"bad query"}}`))
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"InsufficientAccessError","message":"The provided credentials have insufficient access"}}`))
		return
	}
	rows := make([][]any, 0, len(AIRows))
	for _, x := range AIRows {
		ts := f.ts.Add(time.Duration(x[0].(int)) * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
		rows = append(rows, append([]any{ts}, x[1:]...))
	}
	col := func(n, t string) map[string]string { return map[string]string{"name": n, "type": t} }
	_ = json.NewEncoder(w).Encode(map[string]any{"tables": []any{map[string]any{
		"name": "PrimaryResult",
		"columns": []any{col("timestamp", "datetime"), col("operation_Name", "string"), col("cloud_RoleName", "string"),
			col("invocations", "long"), col("errors", "long"), col("avg", "real"), col("p95", "real"), col("p99", "real"), col("max", "real")},
		"rows": rows,
	}}})
}

func (f *Azure) flexSite() string {
	return siteJSON("rg-billing", "billing-flex", "functionapp,linux", "FlexConsumption", "North Europe",
		`,"functionAppConfig":{"runtime":{"name":"node","version":"20"},"scaleAndConcurrency":{"instanceMemoryMB":2048}}`)
}

func (f *Azure) metrics(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.MetricCalls++
	f.mu.Unlock()
	q := r.URL.Query()
	parts := strings.Split(r.URL.Path, "/")
	app := ""
	for i, s := range parts {
		if s == "sites" && i+1 < len(parts) {
			app = parts[i+1]
		}
	}
	if q.Get("interval") != "PT1M" || !strings.Contains(q.Get("timespan"), "/") {
		w.WriteHeader(400)
		return
	}
	names := strings.Split(q.Get("metricnames"), ",")
	aggs := strings.Split(strings.ToLower(q.Get("aggregation")), ",")
	if app == "billing-flex" {
		for _, n := range names {
			if n == "MemoryWorkingSet" || n == "FunctionExecutionCount" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"code":"BadRequest","message":"Failed to find metric configuration for provider: Microsoft.Web, resource Type: sites, metric: ` + n + `"}`))
				return
			}
		}
	}
	type out struct {
		Name       map[string]string `json:"name"`
		Unit       string            `json:"unit"`
		Timeseries []map[string]any  `json:"timeseries"`
	}
	var value []out
	for _, n := range names {
		var data []map[string]any
		for k, ts := range []time.Time{f.ts.Add(-time.Minute), f.ts} {
			d := map[string]any{"timeStamp": ts.UTC().Format(time.RFC3339)}
			for _, a := range aggs {
				if v, ok := AzureMetric(app, n, a); ok {
					if k == 0 {
						v /= 2
					}
					d[a] = v
				}
			}
			data = append(data, d)
		}
		value = append(value, out{Name: map[string]string{"value": n, "localizedValue": n}, Unit: "Count",
			Timeseries: []map[string]any{{"metadatavalues": []any{}, "data": data}}})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"cost": 0, "timespan": q.Get("timespan"), "interval": "PT1M", "value": value})
}
