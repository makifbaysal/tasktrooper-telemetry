package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// Channel is a notification_channels row.
type Channel struct {
	ID     int64
	Name   string
	Type   string // slack | pagerduty | email | webhook | opsgenie | teams
	Config map[string]any
}

// PermanentError marks failures that retrying cannot fix (bad config, 4xx).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func permanent(format string, args ...any) error {
	return &PermanentError{Err: fmt.Errorf(format, args...)}
}

// IsPermanent reports whether err should stop retries.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// SMTPConfig is the e-mail relay (TT_NOTIFIER_SMTP_*).
type SMTPConfig struct {
	Addr, User, Password, From string
}

// Deliverer sends one notification.
type Deliverer struct {
	HTTP *http.Client
	SMTP SMTPConfig
	// PagerDutyURL / OpsgenieURL default to the public APIs; channel config "url" overrides (tests, EU region).
	PagerDutyURL string
	OpsgenieURL  string
	// Slack delivers slack:<channel id> targets through the Slack App (nil: such targets fail permanently).
	Slack    *SlackApp
	Now      func() time.Time
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// NewDeliverer returns a deliverer with a 10 s HTTP timeout.
func NewDeliverer(s SMTPConfig) *Deliverer {
	return &Deliverer{
		HTTP: &http.Client{Timeout: 10 * time.Second}, SMTP: s,
		PagerDutyURL: "https://events.pagerduty.com/v2/enqueue", OpsgenieURL: "https://api.opsgenie.com/v2/alerts",
		Now: time.Now, sendMail: smtp.SendMail,
	}
}

// secret reads config[key] (or the first present of keys); "env:NAME" values are read from the environment
// (notification_channels.config holds secrets by reference only).
func secret(cfg map[string]any, keys ...string) string {
	for _, k := range keys {
		s, _ := cfg[k].(string)
		if s == "" {
			continue
		}
		if name, ok := strings.CutPrefix(s, "env:"); ok {
			return os.Getenv(name)
		}
		return s
	}
	return ""
}

// Deliver sends p (notification kind firing|resolved|reminder|acknowledged) to ch.
func (d *Deliverer) Deliver(ctx context.Context, ch Channel, kind string, p Payload) error {
	switch kind {
	case "resolved":
		p.State, p.Reminder = "resolved", 0
	case "acknowledged":
		p.State, p.Reminder = "acknowledged", 0
	case "reminder":
		p.State = "firing"
		if p.Reminder <= 0 {
			p.Reminder = 1
		}
	default:
		p.Reminder = 0
	}
	switch ch.Type {
	case "slack_app":
		id, _ := ch.Config["channelId"].(string)
		name, _ := ch.Config["channelName"].(string)
		return d.Slack.Deliver(ctx, ch.Name, id, name, kind, p)
	case "slack":
		url := secret(ch.Config, "webhookUrl", "webhook_url", "url")
		if url == "" {
			return permanent("slack channel %q has no webhookUrl", ch.Name)
		}
		body, err := SlackMessage(p)
		if err != nil {
			return permanent("slack payload: %v", err)
		}
		return d.post(ctx, url, body, nil)
	case "pagerduty":
		key := secret(ch.Config, "routingKey", "routing_key", "integrationKey")
		if key == "" {
			return permanent("pagerduty channel %q has no routingKey", ch.Name)
		}
		body, err := PagerDutyEvent(p, key)
		if err != nil {
			return permanent("pagerduty payload: %v", err)
		}
		return d.post(ctx, orDefault(secret(ch.Config, "url"), d.PagerDutyURL), body, nil)
	case "webhook":
		url := secret(ch.Config, "url")
		if url == "" {
			return permanent("webhook channel %q has no url", ch.Name)
		}
		body, err := WebhookBody(p, kind)
		if err != nil {
			return permanent("webhook payload: %v", err)
		}
		h := map[string]string{"X-Trooper-Event": kind}
		if hs, ok := ch.Config["headers"].(map[string]any); ok {
			for k, v := range hs {
				if s, ok := v.(string); ok {
					h[k] = secret(map[string]any{"v": s}, "v")
				}
			}
		}
		if sec := secret(ch.Config, "secret"); sec != "" {
			mac := hmac.New(sha256.New, []byte(sec))
			mac.Write(body)
			h["X-Trooper-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		return d.post(ctx, url, body, h)
	case "opsgenie":
		key := secret(ch.Config, "apiKey", "api_key")
		if key == "" {
			return permanent("opsgenie channel %q has no apiKey", ch.Name)
		}
		base := strings.TrimRight(orDefault(secret(ch.Config, "url"), d.OpsgenieURL), "/")
		h := map[string]string{"Authorization": "GenieKey " + key}
		if p.resolved() {
			return d.post(ctx, base+"/"+OpsgenieAlias(p)+"/close?identifierType=alias", []byte(`{"source":"tasktrooper-telemetry"}`), h)
		}
		body, err := OpsgenieCreate(p)
		if err != nil {
			return permanent("opsgenie payload: %v", err)
		}
		return d.post(ctx, base, body, h)
	case "email":
		return d.email(ch, p)
	case "teams":
		url := secret(ch.Config, "webhookUrl", "webhook_url", "url")
		if url == "" {
			return permanent("teams channel %q has no webhookUrl", ch.Name)
		}
		body, err := TeamsMessage(p)
		if err != nil {
			return permanent("teams payload: %v", err)
		}
		return d.post(ctx, url, body, nil)
	}
	return permanent("channel %q: unsupported type %q", ch.Name, ch.Type)
}

func (d *Deliverer) post(ctx context.Context, url string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return permanent("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tasktrooper-notifier/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err // network: retry
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout:
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return permanent("http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

func (d *Deliverer) email(ch Channel, p Payload) error {
	var to []string
	switch v := ch.Config["to"].(type) {
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				to = append(to, s)
			}
		}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				to = append(to, s)
			}
		}
	}
	if len(to) == 0 {
		return permanent("email channel %q has no recipients", ch.Name)
	}
	if d.SMTP.Addr == "" {
		return permanent("email channel %q: TT_NOTIFIER_SMTP_ADDR not configured", ch.Name)
	}
	from := orDefault(secret(ch.Config, "from"), orDefault(d.SMTP.From, "alerts@tasktrooper.local"))
	var auth smtp.Auth
	if d.SMTP.User != "" {
		host := d.SMTP.Addr
		if i := strings.LastIndexByte(host, ':'); i > 0 {
			host = host[:i]
		}
		auth = smtp.PlainAuth("", d.SMTP.User, d.SMTP.Password, host)
	}
	return d.sendMail(d.SMTP.Addr, auth, from, to, EmailMessage(p, from, to, d.Now()))
}

// Backoff is the delay before retry number attempt (1 = first retry): 30 s doubling, capped at 1 h,
// ±20 % jitter from rnd ∈ [0, 1).
func Backoff(attempt int, rnd float64) time.Duration {
	const (
		base = 30 * time.Second
		cap_ = time.Hour
	)
	if attempt < 1 {
		attempt = 1
	}
	d := float64(base) * math.Pow(2, float64(attempt-1))
	if d > float64(cap_) {
		d = float64(cap_)
	}
	j := (rnd*2 - 1) * 0.2
	return time.Duration(d * (1 + j))
}

// DecodeConfig parses notification_channels.config.
func DecodeConfig(b []byte) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}
