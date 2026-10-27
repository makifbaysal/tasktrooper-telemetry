package eval

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/llmprice"
)

func llmGroups() LLMAgg {
	nan := math.NaN()
	return LLMAgg{P95: 21000, TTFTP95: nan, Groups: []LLMGroup{
		// claude-sonnet-4-5: $3 in / $15 out / $0.30 cache read / $3.75 cache write per 1M
		{Service: "storefront-bff", Provider: "anthropic", Model: "claude-sonnet-4-5-20250929", Calls: 900, Errors: 9,
			In: 4e6, Out: 1e6, CacheR: 2e6, CacheW: 0, P95: 24000, TTFTP95: nan},
		{Service: "search", Provider: "openai", Model: "gpt-5", Calls: 100, Errors: 1, In: 1e6, Out: 1e5, P95: 8000, TTFTP95: 900},
		{Service: "search", Provider: "ollama", Model: "llama3", Calls: 50, Errors: 40, In: 1e5, Out: 1e5, P95: 3000, TTFTP95: nan},
	}}
}

func TestLLMValue(t *testing.T) {
	prices := llmprice.New(nil)
	// claude: uncached 2M × 3 + cache read 2M × 0.3 + out 1M × 15 = 6 + 0.6 + 15 = 21.6
	// gpt-5: 1M × 1.25 + 0.1M × 10 = 2.25
	const claude, gpt = 21.6, 2.25
	cases := []struct {
		metric   string
		window   time.Duration
		want     float64
		ok       bool
		topModel string
		topShare float64
	}{
		{"cost_usd", time.Hour, claude + gpt, true, "claude-sonnet-4-5-20250929", claude / (claude + gpt)},
		{"cost_usd_per_hour", 2 * time.Hour, (claude + gpt) / 2, true, "claude-sonnet-4-5-20250929", claude / (claude + gpt)},
		{"tokens", time.Hour, 4e6 + 1e6 + 1e6 + 1e5 + 2e5, true, "claude-sonnet-4-5-20250929", 5e6 / 6.3e6},
		{"input_tokens", time.Hour, 5.1e6, true, "claude-sonnet-4-5-20250929", 4e6 / 5.1e6},
		{"output_tokens", time.Hour, 1.2e6, true, "claude-sonnet-4-5-20250929", 1e6 / 1.2e6},
		{"calls", time.Hour, 1050, true, "claude-sonnet-4-5-20250929", 900.0 / 1050},
		{"errors", time.Hour, 50, true, "llama3", 40.0 / 50},
		{"error_rate", time.Hour, 50.0 / 1050, true, "llama3", 40.0 / 50},
		{"p95_ms", time.Hour, 21000, true, "claude-sonnet-4-5-20250929", 900.0 / 1050},
		{"ttft_p95_ms", time.Hour, 0, false, "", 0},
		{"nope", time.Hour, 0, false, "", 0},
	}
	for _, tc := range cases {
		o := LLMValue(tc.metric, llmGroups(), prices, tc.window)
		if o.OK != tc.ok || (tc.ok && math.Abs(o.Value-tc.want) > 1e-9) {
			t.Errorf("%s: value %v ok=%v, want %v ok=%v", tc.metric, o.Value, o.OK, tc.want, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		if len(o.Contributors) == 0 || o.Contributors[0].Model != tc.topModel || math.Abs(o.Contributors[0].Share-tc.topShare) > 1e-9 {
			t.Errorf("%s: top %+v, want %s %.3f", tc.metric, o.Contributors, tc.topModel, tc.topShare)
		}
		if strings.HasPrefix(tc.metric, "cost") && (len(o.UnpricedModels) != 1 || o.UnpricedModels[0] != "llama3") {
			t.Errorf("%s: unpriced %v", tc.metric, o.UnpricedModels)
		}
	}
}

func TestLLMValueNoData(t *testing.T) {
	prices := llmprice.New(nil)
	unpriced := LLMAgg{P95: math.NaN(), TTFTP95: math.NaN(), Groups: []LLMGroup{{Provider: "ollama", Model: "llama3", Calls: 10, In: 100}}}
	if o := LLMValue("cost_usd", unpriced, prices, time.Hour); o.OK {
		t.Errorf("all unpriced: want no_data, got %+v", o)
	}
	// an org override prices the model
	org := llmprice.New([]llmprice.Price{{Provider: "ollama", Model: "llama3", InputPerMTok: 1e6, OutputPerMTok: 0}})
	if o := LLMValue("cost_usd", unpriced, org, time.Hour); !o.OK || o.Value != 100 {
		t.Errorf("org price: %+v", o)
	}
	empty := LLMAgg{P95: math.NaN(), TTFTP95: math.NaN()}
	for _, m := range []string{"cost_usd", "calls", "tokens"} {
		if o := LLMValue(m, empty, prices, time.Hour); !o.OK || o.Value != 0 {
			t.Errorf("%s without calls: want 0, got %+v", m, o)
		}
	}
	for _, m := range []string{"error_rate", "p95_ms", "ttft_p95_ms"} {
		if o := LLMValue(m, empty, prices, time.Hour); o.OK {
			t.Errorf("%s without calls: want no_data", m)
		}
	}
}

func TestLLMSummary(t *testing.T) {
	o := LLMOutcome{Value: 34.2, OK: true, Contributors: []LLMContributor{{Service: "storefront-bff", Provider: "anthropic", Model: "claude-sonnet-4-5", Share: 0.81}}}
	cases := []struct {
		metric, op string
		th         float64
		o          LLMOutcome
		want       string
	}{
		{"cost_usd_per_hour", ">", 20, o, "LLM cost $34.20/h above $20/h — claude-sonnet-4-5 in storefront-bff 81%"},
		{"cost_usd", ">", 100, LLMOutcome{Value: 120.5}, "LLM cost $120.50 in 1h above $100"},
		{"tokens", ">", 5e6, LLMOutcome{Value: 6.3e6, Contributors: o.Contributors}, "LLM tokens 6.3M in 1h above 5M — claude-sonnet-4-5 in storefront-bff 81%"},
		{"error_rate", ">", 0.05, LLMOutcome{Value: 0.073, Contributors: []LLMContributor{{Service: "search", Model: "gpt-5", Share: 0.9}}}, "LLM error rate 7.3% above 5% — gpt-5 in search 90% of errors"},
		{"p95_ms", ">", 20000, LLMOutcome{Value: 24100, Contributors: []LLMContributor{{Model: "claude-opus-4-1", Value: 31200}}}, "LLM p95 24.1 s above 20 s — claude-opus-4-1 p95 31.2 s"},
		{"calls", "<", 10, LLMOutcome{Value: 3, Contributors: o.Contributors}, "LLM calls 3 in 1h below 10"},
	}
	for _, tc := range cases {
		if got := LLMSummary(tc.metric, tc.op, tc.th, time.Hour, tc.o); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.metric, got, tc.want)
		}
	}
}

func TestLLMSource(t *testing.T) {
	m := &model.Monitor{OrgID: 1, ProjectID: 2, Kind: "llm", Target: model.Target{Service: "bff", Env: "prod"},
		Query: model.Query{Metric: "calls", Filter: model.InfraFilter{Model: "gpt-5", Method: "post", Route: "/chat"}}}
	now := time.Date(2026, 10, 27, 14, 37, 20, 0, time.UTC)
	cases := []struct {
		window       time.Duration
		table        string
		placeholders int
	}{
		{15 * time.Minute, "llm_1m", 9},           // org, project, env, service, model, route, method + from, to
		{6 * time.Hour, "llm_1h+llm_1m", 7*2 + 6}, // tenant filters twice + hour range + two edge ranges
		{24 * time.Hour, "llm_1h+llm_1m", 20},
	}
	for _, tc := range cases {
		from, to := window(now, tc.window)
		src, args, table := llmSource(m, from, to, tc.window)
		if table != tc.table || strings.Count(src, "?") != len(args) || len(args) != tc.placeholders {
			t.Errorf("%v: table %s, %d placeholders, %d args (want %d)\n%s", tc.window, table, strings.Count(src, "?"), len(args), tc.placeholders, src)
		}
		if !strings.Contains(src, "method = ?") || !containsArg(args, "POST") || !containsArg(args, "bff") {
			t.Errorf("%v: filters missing: %v", tc.window, args)
		}
		if table == "llm_1h+llm_1m" {
			// hour part covers [ceil_hour(from), floor_hour(to)); edges stay on llm_1m
			hFrom, hTo := args[7].(int64), args[8].(int64)
			if hFrom%3600 != 0 || hTo%3600 != 0 || hTo != to.Truncate(time.Hour).Unix() || hFrom < from.Unix() {
				t.Errorf("%v: hour range %d..%d for %v..%v", tc.window, hFrom, hTo, from, to)
			}
		}
	}
}

func containsArg(args []any, v string) bool {
	for _, a := range args {
		if a == v {
			return true
		}
	}
	return false
}

type fakePrices struct{ err error }

func (f fakePrices) Table(context.Context, int64) (*llmprice.Table, error) { return nil, f.err }

func TestLLMUnsupportedAndPrices(t *testing.T) {
	e := &Evaluator{Prices: fakePrices{err: errors.New("pg down")}}
	r, err := e.Eval(t.Context(), &model.Monitor{Kind: "llm", Query: model.Query{Metric: "gb_seconds"}}, time.Now())
	if !errors.Is(err, ErrUnsupported) || !r.NoData {
		t.Errorf("bad metric: %v %+v", err, r)
	}
	// a failing / nil override table falls back to the defaults
	if tb := e.prices(t.Context(), 1); tb == nil {
		t.Fatal("nil table")
	} else if _, ok := tb.Lookup("openai", "gpt-5"); !ok {
		t.Error("defaults missing")
	}
	// target.service is the default filter and the incident's service
	m := &model.Monitor{Kind: "llm", Query: model.Query{Filter: model.InfraFilter{Service: "bff"}}}
	if s, _, _, _ := m.Scope(); s != "bff" {
		t.Errorf("scope service %q", s)
	}
	m.Target.Service = "other"
	if f := LLMFilter(m); f.Service != "bff" {
		t.Errorf("filter.service wins: %q", f.Service)
	}
}
