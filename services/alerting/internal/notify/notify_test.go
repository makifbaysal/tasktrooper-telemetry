package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

var firing = Payload{
	IncidentID: 42, ProjectID: 1, MonitorID: 7, Kind: "error_rate", Title: "Checkout error rate", Severity: "critical",
	State: "firing", Service: "orders-api", Endpoint: "POST /checkout", Value: f(0.0731), Op: ">", Threshold: f(0.02),
	StartedAt: 1_761_400_000_000, Summary: "Failures come from payments-api's external dependency iyzico (0.0% → 7.0% of calls failing).",
	Link: "http://localhost:5173/incidents?id=42",
}

func TestSlackMessage(t *testing.T) {
	tests := []struct {
		name     string
		p        Payload
		contains []string
		blocks   int
	}{
		{"firing", firing, []string{`:red_circle: [CRITICAL] Firing: Checkout error rate`, `*Value*\n0.0731 (> 0.02)`, `*Target*\norders-api · POST /checkout`, `*Likely cause:* Failures come from`, `"url":"http://localhost:5173/incidents?id=42"`, `<!date^1761400000^`}, 4},
		{"resolved", func() Payload { p := firing; p.State = "resolved"; p.Peak = f(0.12); return p }(), []string{`:large_green_circle: Resolved: Checkout error rate`, `peak 0.12`}, 4},
		{"warning minimal", Payload{IncidentID: 1, Title: "Lag", Severity: "warning", State: "firing"}, []string{`:large_orange_circle: [WARNING] Firing: Lag`, `*Value*\n—`}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := SlackMessage(tc.p)
			if err != nil {
				t.Fatal(err)
			}
			s := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(string(b))
			for _, c := range tc.contains {
				if !strings.Contains(s, c) {
					t.Errorf("missing %q in %s", c, s)
				}
			}
			var m struct {
				Text   string           `json:"text"`
				Blocks []map[string]any `json:"blocks"`
			}
			_ = json.Unmarshal(b, &m)
			if len(m.Blocks) != tc.blocks || m.Text == "" || m.Blocks[0]["type"] != "header" {
				t.Fatalf("blocks %d text %q", len(m.Blocks), m.Text)
			}
		})
	}
}

func TestPagerDutyEvent(t *testing.T) {
	tests := []struct {
		name   string
		p      Payload
		action string
		sev    string
	}{
		{"trigger", firing, "trigger", "critical"},
		{"unknown severity maps to warning", func() Payload { p := firing; p.Severity = "page"; return p }(), "trigger", "warning"},
		{"resolve", func() Payload { p := firing; p.State = "resolved"; return p }(), "resolve", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := PagerDutyEvent(tc.p, "RK")
			if err != nil {
				t.Fatal(err)
			}
			var ev map[string]any
			_ = json.Unmarshal(b, &ev)
			if ev["event_action"] != tc.action || ev["dedup_key"] != "tt-incident-42" || ev["routing_key"] != "RK" {
				t.Fatalf("event %v", ev)
			}
			if tc.sev == "" {
				if _, ok := ev["payload"]; ok {
					t.Fatal("resolve must not carry a payload")
				}
				return
			}
			pl := ev["payload"].(map[string]any)
			if pl["severity"] != tc.sev || pl["source"] != "orders-api" || pl["timestamp"] != "2025-10-25T13:46:40Z" {
				t.Fatalf("payload %v", pl)
			}
		})
	}
}

func TestEmailAndWebhookBodies(t *testing.T) {
	msg := string(EmailMessage(firing, "alerts@x.io", []string{"a@x.io", "b@x.io"}, time.Unix(0, 0).UTC()))
	for _, c := range []string{"From: alerts@x.io\r\n", "To: a@x.io, b@x.io\r\n", "Subject: [CRITICAL] Firing: Checkout error rate\r\n", "Likely cause: Failures", "\r\n\r\n"} {
		if !strings.Contains(msg, c) {
			t.Errorf("email missing %q", c)
		}
	}
	p := firing
	p.Title = "Ödeme hataları"
	if !strings.Contains(string(EmailMessage(p, "a@b", []string{"c@d"}, time.Now())), "Subject: =?UTF-8?B?") {
		t.Error("non-ASCII subject not encoded")
	}
	b, _ := WebhookBody(firing, "firing")
	var w struct {
		Version  int     `json:"version"`
		Event    string  `json:"event"`
		Incident Payload `json:"incident"`
	}
	if err := json.Unmarshal(b, &w); err != nil || w.Version != 1 || w.Event != "firing" || w.Incident.IncidentID != 42 {
		t.Fatalf("webhook %s", b)
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		rnd     float64
		want    time.Duration
	}{
		{1, 0.5, 30 * time.Second},
		{2, 0.5, time.Minute},
		{3, 0.5, 2 * time.Minute},
		{6, 0.5, 16 * time.Minute},
		{8, 0.5, time.Hour}, // 64m capped
		{20, 0.5, time.Hour},
		{1, 0, 24 * time.Second},        // −20 %
		{1, 0.999999, 36 * time.Second}, // ≈ +20 %
		{0, 0.5, 30 * time.Second},
	}
	for _, tc := range tests {
		got := Backoff(tc.attempt, tc.rnd)
		if d := got - tc.want; d < -time.Millisecond || d > time.Millisecond {
			t.Errorf("Backoff(%d, %v) = %v want %v", tc.attempt, tc.rnd, got, tc.want)
		}
	}
}

type captured struct {
	mu     sync.Mutex
	bodies []string
	header http.Header
	path   string
}

func TestDeliverHTTP(t *testing.T) {
	var c captured
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies, c.header, c.path = append(c.bodies, string(b)), r.Header.Clone(), r.URL.String()
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	d := NewDeliverer(SMTPConfig{})
	d.PagerDutyURL = srv.URL + "/v2/enqueue"
	t.Setenv("TT_TEST_SLACK", srv.URL+"/slack")

	tests := []struct {
		name      string
		ch        Channel
		kind      string
		status    int
		wantErr   bool
		permanent bool
		check     func(t *testing.T)
	}{
		{"slack via env secret", Channel{Name: "#oncall", Type: "slack", Config: map[string]any{"webhookUrl": "env:TT_TEST_SLACK"}}, "firing", 200, false, false, func(t *testing.T) {
			if c.path != "/slack" || !strings.Contains(c.bodies[len(c.bodies)-1], `"blocks"`) {
				t.Fatalf("slack %s %s", c.path, c.bodies)
			}
		}},
		{"pagerduty resolve", Channel{Name: "pd", Type: "pagerduty", Config: map[string]any{"routingKey": "RK"}}, "resolved", 202, false, false, func(t *testing.T) {
			if c.path != "/v2/enqueue" || !strings.Contains(c.bodies[len(c.bodies)-1], `"event_action":"resolve"`) {
				t.Fatalf("pd %s", c.bodies[len(c.bodies)-1])
			}
		}},
		{"webhook signed", Channel{Name: "hook", Type: "webhook", Config: map[string]any{"url": srv.URL + "/hook", "secret": "s3", "headers": map[string]any{"X-Team": "payments"}}}, "firing", 200, false, false, func(t *testing.T) {
			if !strings.HasPrefix(c.header.Get("X-Trooper-Signature"), "sha256=") || c.header.Get("X-Team") != "payments" || c.header.Get("X-Trooper-Event") != "firing" {
				t.Fatalf("headers %v", c.header)
			}
		}},
		{"opsgenie close", Channel{Name: "og", Type: "opsgenie", Config: map[string]any{"apiKey": "K", "url": srv.URL + "/v2/alerts"}}, "resolved", 202, false, false, func(t *testing.T) {
			if c.path != "/v2/alerts/tt-incident-42/close?identifierType=alias" || c.header.Get("Authorization") != "GenieKey K" {
				t.Fatalf("opsgenie %s %v", c.path, c.header)
			}
		}},
		{"5xx retries", Channel{Name: "hook", Type: "webhook", Config: map[string]any{"url": srv.URL}}, "firing", 503, true, false, nil},
		{"429 retries", Channel{Name: "hook", Type: "webhook", Config: map[string]any{"url": srv.URL}}, "firing", 429, true, false, nil},
		{"4xx is permanent", Channel{Name: "hook", Type: "webhook", Config: map[string]any{"url": srv.URL}}, "firing", 404, true, true, nil},
		{"missing config is permanent", Channel{Name: "#x", Type: "slack", Config: map[string]any{}}, "firing", 200, true, true, nil},
		{"unsupported type", Channel{Name: "chat", Type: "googlechat"}, "firing", 200, true, true, nil},
		{"teams card", Channel{Name: "teams", Type: "teams", Config: map[string]any{"webhookUrl": srv.URL + "/teams"}}, "reminder", 202, false, false, func(t *testing.T) {
			b := c.bodies[len(c.bodies)-1]
			if c.path != "/teams" || !strings.Contains(b, `"application/vnd.microsoft.card.adaptive"`) || !strings.Contains(b, "Still firing (reminder 1)") {
				t.Fatalf("teams %s %s", c.path, b)
			}
		}},
		{"teams without url", Channel{Name: "teams", Type: "teams", Config: map[string]any{}}, "firing", 200, true, true, nil},
		{"webhook reminder event", Channel{Name: "hook", Type: "webhook", Config: map[string]any{"url": srv.URL + "/hook"}}, "reminder", 200, false, false, func(t *testing.T) {
			if c.header.Get("X-Trooper-Event") != "reminder" || !strings.Contains(c.bodies[len(c.bodies)-1], `"event":"reminder"`) || !strings.Contains(c.bodies[len(c.bodies)-1], `"reminder":1`) {
				t.Fatalf("reminder webhook %v %s", c.header, c.bodies[len(c.bodies)-1])
			}
		}},
		{"email without smtp", Channel{Name: "mail", Type: "email", Config: map[string]any{"to": "a@b"}}, "firing", 200, true, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status = tc.status
			err := d.Deliver(context.Background(), tc.ch, tc.kind, firing)
			if (err != nil) != tc.wantErr || (err != nil && IsPermanent(err) != tc.permanent) {
				t.Fatalf("err = %v (permanent=%v)", err, IsPermanent(err))
			}
			if tc.check != nil {
				tc.check(t)
			}
		})
	}

	// network failure is retryable
	d2 := NewDeliverer(SMTPConfig{})
	err := d2.Deliver(context.Background(), Channel{Type: "webhook", Config: map[string]any{"url": "http://127.0.0.1:1/x"}}, "firing", firing)
	if err == nil || IsPermanent(err) {
		t.Fatalf("network error should retry: %v", err)
	}
}

func TestDeliverEmail(t *testing.T) {
	d := NewDeliverer(SMTPConfig{Addr: "smtp.example.com:587", User: "u", Password: "p", From: "alerts@x.io"})
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	d.sendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
		if a == nil {
			t.Error("auth expected")
		}
		return nil
	}
	ch := Channel{Name: "mail", Type: "email", Config: map[string]any{"to": []any{"a@x.io", "b@x.io"}}}
	if err := d.Deliver(context.Background(), ch, "resolved", firing); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "smtp.example.com:587" || gotFrom != "alerts@x.io" || len(gotTo) != 2 || !strings.Contains(string(gotMsg), "Subject: Resolved: Checkout error rate") {
		t.Fatalf("email %s %s %v %s", gotAddr, gotFrom, gotTo, gotMsg)
	}
}

func TestTeamsMessage(t *testing.T) {
	rem := firing
	rem.Reminder = 3
	res := firing
	res.State, res.Peak, res.ResolvedAt = "resolved", f(0.12), 1_761_400_600_000
	tests := []struct {
		name     string
		p        Payload
		color    string
		contains []string
		actions  bool
	}{
		{"firing", firing, "attention", []string{"[CRITICAL] Firing: Checkout error rate", `"title":"Value","value":"0.0731 (\u003e 0.02)"`, "**Likely cause:** Failures", `"Action.OpenUrl"`}, true},
		{"resolved", res, "good", []string{"Resolved: Checkout error rate", "peak 0.12", `"title":"Resolved"`}, true},
		{"reminder", rem, "attention", []string{"Still firing (reminder 3)", `"title":"Reminder","value":"#3`}, true},
		{"info minimal", Payload{IncidentID: 1, Title: "x", Severity: "info", State: "firing"}, "accent", []string{"[INFO] Firing: x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := TeamsMessage(tc.p)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range tc.contains {
				if !strings.Contains(string(b), c) {
					t.Errorf("missing %q in %s", c, b)
				}
			}
			var m struct {
				Type        string `json:"type"`
				Attachments []struct {
					ContentType string `json:"contentType"`
					Content     struct {
						Type    string           `json:"type"`
						Version string           `json:"version"`
						Body    []map[string]any `json:"body"`
						Actions []map[string]any `json:"actions"`
					} `json:"content"`
				} `json:"attachments"`
			}
			if err := json.Unmarshal(b, &m); err != nil || m.Type != "message" || len(m.Attachments) != 1 {
				t.Fatalf("envelope %s", b)
			}
			card := m.Attachments[0].Content
			if card.Type != "AdaptiveCard" || card.Body[0]["color"] != tc.color || (len(card.Actions) > 0) != tc.actions {
				t.Fatalf("card %+v", card)
			}
		})
	}
}

func TestReminderPayloads(t *testing.T) {
	rem := firing
	rem.Reminder = 2
	if h := rem.Headline(); h != "[CRITICAL] Still firing (reminder 2): Checkout error rate" {
		t.Fatal(h)
	}
	b, _ := SlackMessage(rem)
	if !strings.Contains(string(b), ":repeat: :red_circle: [CRITICAL] Still firing (reminder 2)") {
		t.Errorf("slack %s", b)
	}
	b, _ = PagerDutyEvent(rem, "RK")
	if !strings.Contains(string(b), `"event_action":"trigger"`) || !strings.Contains(string(b), `"dedup_key":"tt-incident-42"`) || !strings.Contains(string(b), `"reminder":2`) {
		t.Errorf("pagerduty %s", b)
	}
	b, _ = OpsgenieCreate(rem)
	if !strings.Contains(string(b), `"alias":"tt-incident-42"`) || !strings.Contains(string(b), "Still firing") {
		t.Errorf("opsgenie %s", b)
	}
	if msg := string(EmailMessage(rem, "a@b", []string{"c@d"}, time.Now())); !strings.Contains(msg, "Subject: [CRITICAL] Still firing (reminder 2): Checkout error rate") {
		t.Errorf("email %s", msg)
	}
	b, _ = WebhookBody(rem, "reminder")
	if !strings.Contains(string(b), `"event":"reminder"`) || !strings.Contains(string(b), `"reminder":2`) {
		t.Errorf("webhook %s", b)
	}
	// a resolved payload never reads as a reminder
	res := rem
	res.State = "resolved"
	if res.Headline() != "Resolved: Checkout error rate" {
		t.Error(res.Headline())
	}
}
