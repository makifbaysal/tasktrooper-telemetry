package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Slack App delivery (ADR-017, contracts §17): targets `slack:<channel id>#<name>` are posted with the org's bot
// token via chat.postMessage. The firing message's ts is kept in notification_messages; resolved / reminder /
// acknowledged notifications reply in its thread and chat.update the original (status colour, buttons).

// SlackAPIBase is the Web API root (TT_SLACK_API_URL overrides it for tests).
const SlackAPIBase = "https://slack.com/api"

// Action ids handled by the api's POST /slack/interactions.
const (
	SlackActionAck    = "tt_ack:"    // + incident id
	SlackActionMute1h = "tt_mute1h:" // + monitor id
)

var slackChannelID = regexp.MustCompile(`^[CGDZ][A-Z0-9]{6,}$`)

// ParseSlackTarget parses `slack:<channel id>[#name]`; ok=false for webhook / named-channel targets.
func ParseSlackTarget(target string) (channelID, name string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(target), "slack:")
	if !found {
		return "", "", false
	}
	id, name, _ := strings.Cut(rest, "#")
	if !slackChannelID.MatchString(id) {
		return "", "", false
	}
	return id, name, true
}

// SlackWebhookTarget returns the URL of a `slack:https://hooks.slack.com/...` target ("" otherwise). Only Slack's
// webhook host is accepted, so a target string cannot make the notifier call arbitrary URLs.
func SlackWebhookTarget(target string) string {
	rest, found := strings.CutPrefix(strings.TrimSpace(target), "slack:")
	if !found {
		return ""
	}
	u, err := url.Parse(rest)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "hooks.slack.com") {
		return ""
	}
	return rest
}

// SlackStore is the notifier's persistence for the Slack App.
type SlackStore interface {
	// BotToken returns the bot token of the Slack installation of the project's org ("" when not installed).
	BotToken(ctx context.Context, projectID int64) (string, error)
	// MessageRef returns the first message posted for (incident, target), if any.
	MessageRef(ctx context.Context, incidentID int64, target string) (ts, channel string, ok bool, err error)
	SaveMessageRef(ctx context.Context, incidentID int64, target, ts, channel string) error
}

// SlackApp posts through the Slack Web API.
type SlackApp struct {
	Base         string // default SlackAPIBase
	HTTP         *http.Client
	Store        SlackStore
	PublicAppURL string // TT_PUBLIC_APP_URL: "Open incident" when the payload has no link
	Now          func() time.Time
}

// RetryAfterError asks the worker to retry after a server-provided delay (Slack 429 Retry-After).
type RetryAfterError struct {
	Err   error
	After time.Duration
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// slackError is an ok:false answer.
type slackError struct{ method, code string }

func (e *slackError) Error() string { return "slack " + e.method + ": " + e.code }

// Deliver sends one notification kind (firing | resolved | reminder | acknowledged) to a Slack App target.
func (s *SlackApp) Deliver(ctx context.Context, target, channelID, channelName, kind string, p Payload) error {
	if s == nil || s.Store == nil {
		return permanent("slack app target %q: notifier has no Slack App support configured", target)
	}
	label := "#" + orDefault(channelName, channelID)
	token, err := s.Store.BotToken(ctx, p.ProjectID)
	if err != nil {
		return err
	}
	if token == "" {
		return permanent("slack app is not installed for the org of project %d (target %s)", p.ProjectID, label)
	}
	ts, ch, found, err := s.Store.MessageRef(ctx, p.IncidentID, target)
	if err != nil {
		return err
	}
	if kind == "firing" && found {
		return nil // already posted (a retry after the post succeeded)
	}
	if !found {
		msg := SlackAppMessage(p, s.link(p), s.now())
		msg["channel"] = channelID
		ts, ch, err = s.postJoining(ctx, token, channelID, label, msg)
		if err != nil {
			return err
		}
		return s.Store.SaveMessageRef(ctx, p.IncidentID, target, ts, ch)
	}
	// follow-up: thread reply + refresh the original message
	reply := map[string]any{"channel": ch, "thread_ts": ts, "text": SlackThreadText(p, s.now()), "unfurl_links": false}
	if _, _, err := s.postJoining(ctx, token, ch, label, reply); err != nil {
		return err
	}
	upd := SlackAppMessage(p, s.link(p), s.now())
	upd["channel"], upd["ts"] = ch, ts
	// best effort: the thread reply already went out; retrying would post it twice (message deleted, too old, 429 …)
	_, _ = s.call(ctx, token, "chat.update", upd)
	return nil
}

func (s *SlackApp) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SlackApp) link(p Payload) string {
	if p.Link != "" {
		return p.Link
	}
	if s.PublicAppURL != "" && p.IncidentID > 0 {
		return fmt.Sprintf("%s/incidents?id=%d", strings.TrimRight(s.PublicAppURL, "/"), p.IncidentID)
	}
	return ""
}

// postJoining calls chat.postMessage; on not_in_channel it joins (public channels) and retries once.
func (s *SlackApp) postJoining(ctx context.Context, token, channelID, label string, msg map[string]any) (ts, ch string, err error) {
	resp, err := s.call(ctx, token, "chat.postMessage", msg)
	var se *slackError
	if errors.As(err, &se) && se.code == "not_in_channel" {
		if _, jerr := s.call(ctx, token, "conversations.join", map[string]any{"channel": channelID}); jerr != nil {
			var je *slackError
			if errors.As(jerr, &je) {
				return "", "", permanent("slack: bot is not a member of %s and cannot join it (%s); invite the TaskTrooper app to the channel (/invite)", label, je.code)
			}
			return "", "", jerr
		}
		resp, err = s.call(ctx, token, "chat.postMessage", msg)
	}
	if err != nil {
		if errors.As(err, &se) {
			return "", "", classifySlack(se, label)
		}
		return "", "", err
	}
	ts, _ = resp["ts"].(string)
	ch, _ = resp["channel"].(string)
	if ch == "" {
		ch = channelID
	}
	return ts, ch, nil
}

// classifySlack maps Slack error codes: configuration problems are permanent, everything else is retried.
func classifySlack(se *slackError, label string) error {
	switch se.code {
	case "channel_not_found":
		return permanent("slack: channel %s not found (deleted, or private and the app was not invited)", label)
	case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "no_permission", "missing_scope", "not_allowed_token_type":
		return permanent("slack: bot token rejected (%s); reinstall the Slack app (Integrations → Slack)", se.code)
	case "is_archived":
		return permanent("slack: channel %s is archived", label)
	case "not_in_channel":
		return permanent("slack: bot is not a member of %s; invite the TaskTrooper app to the channel", label)
	case "msg_too_long", "invalid_blocks", "invalid_arguments", "too_many_attachments", "no_text", "restricted_action":
		return permanent("slack: message rejected (%s)", se.code)
	}
	return se
}

// call POSTs a JSON body to a Web API method and returns the decoded answer (ok:false → *slackError).
func (s *SlackApp) call(ctx context.Context, token, method string, body map[string]any) (map[string]any, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, permanent("slack %s: %v", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(orDefault(s.Base, SlackAPIBase), "/")+"/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, permanent("slack %s: %v", method, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err // network: retry
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusTooManyRequests {
		after := time.Minute
		if sec, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && sec >= 0 {
			after = time.Duration(sec) * time.Second
		}
		return nil, &RetryAfterError{Err: fmt.Errorf("slack %s: rate limited (retry after %s)", method, after), After: after}
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("slack %s: http %d", method, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, permanent("slack %s: http %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("slack %s: bad response: %v", method, err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		code, _ := out["error"].(string)
		return out, &slackError{method: method, code: orDefault(code, "unknown_error")}
	}
	return out, nil
}

// ─────────────── message content (Block Kit) ───────────────

// slackColor is the attachment bar colour per status.
func slackColor(p Payload) string {
	switch {
	case p.State == "resolved":
		return "#2EB67D"
	case p.State == "acknowledged":
		return "#7C7C7C"
	case p.Severity == "warning":
		return "#ECB22E"
	case p.Severity == "info":
		return "#36C5F0"
	}
	return "#E01E5A"
}

func slackStatus(p Payload) (emoji, status string) {
	switch {
	case p.State == "resolved":
		return "✅", "Resolved"
	case p.State == "acknowledged":
		if p.AckedBy != "" {
			return "👀", "Acknowledged by " + p.AckedBy
		}
		return "👀", "Acknowledged"
	case p.Reminder > 0:
		return "🔁", fmt.Sprintf("Still firing (reminder %d)", p.Reminder)
	case p.Severity == "warning":
		return "🟠", "Firing"
	case p.Severity == "info":
		return "🔵", "Firing"
	}
	return "🔴", "Firing"
}

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (p Payload) duration(now time.Time) string {
	if p.StartedAt <= 0 {
		return ""
	}
	end := now
	if p.ResolvedAt > 0 {
		end = time.UnixMilli(p.ResolvedAt)
	}
	d := end.Sub(time.UnixMilli(p.StartedAt))
	if d < 0 {
		d = 0
	}
	return fmtDuration(d)
}

// SlackAppMessage builds the chat.postMessage / chat.update body (without channel/ts): header with status emoji and
// title, a coloured attachment with fields (service, value vs threshold, duration, severity), "Likely cause"
// (summary + confidence; omitted without RCA), deploy / code context and Open · Ack · Mute 1h buttons.
func SlackAppMessage(p Payload, link string, now time.Time) map[string]any {
	emoji, status := slackStatus(p)
	head := truncate(emoji+" "+status+": "+p.Title, 150)
	fields := []slackText{{"mrkdwn", "*Status*\n" + status}, {"mrkdwn", "*Severity*\n" + orDefault(p.Severity, "warning")}}
	if t := p.Target(); t != "" {
		fields = append(fields, slackText{"mrkdwn", "*Service*\n" + t})
	}
	fields = append(fields, slackText{"mrkdwn", "*Value*\n" + p.ValueText()})
	if d := p.duration(now); d != "" {
		label := "*Duration*\n"
		if p.State != "resolved" {
			label = "*Firing for*\n"
		}
		fields = append(fields, slackText{"mrkdwn", label + d})
	}
	blocks := []any{map[string]any{"type": "section", "fields": fields}}
	if c := p.CauseText(); c != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": slackText{"mrkdwn", "*Likely cause:* " + c}})
	}
	var ctx []any
	if d := p.Deploy; d != nil && d.Version != "" {
		s := "🚀 Deploy `" + truncate(d.Version, 40) + "`"
		if d.URL != "" {
			s = "🚀 Deploy <" + d.URL + "|" + truncate(d.Version, 40) + ">"
		}
		if d.At > 0 {
			s += " " + fmtDuration(now.Sub(time.UnixMilli(d.At))) + " ago"
		}
		if d.Author != "" {
			s += " by " + d.Author
		}
		if d.CommitMessage != "" {
			s += " — " + truncate(strings.SplitN(d.CommitMessage, "\n", 2)[0], 80)
		}
		ctx = append(ctx, slackText{"mrkdwn", s})
	}
	if p.CodeRef != "" {
		ctx = append(ctx, slackText{"mrkdwn", "📄 Defined in code: `" + p.CodeRef + "`"})
	}
	if len(ctx) > 0 {
		blocks = append(blocks, map[string]any{"type": "context", "elements": ctx})
	}
	var buttons []any
	if link != "" {
		buttons = append(buttons, map[string]any{"type": "button", "text": slackText{"plain_text", "Open incident"}, "url": link, "action_id": "tt_open"})
	}
	if p.State != "resolved" && p.State != "acknowledged" && p.IncidentID > 0 {
		id := strconv.FormatInt(p.IncidentID, 10)
		buttons = append(buttons, map[string]any{"type": "button", "style": "primary", "text": slackText{"plain_text", "Ack"},
			"action_id": SlackActionAck + id, "value": id})
	}
	if p.State != "resolved" && p.MonitorID > 0 {
		id := strconv.FormatInt(p.MonitorID, 10)
		buttons = append(buttons, map[string]any{"type": "button", "text": slackText{"plain_text", "Mute 1h"},
			"action_id": SlackActionMute1h + id, "value": id})
	}
	if len(buttons) > 0 {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": buttons})
	}
	return map[string]any{
		"text":         head, // notification / fallback text
		"blocks":       []any{map[string]any{"type": "header", "text": slackText{"plain_text", head}}},
		"attachments":  []any{map[string]any{"color": slackColor(p), "blocks": blocks}},
		"unfurl_links": false,
	}
}

// SlackThreadText is the thread reply for resolved / reminder / acknowledged.
func SlackThreadText(p Payload, now time.Time) string {
	switch p.State {
	case "resolved":
		s := "✅ Resolved"
		if d := p.duration(now); d != "" {
			s += " after " + d
		}
		if p.Peak != nil {
			s += " — peak " + fnum(*p.Peak)
		}
		return s
	case "acknowledged":
		if p.AckedBy != "" {
			return "👀 Acknowledged by " + p.AckedBy
		}
		return "👀 Acknowledged"
	}
	s := fmt.Sprintf("🔁 Still firing (reminder %d) — %s", max(p.Reminder, 1), p.ValueText())
	if d := p.duration(now); d != "" {
		s += " · firing for " + d
	}
	return s
}
