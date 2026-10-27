package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

// AI quota notifications (ADR-018, contracts §19): kind ai_quota, no incident. The alert-engine enqueues one per
// org + month + threshold to every orgs.ai_notify target; every channel type gets a plain one-line message
// ("AI usage at 80% of this month's credits (4,012 / 5,000). Resets Nov 1."). Slack App targets get a plain
// chat.postMessage (no thread, no buttons).

// KindAIQuota is notifications.kind of AI quota threshold messages.
const KindAIQuota = "ai_quota"

// DirectTarget turns `email:<address>[,<address>…]` into an e-mail channel without a notification_channels row.
func DirectTarget(name string) (Channel, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(name), "email:")
	if !ok {
		return Channel{}, false
	}
	var to []any
	for _, a := range strings.Split(rest, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, err := mail.ParseAddress(a); err != nil {
			return Channel{}, false
		}
		to = append(to, a)
	}
	if len(to) == 0 {
		return Channel{}, false
	}
	return Channel{Name: name, Type: "email", Config: map[string]any{"to": to}}, true
}

func (p Payload) threshold() int {
	if p.Threshold == nil {
		return 0
	}
	return int(math.Round(*p.Threshold))
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// AIQuotaText is the message of every channel.
func AIQuotaText(p Payload) string {
	s := fmt.Sprintf("AI usage at %d%% of this month's credits (%s / %s).", p.threshold(),
		aiquota.FormatCredits(deref(p.Used)), aiquota.FormatCredits(deref(p.Limit)))
	if p.ResetsAt > 0 {
		s += " Resets " + time.UnixMilli(p.ResetsAt).UTC().Format("Jan 2") + "."
	}
	if p.threshold() >= 100 {
		s += " AI enrichment falls back to static rules until then."
	}
	return s
}

// AIQuotaTitle is the subject / headline.
func AIQuotaTitle(p Payload) string {
	if p.threshold() >= 100 {
		return "AI credits used up for this month"
	}
	return fmt.Sprintf("AI usage at %d%% of this month's credits", p.threshold())
}

// AIQuotaDedupKey identifies one threshold message (PagerDuty dedup key / Opsgenie alias).
func AIQuotaDedupKey(p Payload) string {
	return fmt.Sprintf("tt-ai-quota-%d-%s-%d", p.OrgID, p.Period, p.threshold())
}

// AIQuotaWebhookBody is the generic webhook body.
func AIQuotaWebhookBody(p Payload) ([]byte, error) {
	return json.Marshal(map[string]any{
		"event": KindAIQuota, "orgId": p.OrgID, "projectId": p.ProjectID, "period": p.Period, "threshold": p.threshold(),
		"used": deref(p.Used), "limit": deref(p.Limit), "resetsAt": p.ResetsAt, "title": AIQuotaTitle(p), "text": AIQuotaText(p),
	})
}

// AIQuotaEmail builds the plain-text e-mail.
func AIQuotaEmail(p Payload, from string, to []string, now time.Time) []byte {
	var b strings.Builder
	subject := AIQuotaTitle(p)
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n",
		from, strings.Join(to, ", "), mimeHeader(subject), now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "%s\r\n", AIQuotaText(p))
	if p.Link != "" {
		fmt.Fprintf(&b, "\r\n%s\r\n", p.Link)
	}
	return []byte(b.String())
}

func (d *Deliverer) deliverAIQuota(ctx context.Context, ch Channel, p Payload) error {
	text := AIQuotaText(p)
	switch ch.Type {
	case "slack_app":
		id, _ := ch.Config["channelId"].(string)
		name, _ := ch.Config["channelName"].(string)
		return d.Slack.PostText(ctx, ch.Name, id, name, p.ProjectID, text)
	case "slack":
		url := secret(ch.Config, "webhookUrl", "webhook_url", "url")
		if url == "" {
			return permanent("slack channel %q has no webhookUrl", ch.Name)
		}
		body, _ := json.Marshal(map[string]any{"text": text})
		return d.post(ctx, url, body, nil)
	case "teams":
		url := secret(ch.Config, "webhookUrl", "webhook_url", "url")
		if url == "" {
			return permanent("teams channel %q has no webhookUrl", ch.Name)
		}
		card := map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4",
			"body": []any{map[string]any{"type": "TextBlock", "text": text, "wrap": true}}}
		body, _ := json.Marshal(map[string]any{"type": "message", "summary": AIQuotaTitle(p), "attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil, "content": card}}})
		return d.post(ctx, url, body, nil)
	case "webhook":
		url := secret(ch.Config, "url")
		if url == "" {
			return permanent("webhook channel %q has no url", ch.Name)
		}
		body, err := AIQuotaWebhookBody(p)
		if err != nil {
			return permanent("webhook payload: %v", err)
		}
		h := map[string]string{"X-Trooper-Event": KindAIQuota}
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
	case "pagerduty":
		key := secret(ch.Config, "routingKey", "routing_key", "integrationKey")
		if key == "" {
			return permanent("pagerduty channel %q has no routingKey", ch.Name)
		}
		body, _ := json.Marshal(map[string]any{"routing_key": key, "event_action": "trigger", "dedup_key": AIQuotaDedupKey(p),
			"payload": map[string]any{"summary": truncate(text, 1024), "source": "tasktrooper-telemetry", "severity": "warning",
				"custom_details": map[string]any{"orgId": p.OrgID, "period": p.Period, "threshold": p.threshold(), "used": deref(p.Used), "limit": deref(p.Limit)}}})
		return d.post(ctx, orDefault(secret(ch.Config, "url"), d.PagerDutyURL), body, nil)
	case "opsgenie":
		key := secret(ch.Config, "apiKey", "api_key")
		if key == "" {
			return permanent("opsgenie channel %q has no apiKey", ch.Name)
		}
		body, _ := json.Marshal(map[string]any{"message": truncate(AIQuotaTitle(p), 130), "alias": AIQuotaDedupKey(p),
			"description": text, "priority": "P4", "source": "tasktrooper-telemetry"})
		return d.post(ctx, strings.TrimRight(orDefault(secret(ch.Config, "url"), d.OpsgenieURL), "/"), body,
			map[string]string{"Authorization": "GenieKey " + key})
	case "email":
		to := recipients(ch.Config)
		if len(to) == 0 {
			return permanent("email channel %q has no recipients", ch.Name)
		}
		if d.SMTP.Addr == "" {
			return permanent("email channel %q: TT_NOTIFIER_SMTP_ADDR not configured", ch.Name)
		}
		from := orDefault(secret(ch.Config, "from"), orDefault(d.SMTP.From, "alerts@tasktrooper.local"))
		return d.sendMail(d.SMTP.Addr, d.smtpAuth(), from, to, AIQuotaEmail(p, from, to, d.now()))
	}
	return permanent("channel %q: unsupported type %q", ch.Name, ch.Type)
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// recipients reads config.to (comma-separated string or list).
func recipients(cfg map[string]any) []string {
	var to []string
	switch v := cfg["to"].(type) {
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
	return to
}

// PostText posts a plain message (no thread, no buttons) to a Slack App target with the bot token of the org of
// projectID (AI quota notifications).
func (s *SlackApp) PostText(ctx context.Context, target, channelID, channelName string, projectID int64, text string) error {
	if s == nil || s.Store == nil {
		return permanent("slack app target %q: notifier has no Slack App support configured", target)
	}
	label := "#" + orDefault(channelName, channelID)
	token, err := s.Store.BotToken(ctx, projectID)
	if err != nil {
		return err
	}
	if token == "" {
		return permanent("slack app is not installed for the org of project %d (target %s)", projectID, label)
	}
	_, _, err = s.postJoining(ctx, token, channelID, label, map[string]any{"channel": channelID, "text": text, "unfurl_links": false})
	return err
}

// smtpAuth is PLAIN auth when TT_NOTIFIER_SMTP_USER is set.
func (d *Deliverer) smtpAuth() smtp.Auth {
	if d.SMTP.User == "" {
		return nil
	}
	host := d.SMTP.Addr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return smtp.PlainAuth("", d.SMTP.User, d.SMTP.Password, host)
}
