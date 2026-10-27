package server

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// ---- AI quota fakes (identityFake: user 5 admin of org 7, viewer of org 8)

type aiCall struct {
	org, user int64
	admin     bool
	month     time.Time
}

var aiCalls []aiCall

func (f *identityFake) MeAI(_ context.Context, org, user int64, now time.Time) (*model.MeAI, error) {
	l := 5000.0
	return &model.MeAI{Period: now.Format("2006-01"), Limit: &l, Used: 12.5, UserUsed: 2}, nil
}

func (f *identityFake) AIUsage(_ context.Context, org, user int64, month time.Time, admin bool) (*model.AIUsage, error) {
	aiCalls = append(aiCalls, aiCall{org, user, admin, month})
	u := &model.AIUsage{Period: month.Format("2006-01"), ByUser: []model.AIUserUsage{}}
	if admin {
		u.ByUser = append(u.ByUser, model.AIUserUsage{UserID: "5"})
	}
	return u, nil
}

func (f *identityFake) SetOrgAIQuota(_ context.Context, org int64, in model.AIQuotaInput) (*model.AIQuota, error) {
	if in.MonthlyCredits != nil && *in.MonthlyCredits < -1 {
		return nil, store.Invalid("monthlyCredits must be null, -1 (unlimited) or >= 0")
	}
	return &model.AIQuota{OrgID: "7", MonthlyCredits: in.MonthlyCredits, Notify: in.Notify}, nil
}

func (f *identityFake) SetMemberAIQuota(_ context.Context, org, user int64, in model.MemberAIQuotaInput) (*model.MemberAIQuota, error) {
	if user == 404 {
		return nil, store.ErrNotFound
	}
	return &model.MemberAIQuota{OrgID: "7", UserID: "9", MonthlyCredits: in.MonthlyCredits}, nil
}

func TestAIQuotaRoutes(t *testing.T) {
	f := newIdentityFake()
	h := identityServer(t, f, nil).Handler()

	w := cookieReq(h, "GET", "/api/v1/me", "sess-5", "")
	var me model.Me
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if w.Code != 200 || me.AI == nil || me.AI.Used != 12.5 || me.AI.Limit == nil || *me.AI.Limit != 5000 || me.AI.UserLimit != nil {
		t.Fatalf("me.ai: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"userLimit":null`) {
		t.Errorf("userLimit must be null: %s", w.Body)
	}

	aiCalls = nil
	if w := cookieReq(h, "GET", "/api/v1/usage/ai?month=2026-09", "sess-5", ""); w.Code != 200 {
		t.Fatalf("usage: %d %s", w.Code, w.Body)
	}
	if len(aiCalls) != 1 || aiCalls[0].org != 7 || aiCalls[0].user != 5 || !aiCalls[0].admin || aiCalls[0].month.Month() != time.September {
		t.Errorf("usage call: %+v", aiCalls)
	}
	if w := cookieReq(h, "GET", "/api/v1/usage/ai?month=sept", "sess-5", ""); w.Code != 400 {
		t.Errorf("bad month: %d", w.Code)
	}
	if w := cookieReq(h, "GET", "/api/v1/usage/ai", "sess-5", "", projectHeader, "80"); w.Code != 403 {
		t.Errorf("viewer usage: %d", w.Code)
	}

	f.audit = nil
	w = cookieReq(h, "PUT", "/api/v1/orgs/7/ai-quota", "sess-5", `{"monthlyCredits":2000,"notify":["slack:C1#ops"]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"monthlyCredits":2000`) {
		t.Fatalf("org quota: %d %s", w.Code, w.Body)
	}
	if len(f.audit) != 1 || f.audit[0].Action != "ai_quota.update" || f.audit[0].OrgID != 7 || f.audit[0].Target != "7" || f.audit[0].UserID != 5 {
		t.Errorf("audit: %+v", f.audit)
	}
	if w := cookieReq(h, "PUT", "/api/v1/orgs/7/ai-quota", "sess-5", `{"monthlyCredits":-5}`); w.Code != 400 {
		t.Errorf("invalid credits: %d", w.Code)
	}
	if w := cookieReq(h, "PUT", "/api/v1/orgs/8/ai-quota", "sess-5", `{"monthlyCredits":null}`); w.Code != 403 {
		t.Errorf("viewer org quota: %d", w.Code)
	}
	if w := cookieReq(h, "PUT", "/api/v1/orgs/99/ai-quota", "sess-5", `{"monthlyCredits":null}`); w.Code != 404 {
		t.Errorf("foreign org: %d", w.Code)
	}

	f.audit = nil
	w = cookieReq(h, "PUT", "/api/v1/orgs/7/members/9/ai-quota", "sess-5", `{"monthlyCredits":300}`)
	if w.Code != 200 || len(f.audit) != 1 || f.audit[0].Action != "ai_quota.member" || f.audit[0].Target != "9" {
		t.Fatalf("member quota: %d %s %+v", w.Code, w.Body, f.audit)
	}
	if w := cookieReq(h, "PUT", "/api/v1/orgs/7/members/404/ai-quota", "sess-5", `{"monthlyCredits":null}`); w.Code != 404 {
		t.Errorf("unknown member: %d", w.Code)
	}
	if w := cookieReq(h, "PUT", "/api/v1/orgs/8/members/9/ai-quota", "sess-5", `{"monthlyCredits":1}`); w.Code != 403 {
		t.Errorf("viewer member quota: %d", w.Code)
	}
}

// ---- infra / serverless fakes

type phase6Fake struct {
	fakeStore
	gotF   store.InfraFilter
	gotSF  store.ServerlessFilter
	gotTF  store.TraceFilter
	gotLF  store.LogFilter
	gotQ   store.Q
	gotArg string
}

func (f *phase6Fake) SearchTraces(_ context.Context, _ store.Q, fl store.TraceFilter) ([]model.TraceSummary, error) {
	f.gotTF = fl
	return []model.TraceSummary{}, nil
}

func (f *phase6Fake) SearchLogs(_ context.Context, _ store.Q, fl store.LogFilter) (model.LogPage, error) {
	f.gotLF = fl
	return model.LogPage{}, nil
}

func (f *phase6Fake) ListHosts(_ context.Context, q store.Q, fl store.InfraFilter) ([]model.Host, error) {
	f.gotQ, f.gotF = q, fl
	return []model.Host{{ID: "node-a", Status: model.InfraOK, Spark: model.HostSpark{CPU: []float64{}, Memory: []float64{}}}}, nil
}

func (f *phase6Fake) GetHost(_ context.Context, q store.Q, host string) (*model.HostDetail, error) {
	f.gotArg = host
	if host != "node-a" {
		return nil, store.ErrNotFound
	}
	return &model.HostDetail{Host: model.Host{ID: host, CPU: 0.5}, Series: model.HostSeries{CPU: model.SeriesSet{Timestamps: []int64{1}, Series: []timerange.Series{{ID: "cpu", Data: []float64{0.5}}}}}}, nil
}

func (f *phase6Fake) ListContainers(_ context.Context, _ store.Q, fl store.InfraFilter) ([]model.Container, error) {
	f.gotF = fl
	return []model.Container{}, nil
}

func (f *phase6Fake) ListPods(_ context.Context, _ store.Q, fl store.InfraFilter) ([]model.Pod, error) {
	f.gotF = fl
	return []model.Pod{}, nil
}

func (f *phase6Fake) ListK8sEvents(_ context.Context, _ store.Q, fl store.InfraFilter) ([]model.K8sEvent, error) {
	f.gotF = fl
	return []model.K8sEvent{}, nil
}

func (f *phase6Fake) ServiceInfra(_ context.Context, _ store.Q, svc string) (*model.ServiceInfra, error) {
	f.gotArg = svc
	return &model.ServiceInfra{}, nil
}

func (f *phase6Fake) ListFunctions(_ context.Context, _ store.Q, fl store.ServerlessFilter) ([]model.ServerlessFunction, error) {
	f.gotSF = fl
	return []model.ServerlessFunction{{ID: "1", P95Ms: math.Inf(1)}}, nil
}

func (f *phase6Fake) GetFunction(_ context.Context, _ store.Q, id string) (*model.ServerlessFunctionDetail, error) {
	f.gotArg = id
	return &model.ServerlessFunctionDetail{ServerlessFunction: model.ServerlessFunction{ID: id, Invocations: 7},
		Series: model.FunctionSeries{Invocations: model.SeriesSet{Series: []timerange.Series{{ID: "invocations"}}}}}, nil
}

func TestPhase6Routes(t *testing.T) {
	f := &phase6Fake{}
	srv := identityServer(t, &identityFake{}, nil)
	srv.Store = f
	h := srv.Handler()

	w := do(h, "GET", "/api/v1/infra/hosts?range=15m&cluster=prod&q=node", "good", "")
	if w.Code != 200 || f.gotF.Cluster != "prod" || f.gotF.Q != "node" || f.gotQ.W.Points != 15 {
		t.Fatalf("hosts: %d %+v %+v", w.Code, f.gotF, f.gotQ.W)
	}
	if w := do(h, "GET", "/api/v1/infra/hosts?range=2d", "good", ""); w.Code != 400 {
		t.Errorf("bad range: %d", w.Code)
	}
	w = do(h, "GET", "/api/v1/infra/hosts/node-a?range=30m", "good", "")
	var hd map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &hd)
	if w.Code != 200 || string(hd["cpu"]) != "0.5" || !strings.HasPrefix(string(hd["series"]), `{"cpu":{"timestamps"`) || string(hd["id"]) != `"node-a"` {
		t.Fatalf("host detail (row fields + series): %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/api/v1/infra/hosts/nope", "good", ""); w.Code != 404 {
		t.Errorf("unknown host: %d", w.Code)
	}
	if do(h, "GET", "/api/v1/infra/containers?host=node-a&service=checkout", "good", ""); f.gotF.Host != "node-a" || f.gotF.Service != "checkout" {
		t.Errorf("containers filter: %+v", f.gotF)
	}
	if do(h, "GET", "/api/v1/infra/k8s/pods?cluster=c&namespace=n&workload=w&node=x&service=s", "good", ""); f.gotF != (store.InfraFilter{Cluster: "c", Namespace: "n", Workload: "w", Node: "x", Service: "s"}) {
		t.Errorf("pods filter: %+v", f.gotF)
	}
	if do(h, "GET", "/api/v1/infra/k8s/events?cluster=c&kind=Pod&name=p&limit=5", "good", ""); f.gotF != (store.InfraFilter{Cluster: "c", Kind: "Pod", Name: "p", Limit: 5}) {
		t.Errorf("events filter: %+v", f.gotF)
	}
	if w := do(h, "GET", "/api/v1/services/checkout/infra", "good", ""); w.Code != 200 || f.gotArg != "checkout" {
		t.Errorf("service infra: %d %s", w.Code, f.gotArg)
	}
	if w := do(h, "GET", "/api/v1/serverless/functions?provider=gcp", "good", ""); w.Code != 400 {
		t.Errorf("bad provider: %d", w.Code)
	}
	// a non-finite value never produces a broken 200 (the store maps them to 0; writeJSON turns them into a 500)
	if w := do(h, "GET", "/api/v1/serverless/functions?provider=aws&q=ord", "good", ""); w.Code != 500 || f.gotSF.Provider != "aws" || f.gotSF.Q != "ord" {
		t.Errorf("functions: %d %+v", w.Code, f.gotSF)
	}
	if do(h, "GET", "/api/v1/serverless/functions?parent=12", "good", ""); f.gotSF.Parent != "12" {
		t.Errorf("functions parent: %+v", f.gotSF)
	}
	// pod traces / logs (contracts §22)
	if w := do(h, "GET", "/api/v1/traces?service=api&pod=api-7f&namespace=shop&node=n1&host=h1&errors=true", "good", ""); w.Code != 200 ||
		f.gotTF.Pod != "api-7f" || f.gotTF.Namespace != "shop" || f.gotTF.Node != "n1" || f.gotTF.Host != "h1" || !f.gotTF.ErrorsOnly || f.gotTF.Service != "api" {
		t.Errorf("trace filter: %d %+v", w.Code, f.gotTF)
	}
	if w := do(h, "GET", "/api/v1/logs?pod=api-7f&namespace=shop&q=boom", "good", ""); w.Code != 200 || f.gotLF.Pod != "api-7f" || f.gotLF.Namespace != "shop" || f.gotLF.Text != "boom" {
		t.Errorf("log filter: %d %+v", w.Code, f.gotLF)
	}
	w = do(h, "GET", "/api/v1/serverless/functions/aws-eu-west-1-orders", "good", "")
	var fd map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &fd)
	if w.Code != 200 || f.gotArg != "aws-eu-west-1-orders" || string(fd["invocations"]) != "7" || !strings.HasPrefix(string(fd["series"]), `{"invocations":{`) || string(fd["integration"]) != "null" {
		t.Errorf("function detail: %d %s", w.Code, w.Body)
	}
}
