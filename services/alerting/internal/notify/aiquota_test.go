package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

func aiPayload(th, used, limit float64) Payload {
	return Payload{OrgID: 3, ProjectID: 9, Kind: KindAIQuota, Period: "2026-10", Threshold: &th, Used: &used, Limit: &limit,
		ResetsAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli()}
}

func TestAIQuotaText(t *testing.T) {
	tests := []struct {
		p    Payload
		want string
	}{
		{aiPayload(80, 4012.4, 5000), "AI usage at 80% of this month's credits (4,012 / 5,000). Resets Nov 1."},
		{aiPayload(100, 101, 100), "AI usage at 100% of this month's credits (101 / 100). Resets Nov 1. AI enrichment falls back to static rules until then."},
	}
	for _, tt := range tests {
		if got := AIQuotaText(tt.p); got != tt.want {
			t.Errorf("got %q\nwant %q", got, tt.want)
		}
	}
}

func TestDirectTarget(t *testing.T) {
	tests := []struct {
		in   string
		ok   bool
		want int
	}{
		{"email:ops@example.com", true, 1},
		{"email:a@example.com, b@example.com", true, 2},
		{"email:not-an-address", false, 0},
		{"email:", false, 0},
		{"slack:#x", false, 0},
	}
	for _, tt := range tests {
		c, ok := DirectTarget(tt.in)
		if ok != tt.ok || (ok && (c.Type != "email" || len(recipients(c.Config)) != tt.want)) {
			t.Errorf("%q: %v %+v", tt.in, ok, c)
		}
	}
}

func TestDeliverAIQuota(t *testing.T) {
	type got struct {
		header http.Header
		body   map[string]any
	}
	var last got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last = got{r.Header.Clone(), nil}
		_ = json.Unmarshal(b, &last.body)
		w.WriteHeader(202)
	}))
	defer srv.Close()
	p := aiPayload(80, 4012, 5000)
	text := AIQuotaText(p)
	tests := []struct {
		name  string
		ch    Channel
		check func(t *testing.T, g got)
	}{
		{"slack webhook", Channel{Type: "slack", Config: map[string]any{"webhookUrl": srv.URL}}, func(t *testing.T, g got) {
			if g.body["text"] != text || g.body["blocks"] != nil {
				t.Fatalf("%v", g.body)
			}
		}},
		{"webhook", Channel{Type: "webhook", Config: map[string]any{"url": srv.URL, "secret": "s"}}, func(t *testing.T, g got) {
			if g.body["event"] != "ai_quota" || g.body["threshold"] != 80.0 || g.body["text"] != text ||
				g.header.Get("X-Trooper-Event") != "ai_quota" || !strings.HasPrefix(g.header.Get("X-Trooper-Signature"), "sha256=") {
				t.Fatalf("%v %v", g.body, g.header)
			}
		}},
		{"teams", Channel{Type: "teams", Config: map[string]any{"url": srv.URL}}, func(t *testing.T, g got) {
			if g.body["type"] != "message" || !strings.Contains(mustJSON(g.body), "4,012 / 5,000") {
				t.Fatalf("%v", g.body)
			}
		}},
		{"pagerduty", Channel{Type: "pagerduty", Config: map[string]any{"routingKey": "rk", "url": srv.URL}}, func(t *testing.T, g got) {
			if g.body["dedup_key"] != "tt-ai-quota-3-2026-10-80" || g.body["event_action"] != "trigger" {
				t.Fatalf("%v", g.body)
			}
		}},
		{"opsgenie", Channel{Type: "opsgenie", Config: map[string]any{"apiKey": "k", "url": srv.URL}}, func(t *testing.T, g got) {
			if g.body["alias"] != "tt-ai-quota-3-2026-10-80" || g.header.Get("Authorization") != "GenieKey k" {
				t.Fatalf("%v", g.body)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			last = got{}
			d := NewDeliverer(SMTPConfig{})
			if err := d.Deliver(context.Background(), tt.ch, KindAIQuota, p); err != nil {
				t.Fatal(err)
			}
			tt.check(t, last)
		})
	}

	t.Run("email", func(t *testing.T) {
		d := NewDeliverer(SMTPConfig{Addr: "smtp.example.com:587", From: "tt@example.com"})
		var msg string
		var rcpt []string
		d.sendMail = func(addr string, a smtp.Auth, from string, to []string, m []byte) error {
			msg, rcpt = string(m), to
			return nil
		}
		ch, _ := DirectTarget("email:ops@example.com")
		if err := d.Deliver(context.Background(), ch, KindAIQuota, p); err != nil {
			t.Fatal(err)
		}
		if len(rcpt) != 1 || !strings.Contains(msg, "Subject: AI usage at 80% of this month's credits") || !strings.Contains(msg, text) {
			t.Fatalf("%v %s", rcpt, msg)
		}
	})

	t.Run("slack app: plain post, no thread, no buttons", func(t *testing.T) {
		fs := &fakeSlack{script: map[string][]fakeResp{}}
		api := httptest.NewServer(fs)
		defer api.Close()
		store := &memSlackStore{token: "xoxb-1", refs: map[string][2]string{}}
		d := NewDeliverer(SMTPConfig{})
		d.Slack = &SlackApp{Base: api.URL + "/api", Store: store}
		id, name, _ := ParseSlackTarget("slack:C0123ABCD#ai-usage")
		ch := Channel{Name: "slack:C0123ABCD#ai-usage", Type: "slack_app", Config: map[string]any{"channelId": id, "channelName": name}}
		if err := d.Deliver(context.Background(), ch, KindAIQuota, p); err != nil {
			t.Fatal(err)
		}
		if len(fs.calls) != 1 || fs.calls[0].method != "chat.postMessage" {
			t.Fatalf("calls %+v", fs.calls)
		}
		b := fs.calls[0].body
		if b["channel"] != "C0123ABCD" || b["text"] != text || b["thread_ts"] != nil || b["blocks"] != nil || b["attachments"] != nil {
			t.Fatalf("body %v", b)
		}
		if len(store.refs) != 0 {
			t.Fatal("ai_quota must not store a thread ref")
		}
		store.token = ""
		if err := d.Deliver(context.Background(), ch, KindAIQuota, p); !IsPermanent(err) {
			t.Fatalf("not installed: %v", err)
		}
	})
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
