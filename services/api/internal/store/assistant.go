package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

// Dashboard assistant storage (ADR-033, .ai/assistant.md). Every statement is project scoped ({{tenant}}) and
// filtered by the owning user: threads are private.

// AssistantTitleMax is the length a thread title is trimmed to.
const AssistantTitleMax = 80

// AssistantTitle derives a thread title from a message.
func AssistantTitle(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(t) > AssistantTitleMax {
		r := []rune(t)
		t = strings.TrimSpace(string(r[:AssistantTitleMax-1])) + "…"
	}
	return t
}

const threadSelect = `SELECT id, title, created_at, updated_at FROM assistant_threads`

func scanThread(row pgx.Row) (model.AssistantThread, error) {
	var t model.AssistantThread
	var id int64
	err := row.Scan(&id, &t.Title, &t.CreatedAt, &t.UpdatedAt)
	t.ID = strconv.FormatInt(id, 10)
	return t, err
}

// ListAssistantThreads returns the user's threads, newest first (max 100).
func (d *DB) ListAssistantThreads(ctx context.Context, userID int64) ([]model.AssistantThread, error) {
	rows, err := d.pg.Query(ctx, threadSelect+` WHERE {{tenant}} AND user_id = $1 ORDER BY updated_at DESC, id DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AssistantThread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateAssistantThread starts a thread.
func (d *DB) CreateAssistantThread(ctx context.Context, userID int64, title string) (*model.AssistantThread, error) {
	t, err := scanThread(d.pg.QueryRow(ctx, `INSERT INTO assistant_threads (org_id, project_id, user_id, title)
		VALUES ({{org}}, {{project}}, $1, $2) RETURNING id, title, created_at, updated_at`, userID, AssistantTitle(title)))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetAssistantThread returns one of the user's threads.
func (d *DB) GetAssistantThread(ctx context.Context, userID int64, id string) (*model.AssistantThread, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	t, err := scanThread(d.pg.QueryRow(ctx, threadSelect+` WHERE {{tenant}} AND user_id = $1 AND id = $2`, userID, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// RenameAssistantThread sets a thread's title.
func (d *DB) RenameAssistantThread(ctx context.Context, userID int64, id, title string) (*model.AssistantThread, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	title = AssistantTitle(title)
	if title == "" {
		return nil, Invalid("title is required")
	}
	t, err := scanThread(d.pg.QueryRow(ctx, `UPDATE assistant_threads SET title = $3 WHERE {{tenant}} AND user_id = $1 AND id = $2
		RETURNING id, title, created_at, updated_at`, userID, n, title))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteAssistantThread deletes a thread and its messages.
func (d *DB) DeleteAssistantThread(ctx context.Context, userID int64, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `DELETE FROM assistant_threads WHERE {{tenant}} AND user_id = $1 AND id = $2`, userID, n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAssistantMessages returns a thread's messages in order, transcripts included.
func (d *DB) ListAssistantMessages(ctx context.Context, userID int64, threadID string) ([]model.AssistantMessage, error) {
	n, err := parseID(threadID)
	if err != nil {
		return nil, err
	}
	rows, err := d.pg.Query(ctx, `SELECT m.id, m.role, m.content, m.transcript, m.created_at FROM assistant_messages m
		JOIN assistant_threads t ON t.id = m.thread_id
		WHERE {{tenant:m}} AND t.user_id = $1 AND m.thread_id = $2 ORDER BY m.id`, userID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AssistantMessage{}
	for rows.Next() {
		var m model.AssistantMessage
		var id int64
		var transcript []byte
		if err := rows.Scan(&id, &m.Role, &m.Content, &transcript, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ID = strconv.FormatInt(id, 10)
		if len(transcript) > 0 {
			var t model.AssistantTurn
			if err := json.Unmarshal(transcript, &t); err != nil {
				return nil, err
			}
			m.Transcript = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func transcriptJSON(t *model.AssistantTurn) ([]byte, error) {
	if t == nil {
		return nil, nil
	}
	return json.Marshal(t)
}

// AddAssistantMessage appends a message to one of the user's threads, bumps the thread and titles an untitled
// thread from a user message.
func (d *DB) AddAssistantMessage(ctx context.Context, userID int64, threadID, role string, content any, transcript *model.AssistantTurn) (model.AssistantMessage, error) {
	n, err := parseID(threadID)
	if err != nil {
		return model.AssistantMessage{}, err
	}
	c, err := json.Marshal(content)
	if err != nil {
		return model.AssistantMessage{}, err
	}
	tr, err := transcriptJSON(transcript)
	if err != nil {
		return model.AssistantMessage{}, err
	}
	title := ""
	if u, ok := content.(model.AssistantUserContent); ok {
		title = UserTitle(u)
	}
	var id int64
	var at time.Time
	err = d.pg.QueryRow(ctx, `WITH t AS (
			UPDATE assistant_threads SET updated_at = now(), title = CASE WHEN title = '' THEN $5 ELSE title END
			WHERE {{tenant}} AND user_id = $1 AND id = $2 RETURNING id, org_id, project_id)
		INSERT INTO assistant_messages (thread_id, org_id, project_id, role, content, transcript)
		SELECT t.id, t.org_id, t.project_id, $3, $4, $6 FROM t RETURNING id, created_at`,
		userID, n, role, c, title, tr).Scan(&id, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.AssistantMessage{}, ErrNotFound
	}
	if err != nil {
		return model.AssistantMessage{}, err
	}
	return model.AssistantMessage{ID: strconv.FormatInt(id, 10), Role: role, CreatedAt: at, Content: c, Transcript: transcript}, nil
}

// UpdateAssistantMessage replaces the content and transcript of a message (an assistant turn as it progresses).
func (d *DB) UpdateAssistantMessage(ctx context.Context, userID int64, threadID, messageID string, content any, transcript *model.AssistantTurn) error {
	tn, err := parseID(threadID)
	if err != nil {
		return err
	}
	mn, err := parseID(messageID)
	if err != nil {
		return err
	}
	c, err := json.Marshal(content)
	if err != nil {
		return err
	}
	tr, err := transcriptJSON(transcript)
	if err != nil {
		return err
	}
	tag, err := d.pg.Exec(ctx, `UPDATE assistant_messages m SET content = $4, transcript = $5
		FROM assistant_threads t WHERE t.id = m.thread_id AND {{tenant:m}} AND t.user_id = $1 AND m.thread_id = $2 AND m.id = $3`,
		userID, tn, mn, c, tr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateAssistantAttachment stores an uploaded image.
func (d *DB) CreateAssistantAttachment(ctx context.Context, userID int64, mediaType string, data []byte) (*model.AssistantAttachment, error) {
	id := uuid.New()
	_, err := d.pg.Exec(ctx, `INSERT INTO assistant_attachments (id, org_id, project_id, user_id, media_type, size, data)
		VALUES ($1, {{org}}, {{project}}, $2, $3, $4, $5)`, id, userID, mediaType, len(data), data)
	if err != nil {
		return nil, err
	}
	return &model.AssistantAttachment{ID: id.String(), MediaType: mediaType, Size: len(data)}, nil
}

// GetAssistantAttachment returns one of the user's images.
func (d *DB) GetAssistantAttachment(ctx context.Context, userID int64, id string) (string, []byte, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", nil, ErrNotFound
	}
	var mt string
	var data []byte
	err = d.pg.QueryRow(ctx, `SELECT media_type, data FROM assistant_attachments WHERE {{tenant}} AND user_id = $1 AND id = $2`, userID, u).Scan(&mt, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return mt, data, err
}

// ---- AI quota (ADR-018): feature assistant, ref assistant:<thread id> ----

// pgxTx adapts a transaction to aiquota.DB.
type pgxTx struct{ tx pgx.Tx }

func (p pgxTx) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.tx.Exec(ctx, sql, args...)
	return err
}
func (p pgxTx) QueryRow(ctx context.Context, sql string, args ...any) aiquota.Row {
	return p.tx.QueryRow(ctx, sql, args...)
}

// pgxPoolDB adapts the pool to aiquota.DB for Record.
type pgxPoolDB struct {
	q interface {
		Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
}

func (p pgxPoolDB) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.q.Exec(ctx, sql, args...)
	return err
}
func (p pgxPoolDB) QueryRow(ctx context.Context, sql string, args ...any) aiquota.Row {
	return p.q.QueryRow(ctx, sql, args...)
}

// AssistantEstimate is the pre-call credit estimate of one model request.
func (d *DB) AssistantEstimate(modelID string, promptChars int, maxTokens int64) float64 {
	return d.aiConfig().Prices.Estimate(modelID, promptChars, maxTokens)
}

// AssistantCredits converts real usage into credits.
func (d *DB) AssistantCredits(modelID string, in, out int64) float64 {
	return d.aiConfig().Prices.Credits(modelID, in, out)
}

// AssistantReserve checks that the estimate fits the org's and the user's monthly AI credits.
func (d *DB) AssistantReserve(ctx context.Context, orgID, projectID, userID int64, estimate float64) (aiquota.Decision, error) {
	var dec aiquota.Decision
	uid := userID
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var err error
		dec, err = d.aiConfig().Reserve(ctx, pgxTx{tx}, aiquota.Request{OrgID: orgID, ProjectID: projectID, UserID: &uid,
			Feature: aiquota.FeatureAssistant, EstimateCredits: estimate})
		return err
	})
	return dec, err
}

// AssistantRecord writes the real usage of a model request (feature empty = assistant; the incident summary passes rca).
func (d *DB) AssistantRecord(ctx context.Context, u aiquota.Usage) error {
	if u.Feature == "" {
		u.Feature = aiquota.FeatureAssistant
	}
	return aiquota.Record(ctx, pgxPoolDB{d.Pool}, u)
}

// UserTitle titles a thread from its first message: the text plus the mentioned items (the composer keeps mentions
// as chips, out of the text).
func UserTitle(u model.AssistantUserContent) string {
	t := u.Text
	var labels []string
	for _, r := range u.Refs {
		labels = append(labels, r.Label)
	}
	if len(labels) > 0 {
		t += " · " + strings.Join(labels, ", ")
	}
	if strings.TrimSpace(t) == "" && len(u.Attachments) > 0 {
		t = "Image"
	}
	return AssistantTitle(t)
}
