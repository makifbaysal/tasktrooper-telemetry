// Package notify delivers the notifications outbox: payload builders per channel type, delivery,
// retry schedule and the polling worker.
package notify

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Payload is notifications.payload as written by the alert engine.
type Payload struct {
	IncidentID int64    `json:"incidentId"`
	ProjectID  int64    `json:"projectId"`
	MonitorID  int64    `json:"monitorId,omitempty"`
	KpiID      int64    `json:"kpiId,omitempty"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Severity   string   `json:"severity"`
	State      string   `json:"state"`              // firing | resolved
	Reminder   int      `json:"reminder,omitempty"` // reminder number (notification kind reminder)
	Service    string   `json:"service,omitempty"`
	Endpoint   string   `json:"endpoint,omitempty"`
	Value      *float64 `json:"value,omitempty"`
	Op         string   `json:"op,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	Peak       *float64 `json:"peak,omitempty"`
	StartedAt  int64    `json:"startedAt,omitempty"`  // unix ms
	ResolvedAt int64    `json:"resolvedAt,omitempty"` // unix ms
	Summary    string   `json:"summary,omitempty"`    // root cause sentence
	Link       string   `json:"link,omitempty"`
	// RootCause is the RCA result (incidents.root_cause): summary + confidence 0..1.
	RootCause *Cause `json:"rootCause,omitempty"`
	// Source / CodeRef: alert-as-code monitors ("repo:path").
	Source  string `json:"source,omitempty"`
	CodeRef string `json:"codeRef,omitempty"`
	// Deploy is the newest deploy of the service within the hour before firing.
	Deploy  *Deploy `json:"deploy,omitempty"`
	AckedBy string  `json:"ackedBy,omitempty"`
	AckedAt int64   `json:"ackedAt,omitempty"` // unix ms
	// AI quota notifications (kind ai_quota, no incident; contracts §19): Threshold is the percent (80 | 100).
	OrgID    int64    `json:"orgId,omitempty"`
	Period   string   `json:"period,omitempty"` // YYYY-MM
	Used     *float64 `json:"used,omitempty"`
	Limit    *float64 `json:"limit,omitempty"`
	ResetsAt int64    `json:"resetsAt,omitempty"` // unix ms
}

// Cause is payload.rootCause.
type Cause struct {
	Summary    string   `json:"summary"`
	Confidence *float64 `json:"confidence,omitempty"`
}

// Deploy is payload.deploy.
type Deploy struct {
	Version       string `json:"version"`
	Source        string `json:"source,omitempty"`
	Env           string `json:"env,omitempty"`
	At            int64  `json:"at,omitempty"` // unix ms
	URL           string `json:"url,omitempty"`
	CommitMessage string `json:"commitMessage,omitempty"`
	Author        string `json:"author,omitempty"`
}

// CauseText is "summary (78% confidence)", "summary (12% confidence · low confidence)" below 30 %, the bare
// summary without a confidence, or "" without RCA.
func (p Payload) CauseText() string {
	s := p.Summary
	var conf *float64
	if p.RootCause != nil {
		if p.RootCause.Summary != "" {
			s = p.RootCause.Summary
		}
		conf = p.RootCause.Confidence
	}
	if s == "" {
		return ""
	}
	if conf == nil {
		return s
	}
	pct := int(math.Round(*conf * 100))
	if *conf < LowConfidence {
		return fmt.Sprintf("%s (%d%% confidence · low confidence)", s, pct)
	}
	return fmt.Sprintf("%s (%d%% confidence)", s, pct)
}

// LowConfidence: root causes below this confidence are labelled "low confidence" (contracts §17).
const LowConfidence = 0.3

func fnum(v float64) string {
	if math.Abs(v) >= 1000 || v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.4g", v)
}

// ValueText is "1.23 (> 0.02)" style.
func (p Payload) ValueText() string {
	if p.Value == nil {
		return "—"
	}
	s := fnum(*p.Value)
	if p.Threshold != nil && p.State != "resolved" {
		op := p.Op
		if op == "" {
			op = ">"
		}
		s += fmt.Sprintf(" (%s %s)", op, fnum(*p.Threshold))
	}
	if p.Peak != nil {
		s += fmt.Sprintf(", peak %s", fnum(*p.Peak))
	}
	return s
}

// Target is "service · endpoint" or whichever is set.
func (p Payload) Target() string {
	switch {
	case p.Service != "" && p.Endpoint != "":
		return p.Service + " · " + p.Endpoint
	case p.Service != "":
		return p.Service
	}
	return p.Endpoint
}

func (p Payload) resolved() bool { return p.State == "resolved" }

func (p Payload) reminder() bool { return p.Reminder > 0 && !p.resolved() }

// Headline is the one-line summary used by every channel.
func (p Payload) Headline() string {
	if p.resolved() {
		return "Resolved: " + p.Title
	}
	if p.reminder() {
		return fmt.Sprintf("[%s] Still firing (reminder %d): %s", strings.ToUpper(orDefault(p.Severity, "warning")), p.Reminder, p.Title)
	}
	return fmt.Sprintf("[%s] Firing: %s", strings.ToUpper(orDefault(p.Severity, "warning")), p.Title)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ─────────────── Slack (incoming webhook with Block Kit) ───────────────

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// SlackMessage builds an incoming-webhook body.
func SlackMessage(p Payload) ([]byte, error) {
	icon := ":red_circle:"
	switch {
	case p.resolved():
		icon = ":large_green_circle:"
	case p.Severity == "warning":
		icon = ":large_orange_circle:"
	case p.Severity == "info":
		icon = ":large_blue_circle:"
	}
	if p.reminder() {
		icon = ":repeat: " + icon
	}
	head := icon + " " + p.Headline()
	fields := []slackText{
		{"mrkdwn", "*Severity*\n" + orDefault(p.Severity, "warning")},
		{"mrkdwn", "*Value*\n" + p.ValueText()},
	}
	if t := p.Target(); t != "" {
		fields = append(fields, slackText{"mrkdwn", "*Target*\n" + t})
	}
	if p.StartedAt > 0 {
		at := time.UnixMilli(p.StartedAt)
		fields = append(fields, slackText{"mrkdwn", fmt.Sprintf("*Started*\n<!date^%d^{date_short_pretty} {time}|%s>", at.Unix(), at.UTC().Format(time.RFC3339))})
	}
	blocks := []any{
		map[string]any{"type": "header", "text": slackText{"plain_text", truncate(head, 150)}},
		map[string]any{"type": "section", "fields": fields},
	}
	if c := p.CauseText(); c != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": slackText{"mrkdwn", "*Likely cause:* " + c}})
	}
	if p.CodeRef != "" {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{slackText{"mrkdwn", "Defined in code: `" + p.CodeRef + "`"}}})
	}
	if p.Link != "" {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []any{map[string]any{
			"type": "button", "text": slackText{"plain_text", "Open incident"}, "url": p.Link,
		}}})
	}
	return json.Marshal(map[string]any{"text": head, "blocks": blocks})
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ─────────────── PagerDuty Events API v2 ───────────────

// PagerDutyDedupKey ties trigger and resolve of one incident together.
func PagerDutyDedupKey(p Payload) string { return fmt.Sprintf("tt-incident-%d", p.IncidentID) }

// PagerDutyEvent builds an Events v2 trigger (firing) or resolve body.
func PagerDutyEvent(p Payload, routingKey string) ([]byte, error) {
	ev := map[string]any{"routing_key": routingKey, "dedup_key": PagerDutyDedupKey(p)}
	if p.resolved() {
		ev["event_action"] = "resolve"
		return json.Marshal(ev)
	}
	sev := p.Severity
	switch sev {
	case "critical", "error", "warning", "info":
	default:
		sev = "warning"
	}
	ts := time.Now().UTC()
	if p.StartedAt > 0 {
		ts = time.UnixMilli(p.StartedAt).UTC()
	}
	// reminders re-trigger with the same dedup_key: PagerDuty appends to the open alert instead of paging anew
	ev["event_action"] = "trigger"
	ev["payload"] = map[string]any{
		"summary":   truncate(p.Headline()+" — "+p.ValueText(), 1024),
		"source":    orDefault(p.Service, "tasktrooper-telemetry"),
		"severity":  sev,
		"timestamp": ts.Format(time.RFC3339),
		"component": p.Endpoint,
		"class":     p.Kind,
		"custom_details": map[string]any{
			"incidentId": p.IncidentID, "monitorId": p.MonitorID, "kpiId": p.KpiID, "value": p.Value,
			"threshold": p.Threshold, "rootCause": p.CauseText(), "reminder": p.Reminder, "codeRef": p.CodeRef,
		},
	}
	if p.Link != "" {
		ev["links"] = []any{map[string]string{"href": p.Link, "text": "Open incident"}}
	}
	return json.Marshal(ev)
}

// ─────────────── generic webhook ───────────────

// WebhookBody is { version, event, incident }.
func WebhookBody(p Payload, kind string) ([]byte, error) {
	return json.Marshal(map[string]any{"version": 1, "event": kind, "incident": p})
}

// ─────────────── Opsgenie Alert API ───────────────

// OpsgenieAlias groups create/close of one incident.
func OpsgenieAlias(p Payload) string { return PagerDutyDedupKey(p) }

// OpsgenieCreate builds a create-alert body.
func OpsgenieCreate(p Payload) ([]byte, error) {
	prio := map[string]string{"critical": "P1", "warning": "P3", "info": "P5"}[p.Severity]
	if prio == "" {
		prio = "P3"
	}
	return json.Marshal(map[string]any{
		"message": truncate(p.Headline(), 130), "alias": OpsgenieAlias(p), "priority": prio, "source": "tasktrooper-telemetry",
		"description": p.ValueText() + "\n" + p.CauseText() + "\n" + p.Link,
		"details":     map[string]string{"service": p.Service, "endpoint": p.Endpoint, "incidentId": fmt.Sprint(p.IncidentID)},
	})
}

// ─────────────── e-mail ───────────────

// EmailMessage builds an RFC 5322 plain-text message.
func EmailMessage(p Payload, from string, to []string, now time.Time) []byte {
	var b strings.Builder
	subject := p.Headline()
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n",
		from, strings.Join(to, ", "), mimeHeader(subject), now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "%s\r\n\r\n", subject)
	if t := p.Target(); t != "" {
		fmt.Fprintf(&b, "Target:   %s\r\n", t)
	}
	fmt.Fprintf(&b, "Severity: %s\r\nValue:    %s\r\n", orDefault(p.Severity, "warning"), p.ValueText())
	if p.StartedAt > 0 {
		fmt.Fprintf(&b, "Started:  %s\r\n", time.UnixMilli(p.StartedAt).UTC().Format(time.RFC3339))
	}
	if p.ResolvedAt > 0 {
		fmt.Fprintf(&b, "Resolved: %s\r\n", time.UnixMilli(p.ResolvedAt).UTC().Format(time.RFC3339))
	}
	if c := p.CauseText(); c != "" {
		fmt.Fprintf(&b, "\r\nLikely cause: %s\r\n", c)
	}
	if p.CodeRef != "" {
		fmt.Fprintf(&b, "Defined in: %s\r\n", p.CodeRef)
	}
	if p.Link != "" {
		fmt.Fprintf(&b, "\r\n%s\r\n", p.Link)
	}
	return []byte(b.String())
}

// mimeHeader Q-encodes non-ASCII subjects.
func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + b64(s) + "?="
		}
	}
	return s
}

// ─────────────── Microsoft Teams (Incoming Webhook / Workflows) ───────────────

// TeamsMessage builds an Adaptive Card message. Both the (legacy) Office 365 Incoming Webhook and the Power
// Automate "Post to a channel when a webhook request is received" Workflow accept this envelope.
func TeamsMessage(p Payload) ([]byte, error) {
	color := "attention"
	switch {
	case p.resolved():
		color = "good"
	case p.Severity == "warning":
		color = "warning"
	case p.Severity == "info":
		color = "accent"
	}
	facts := []map[string]string{
		{"title": "Severity", "value": orDefault(p.Severity, "warning")},
		{"title": "Value", "value": p.ValueText()},
	}
	if t := p.Target(); t != "" {
		facts = append(facts, map[string]string{"title": "Target", "value": t})
	}
	if p.StartedAt > 0 {
		facts = append(facts, map[string]string{"title": "Started", "value": time.UnixMilli(p.StartedAt).UTC().Format(time.RFC3339)})
	}
	if p.ResolvedAt > 0 {
		facts = append(facts, map[string]string{"title": "Resolved", "value": time.UnixMilli(p.ResolvedAt).UTC().Format(time.RFC3339)})
	}
	if p.reminder() {
		facts = append(facts, map[string]string{"title": "Reminder", "value": fmt.Sprintf("#%d — still firing, not acknowledged", p.Reminder)})
	}
	body := []any{
		map[string]any{"type": "TextBlock", "text": truncate(p.Headline(), 200), "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
		map[string]any{"type": "FactSet", "facts": facts},
	}
	if c := p.CauseText(); c != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": "**Likely cause:** " + c, "wrap": true})
	}
	if p.CodeRef != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": "Defined in code: " + p.CodeRef, "wrap": true, "isSubtle": true})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard", "version": "1.4", "body": body, "msteams": map[string]any{"width": "Full"},
	}
	if p.Link != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open incident", "url": p.Link}}
	}
	return json.Marshal(map[string]any{
		"type":    "message",
		"summary": truncate(p.Headline(), 150),
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil, "content": card,
		}},
	})
}
