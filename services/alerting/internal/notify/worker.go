package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tasktrooper/telemetry/services/alerting/internal/obs"
)

// WorkerConfig tunes the outbox poller.
type WorkerConfig struct {
	Poll        time.Duration // 2s
	Batch       int           // 50
	Lease       time.Duration // claimed rows are invisible to other workers this long (2m)
	MaxAttempts int           // after this many failures a row is left with its error (10)
	Concurrency int           // parallel deliveries (8)
}

func (c *WorkerConfig) defaults() {
	if c.Poll <= 0 {
		c.Poll = 2 * time.Second
	}
	if c.Batch <= 0 {
		c.Batch = 50
	}
	if c.Lease <= 0 {
		c.Lease = 2 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 8
	}
}

// Worker polls the notifications outbox. Safe to run as several replicas: rows are claimed with
// FOR UPDATE SKIP LOCKED and leased by pushing next_at forward, then delivered outside the transaction
// (at-least-once; receivers dedupe by incident id / PagerDuty dedup_key).
type Worker struct {
	DB  *pgxpool.Pool
	D   *Deliverer
	Cfg WorkerConfig
	Log *slog.Logger
	Reg *obs.Registry
	Rnd func() float64
	// Secrets resolves "secret:<id>" values in channel config before delivery (nil: values used as stored).
	Secrets *SecretResolver
}

// Row is a claimed notification.
type Row struct {
	ID        int64
	Incident  *int64
	Channel   string
	Kind      string
	Payload   Payload
	Attempts  int
	ProjectID int64
}

// Run polls until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	w.Cfg.defaults()
	t := time.NewTicker(w.Cfg.Poll)
	defer t.Stop()
	for {
		for {
			n, err := w.Once(ctx)
			if err != nil {
				if ctx.Err() == nil {
					w.Log.Error("outbox poll failed", "err", err)
				}
				break
			}
			if n < w.Cfg.Batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Claim leases up to Batch due rows.
func (w *Worker) Claim(ctx context.Context) ([]Row, error) {
	w.Cfg.defaults()
	rows, err := w.DB.Query(ctx, `WITH due AS (
			SELECT id FROM notifications
			WHERE sent_at IS NULL AND next_at <= now() AND attempts < $1
			ORDER BY next_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE notifications n SET next_at = now() + make_interval(secs => $3)
		FROM due WHERE n.id = due.id
		RETURNING n.id, n.incident_id, n.channel, n.kind, n.payload, n.attempts,
			coalesce((SELECT project_id FROM incidents i WHERE i.id = n.incident_id), 0)`,
		w.Cfg.MaxAttempts, w.Cfg.Batch, w.Cfg.Lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Incident, &r.Channel, &r.Kind, &payload, &r.Attempts, &r.ProjectID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(payload, &r.Payload)
		if r.ProjectID == 0 {
			r.ProjectID = r.Payload.ProjectID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Once claims and delivers one batch; returns the number of rows claimed.
func (w *Worker) Once(ctx context.Context) (int, error) {
	rows, err := w.Claim(ctx)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	sem := make(chan struct{}, w.Cfg.Concurrency)
	var wg sync.WaitGroup
	for _, r := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(r Row) {
			defer func() { <-sem; wg.Done() }()
			w.handle(ctx, r)
		}(r)
	}
	wg.Wait()
	return len(rows), nil
}

func (w *Worker) handle(ctx context.Context, r Row) {
	ch, err := w.Resolve(ctx, r.ProjectID, r.Channel)
	if err == nil && w.Secrets != nil {
		ch.Config, err = w.Secrets.ResolveConfig(ctx, r.ProjectID, ch.Config)
	}
	if r.Payload.ProjectID == 0 {
		r.Payload.ProjectID = r.ProjectID
	}
	if r.Payload.IncidentID == 0 && r.Incident != nil {
		r.Payload.IncidentID = *r.Incident
	}
	if err == nil {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = w.D.Deliver(dctx, ch, r.Kind, r.Payload)
		cancel()
	}
	if err == nil {
		w.count("sent", ch.Type)
		if merr := w.markSent(ctx, r, ch); merr != nil {
			w.Log.Error("mark sent failed", "notification", r.ID, "err", merr)
		}
		return
	}
	terminal := IsPermanent(err) || r.Attempts+1 >= w.Cfg.MaxAttempts
	w.count(map[bool]string{true: "failed", false: "retry"}[terminal], ch.Type)
	w.Log.Warn("notification delivery failed", "notification", r.ID, "channel", r.Channel, "attempt", r.Attempts+1, "terminal", terminal, "err", err)
	if merr := w.markFailed(ctx, r, err, terminal); merr != nil {
		w.Log.Error("mark failed failed", "notification", r.ID, "err", merr)
	}
}

func (w *Worker) count(outcome, typ string) {
	if w.Reg != nil {
		w.Reg.Add("tt_notifier_deliveries_total", 1, "outcome", outcome, "type", orDefault(typ, "unknown"))
	}
}

// Resolve finds the channel by name within the project. Monitors store free text like "#orders-oncall" or
// "slack:#payments-alerts"; an exact name match wins, else the "type:" prefix is stripped and must match the type.
// Slack App targets (slack:<channel id>#<name>) and Slack webhook URLs (slack:https://hooks.slack.com/…) need no row.
func (w *Worker) Resolve(ctx context.Context, projectID int64, name string) (Channel, error) {
	name = strings.TrimSpace(name)
	if id, chName, ok := ParseSlackTarget(name); ok {
		return Channel{Name: name, Type: "slack_app", Config: map[string]any{"channelId": id, "channelName": chName}}, nil
	}
	if u := SlackWebhookTarget(name); u != "" {
		return Channel{Name: name, Type: "slack", Config: map[string]any{"webhookUrl": u}}, nil
	}
	bare, typ := name, ""
	if i := strings.IndexByte(name, ':'); i > 0 {
		switch p := strings.ToLower(name[:i]); p {
		case "slack", "pagerduty", "email", "webhook", "opsgenie", "teams":
			typ, bare = p, name[i+1:]
		case "msteams":
			typ, bare = "teams", name[i+1:]
		}
	}
	var c Channel
	var cfg []byte
	err := w.DB.QueryRow(ctx, `SELECT id, name, type, config FROM notification_channels
		WHERE project_id = $1 AND (name = $2 OR (name = $3 AND ($4 = '' OR type = $4)))
		ORDER BY (name = $2) DESC LIMIT 1`, projectID, name, bare, typ).Scan(&c.ID, &c.Name, &c.Type, &cfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, permanent("unknown notification channel %q in project %d", name, projectID)
	}
	if err != nil {
		return Channel{}, err
	}
	c.Config = DecodeConfig(cfg)
	return c, nil
}

func (w *Worker) markSent(ctx context.Context, r Row, ch Channel) error {
	return pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE notifications SET sent_at = now(), attempts = attempts + 1, error = NULL WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if r.Incident == nil {
			return nil
		}
		data, _ := json.Marshal(map[string]any{"channel": r.Channel, "type": ch.Type, "kind": r.Kind, "notificationId": r.ID})
		_, err := tx.Exec(ctx, `INSERT INTO incident_events (incident_id, kind, data) VALUES ($1, 'notified', $2)`, *r.Incident, data)
		return err
	})
}

func (w *Worker) markFailed(ctx context.Context, r Row, cause error, terminal bool) error {
	msg := cause.Error()
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	if terminal {
		_, err := w.DB.Exec(ctx, `UPDATE notifications SET attempts = greatest(attempts + 1, $2), error = $3 WHERE id = $1`, r.ID, w.Cfg.MaxAttempts, msg)
		return err
	}
	rnd := rand.Float64
	if w.Rnd != nil {
		rnd = w.Rnd
	}
	delay := Backoff(r.Attempts+1, rnd())
	var ra *RetryAfterError
	if errors.As(cause, &ra) && ra.After > 0 {
		delay = ra.After // Slack 429 Retry-After
	}
	_, err := w.DB.Exec(ctx, `UPDATE notifications SET attempts = attempts + 1, error = $2, next_at = now() + make_interval(secs => $3) WHERE id = $1`,
		r.ID, fmt.Sprintf("attempt %d: %s", r.Attempts+1, msg), delay.Seconds())
	return err
}
