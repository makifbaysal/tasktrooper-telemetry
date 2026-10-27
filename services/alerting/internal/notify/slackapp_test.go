package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memSlackStore struct {
	mu    sync.Mutex
	token string
	refs  map[string][2]string
}

func (m *memSlackStore) BotToken(context.Context, int64) (string, error) { return m.token, nil }
func (m *memSlackStore) MessageRef(_ context.Context, id int64, target string) (string, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refs[target+"|"+itoa(id)]
	return r[0], r[1], ok, nil
}
func (m *memSlackStore) SaveMessageRef(_ context.Context, id int64, target, ts, ch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.refs[target+"|"+itoa(id)]; !ok {
		m.refs[target+"|"+itoa(id)] = [2]string{ts, ch}
	}
	return nil
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }

type slackCall struct {
	method string
	auth   string
	body   map[string]any
}

// fakeSlack answers Web API calls; script maps method → successive responses (status, json, retry-after).
type fakeSlack struct {
	mu     sync.Mutex
	calls  []slackCall
	script map[string][]fakeResp
}

type fakeResp struct {
	status     int
	body       string
	retryAfter string
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	f.mu.Lock()
	f.calls = append(f.calls, slackCall{method, r.Header.Get("Authorization"), body})
	resp := fakeResp{200, `{"ok":true,"ts":"1700000000.000100","channel":"C0123ABCD"}`, ""}
	if q := f.script[method]; len(q) > 0 {
		resp, f.script[method] = q[0], q[1:]
	}
	f.mu.Unlock()
	if resp.retryAfter != "" {
		w.Header().Set("Retry-After", resp.retryAfter)
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write([]byte(resp.body))
}

func (f *fakeSlack) methods() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return strings.Join(out, ",")
}

func newSlackApp(t *testing.T, f *fakeSlack, token string) (*SlackApp, *memSlackStore) {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st := &memSlackStore{token: token, refs: map[string][2]string{}}
	now := time.UnixMilli(1_700_000_600_000)
	return &SlackApp{Base: srv.URL + "/api", Store: st, PublicAppURL: "https://app.example.com/", Now: func() time.Time { return now }}, st
}

func f64(v float64) *float64 { return &v }

func firingPayload() Payload {
	return Payload{IncidentID: 5, ProjectID: 9, MonitorID: 7, Kind: "error_rate", Title: "Checkout errors", Severity: "critical",
		State: "firing", Service: "orders-api", Endpoint: "POST /checkout", Value: f64(0.12), Op: ">", Threshold: f64(0.02),
		StartedAt: 1_700_000_000_000, RootCause: &Cause{Summary: "payments-api → iyzico timeouts", Confidence: f64(0.78)},
		Deploy: &Deploy{Version: "abc123", At: 1_699_999_700_000, Author: "ayse", CommitMessage: "bump client\nmore"}, CodeRef: "acme/shop:.trooper/monitors/checkout.yaml"}
}

const target = "slack:C0123ABCD#payments-alerts"

func TestSlackAppFiringThenResolved(t *testing.T) {
	f := &fakeSlack{}
	app, st := newSlackApp(t, f, "xoxb-test")
	ctx := context.Background()
	if err := app.Deliver(ctx, target, "C0123ABCD", "payments-alerts", "firing", firingPayload()); err != nil {
		t.Fatal(err)
	}
	if f.methods() != "chat.postMessage" || f.calls[0].auth != "Bearer xoxb-test" {
		t.Fatalf("calls %s auth %q", f.methods(), f.calls[0].auth)
	}
	raw, _ := json.Marshal(f.calls[0].body)
	s := string(raw)
	for _, want := range []string{`"channel":"C0123ABCD"`, "Likely cause:* payments-api → iyzico timeouts (78% confidence)", `"action_id":"tt_ack:5"`,
		`"action_id":"tt_mute1h:7"`, `"url":"https://app.example.com/incidents?id=5"`, "🔴 Firing: Checkout errors", "Deploy `abc123` 15m ago by ayse — bump client",
		"acme/shop:.trooper/monitors/checkout.yaml", `"color":"#E01E5A"`, "*Firing for*\\n10m"} {
		if !strings.Contains(s, want) {
			t.Errorf("firing message lacks %q:\n%s", want, s)
		}
	}
	if ts, ch, ok, _ := st.MessageRef(ctx, 5, target); !ok || ts != "1700000000.000100" || ch != "C0123ABCD" {
		t.Fatalf("ref %s %s %v", ts, ch, ok)
	}
	// a retried firing row does not post twice
	if err := app.Deliver(ctx, target, "C0123ABCD", "payments-alerts", "firing", firingPayload()); err != nil || f.methods() != "chat.postMessage" {
		t.Fatalf("retry: %v %s", err, f.methods())
	}

	res := firingPayload()
	res.State, res.ResolvedAt, res.Peak = "resolved", 1_700_000_900_000, f64(0.2)
	if err := app.Deliver(ctx, target, "C0123ABCD", "payments-alerts", "resolved", res); err != nil {
		t.Fatal(err)
	}
	if f.methods() != "chat.postMessage,chat.postMessage,chat.update" {
		t.Fatalf("calls %s", f.methods())
	}
	reply, upd := f.calls[1].body, f.calls[2].body
	if reply["thread_ts"] != "1700000000.000100" || reply["channel"] != "C0123ABCD" || !strings.Contains(reply["text"].(string), "Resolved after 15m — peak 0.2") {
		t.Fatalf("reply %v", reply)
	}
	ub, _ := json.Marshal(upd)
	if upd["ts"] != "1700000000.000100" || !strings.Contains(string(ub), `"color":"#2EB67D"`) || strings.Contains(string(ub), "tt_ack:") ||
		strings.Contains(string(ub), "tt_mute1h:") || !strings.Contains(string(ub), "✅ Resolved: Checkout errors") {
		t.Fatalf("update %s", ub)
	}
}

func TestSlackAppAckAndReminderThread(t *testing.T) {
	f := &fakeSlack{}
	app, st := newSlackApp(t, f, "xoxb-test")
	ctx := context.Background()
	_ = st.SaveMessageRef(ctx, 5, target, "111.222", "C0123ABCD")
	d := &Deliverer{Slack: app}
	ch := Channel{Name: target, Type: "slack_app", Config: map[string]any{"channelId": "C0123ABCD", "channelName": "payments-alerts"}}
	p := firingPayload()
	p.AckedBy = "Ayşe"
	if err := d.Deliver(ctx, ch, "acknowledged", p); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].body["text"] != "👀 Acknowledged by Ayşe" || f.calls[0].body["thread_ts"] != "111.222" {
		t.Fatalf("ack reply %v", f.calls[0].body)
	}
	ub, _ := json.Marshal(f.calls[1].body)
	if strings.Contains(string(ub), "tt_ack:") || !strings.Contains(string(ub), "tt_mute1h:7") || !strings.Contains(string(ub), "Acknowledged by Ayşe") {
		t.Fatalf("ack update %s", ub)
	}
	p.Reminder = 2
	if err := d.Deliver(ctx, ch, "reminder", p); err != nil {
		t.Fatal(err)
	}
	if txt := f.calls[2].body["text"].(string); !strings.HasPrefix(txt, "🔁 Still firing (reminder 2) — 0.12 (> 0.02)") {
		t.Fatalf("reminder reply %q", txt)
	}
}

func TestSlackAppErrors(t *testing.T) {
	notIn := fakeResp{200, `{"ok":false,"error":"not_in_channel"}`, ""}
	tests := []struct {
		name      string
		token     string
		script    map[string][]fakeResp
		wantCalls string
		wantErr   string // "" = success; "permanent:" prefix = permanent
		retry     time.Duration
	}{
		{"join public channel and retry", "x", map[string][]fakeResp{"chat.postMessage": {notIn}}, "chat.postMessage,conversations.join,chat.postMessage", "", 0},
		{"private channel cannot be joined", "x", map[string][]fakeResp{"chat.postMessage": {notIn},
			"conversations.join": {{200, `{"ok":false,"error":"method_not_supported_for_channel_type"}`, ""}}}, "chat.postMessage,conversations.join", "permanent:invite the TaskTrooper app", 0},
		{"channel not found", "x", map[string][]fakeResp{"chat.postMessage": {{200, `{"ok":false,"error":"channel_not_found"}`, ""}}}, "chat.postMessage", "permanent:channel #payments-alerts not found", 0},
		{"invalid auth", "x", map[string][]fakeResp{"chat.postMessage": {{200, `{"ok":false,"error":"invalid_auth"}`, ""}}}, "chat.postMessage", "permanent:bot token rejected (invalid_auth)", 0},
		{"rate limited", "x", map[string][]fakeResp{"chat.postMessage": {{429, `{"ok":false,"error":"ratelimited"}`, "7"}}}, "chat.postMessage", "rate limited", 7 * time.Second},
		{"server error retried", "x", map[string][]fakeResp{"chat.postMessage": {{503, ``, ""}}}, "chat.postMessage", "http 503", 0},
		{"transient slack error retried", "x", map[string][]fakeResp{"chat.postMessage": {{200, `{"ok":false,"error":"internal_error"}`, ""}}}, "chat.postMessage", "internal_error", 0},
		{"app not installed", "", nil, "", "permanent:slack app is not installed", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeSlack{script: tt.script}
			app, _ := newSlackApp(t, f, tt.token)
			err := app.Deliver(context.Background(), target, "C0123ABCD", "payments-alerts", "firing", firingPayload())
			if f.methods() != tt.wantCalls {
				t.Errorf("calls %q want %q", f.methods(), tt.wantCalls)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			want, perm := strings.CutPrefix(tt.wantErr, "permanent:")
			if err == nil || !strings.Contains(err.Error(), want) || IsPermanent(err) != perm {
				t.Fatalf("err %v (permanent=%v), want %q permanent=%v", err, IsPermanent(err), want, perm)
			}
			var ra *RetryAfterError
			if tt.retry > 0 && (!errors.As(err, &ra) || ra.After != tt.retry) {
				t.Fatalf("retry-after: %v", err)
			}
		})
	}
}

func TestCauseText(t *testing.T) {
	tests := []struct {
		p    Payload
		want string
	}{
		{Payload{}, ""},
		{Payload{Summary: "db slow"}, "db slow"},
		{Payload{RootCause: &Cause{Summary: "db slow", Confidence: f64(0.784)}}, "db slow (78% confidence)"},
		{Payload{RootCause: &Cause{Summary: "db slow", Confidence: f64(0.12)}}, "db slow (12% confidence · low confidence)"},
		{Payload{Summary: "old", RootCause: &Cause{Summary: "new", Confidence: f64(0.3)}}, "new (30% confidence)"},
	}
	for _, tt := range tests {
		if got := tt.p.CauseText(); got != tt.want {
			t.Errorf("%+v: %q want %q", tt.p, got, tt.want)
		}
	}
	// no RCA → no "Likely cause" section
	b, _ := json.Marshal(SlackAppMessage(Payload{Title: "x", State: "firing"}, "", time.Now()))
	if strings.Contains(string(b), "Likely cause") {
		t.Fatal("likely cause without RCA")
	}
}

func TestSlackTargets(t *testing.T) {
	for _, tt := range []struct {
		in       string
		id, name string
		ok       bool
		webhook  string
	}{
		{"slack:C0123ABCD#payments-alerts", "C0123ABCD", "payments-alerts", true, ""},
		{"slack:G01ABCDEF", "G01ABCDEF", "", true, ""},
		{"slack:#payments-alerts", "", "", false, ""},
		{"slack:payments", "", "", false, ""},
		{"#C0123ABCD", "", "", false, ""},
		{"slack:https://hooks.slack.com/services/T/B/x", "", "", false, "https://hooks.slack.com/services/T/B/x"},
		{"slack:https://evil.example.com/services/T/B/x", "", "", false, ""},
		{"slack:http://hooks.slack.com/services/T/B/x", "", "", false, ""},
	} {
		id, name, ok := ParseSlackTarget(tt.in)
		if id != tt.id || name != tt.name || ok != tt.ok || SlackWebhookTarget(tt.in) != tt.webhook {
			t.Errorf("%s: %q %q %v %q", tt.in, id, name, ok, SlackWebhookTarget(tt.in))
		}
	}
}
