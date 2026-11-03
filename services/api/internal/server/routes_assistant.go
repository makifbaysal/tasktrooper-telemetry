package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tasktrooper/telemetry/services/api/internal/assistant"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/mcp/toolset"
	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
)

// Dashboard assistant (ADR-033, .ai/assistant.md).

// AssistantStore is the assistant's persistence (store.DB).
type AssistantStore interface {
	ListAssistantThreads(ctx context.Context, userID int64) ([]model.AssistantThread, error)
	CreateAssistantThread(ctx context.Context, userID int64, title string) (*model.AssistantThread, error)
	GetAssistantThread(ctx context.Context, userID int64, id string) (*model.AssistantThread, error)
	RenameAssistantThread(ctx context.Context, userID int64, id, title string) (*model.AssistantThread, error)
	DeleteAssistantThread(ctx context.Context, userID int64, id string) error
	ListAssistantMessages(ctx context.Context, userID int64, threadID string) ([]model.AssistantMessage, error)
	AddAssistantMessage(ctx context.Context, userID int64, threadID, role string, content any, transcript *model.AssistantTurn) (model.AssistantMessage, error)
	UpdateAssistantMessage(ctx context.Context, userID int64, threadID, messageID string, content any, transcript *model.AssistantTurn) error
	CreateAssistantAttachment(ctx context.Context, userID int64, mediaType string, data []byte) (*model.AssistantAttachment, error)
	GetAssistantAttachment(ctx context.Context, userID int64, id string) (string, []byte, error)
	AssistantEstimate(modelID string, promptChars int, maxTokens int64) float64
	AssistantCredits(modelID string, in, out int64) float64
	AssistantReserve(ctx context.Context, orgID, projectID, userID int64, estimate float64) (aiquota.Decision, error)
	AssistantRecord(ctx context.Context, u aiquota.Usage) error
}

// AssistantConfig configures the assistant (nil Server.Assistant = offline mock with defaults).
type AssistantConfig struct {
	Provider   assistant.Provider // nil = mock
	MaxSteps   int
	MaxTokens  int64
	AllowWrite bool
}

const (
	assistantMaxText        = 20000
	assistantMaxRefs        = 20
	assistantMaxAttachments = 4
	assistantMaxImageBytes  = 5 << 20
	assistantStreamDeadline = 10 * time.Minute
)

var assistantRefKinds = map[string]bool{"service": true, "endpoint": true, "incident": true, "kpi": true, "monitor": true,
	"loadtest_run": true, "loadtest_scenario": true, "host": true, "database": true}

var assistantImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

func (s *Server) assistantConfig() AssistantConfig {
	c := AssistantConfig{AllowWrite: true}
	if s.Assistant != nil {
		c = *s.Assistant
	}
	if c.Provider == nil {
		c.Provider = assistant.NewMock()
	}
	return c
}

// threadLocks allows one running turn per thread (per api instance).
type threadLocks struct {
	mu   sync.Mutex
	busy map[string]bool
}

func (l *threadLocks) lock(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy == nil {
		l.busy = map[string]bool{}
	}
	if l.busy[id] {
		return false
	}
	l.busy[id] = true
	return true
}

func (l *threadLocks) unlock(id string) {
	l.mu.Lock()
	delete(l.busy, id)
	l.mu.Unlock()
}

// assistantUser requires a signed-in user; chat content stays out of the audit log (the tools' own writes are audited
// by their inner requests).
func (s *Server) assistantUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r.Context())
		if !p.Session || p.UserID == 0 {
			forbidden(w, "the assistant needs a signed-in user (api keys can use the MCP server instead)")
			return
		}
		p.audited = true
		next.ServeHTTP(w, r)
	})
}

func (s *Server) assistantRoutes(r chi.Router) {
	r.Route("/assistant", func(r chi.Router) {
		r.Use(s.assistantUser)
		r.Get("/status", func(w http.ResponseWriter, r *http.Request) {
			c := s.assistantConfig()
			writeJSON(w, http.StatusOK, model.AssistantStatus{Provider: c.Provider.Name(), Model: c.Provider.Model(), WriteTools: c.AllowWrite})
		})
		r.Get("/threads", func(w http.ResponseWriter, r *http.Request) {
			v, err := s.Store.ListAssistantThreads(r.Context(), principalOf(r.Context()).UserID)
			s.reply(w, r, v, err)
		})
		r.Post("/threads", func(w http.ResponseWriter, r *http.Request) {
			var in model.AssistantThreadInput
			if err := decodeOptional(r, &in); err != nil {
				s.fail(w, r, err)
				return
			}
			v, err := s.Store.CreateAssistantThread(r.Context(), principalOf(r.Context()).UserID, in.Title)
			s.created(w, r, v, err)
		})
		r.Get("/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
			uid := principalOf(r.Context()).UserID
			t, err := s.Store.GetAssistantThread(r.Context(), uid, param(r, "id"))
			if err != nil {
				s.fail(w, r, err)
				return
			}
			msgs, err := s.Store.ListAssistantMessages(r.Context(), uid, t.ID)
			s.reply(w, r, model.AssistantThreadDetail{Thread: *t, Messages: msgs}, err)
		})
		r.Patch("/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
			var in model.AssistantThreadInput
			if err := decode(r, &in); err != nil {
				s.fail(w, r, err)
				return
			}
			v, err := s.Store.RenameAssistantThread(r.Context(), principalOf(r.Context()).UserID, param(r, "id"), in.Title)
			s.reply(w, r, v, err)
		})
		r.Delete("/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
			s.noContent(w, r, s.Store.DeleteAssistantThread(r.Context(), principalOf(r.Context()).UserID, param(r, "id")))
		})
		r.Post("/threads/{id}/messages", s.assistantSend)
		r.Post("/threads/{id}/approvals/{toolUseId}", s.assistantApprove)
		r.Post("/attachments", s.assistantUpload)
		r.Get("/attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
			mt, data, err := s.Store.GetAssistantAttachment(r.Context(), principalOf(r.Context()).UserID, param(r, "id"))
			if err != nil {
				s.fail(w, r, err)
				return
			}
			w.Header().Set("Content-Type", mt)
			w.Header().Set("Cache-Control", "private, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(data)
		})
	})
}

func (s *Server) assistantUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, assistantMaxImageBytes*4/3+4096)
	var in model.AssistantAttachmentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, r, store.Invalid("body must be {mediaType, data} with base64 data of at most 5 MB"))
		return
	}
	if !assistantImageTypes[in.MediaType] {
		s.fail(w, r, store.Invalid("mediaType must be image/png, image/jpeg, image/gif or image/webp"))
		return
	}
	if i := strings.Index(in.Data, ","); strings.HasPrefix(in.Data, "data:") && i > 0 {
		in.Data = in.Data[i+1:]
	}
	data, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil || len(data) == 0 {
		s.fail(w, r, store.Invalid("data must be base64"))
		return
	}
	if len(data) > assistantMaxImageBytes {
		s.fail(w, r, store.Invalid("images are limited to 5 MB"))
		return
	}
	if got := http.DetectContentType(data); got != in.MediaType {
		s.fail(w, r, store.Invalid("data is not a "+in.MediaType+" image"))
		return
	}
	v, err := s.Store.CreateAssistantAttachment(r.Context(), principalOf(r.Context()).UserID, in.MediaType, data)
	s.created(w, r, v, err)
}

// ---- streaming ----

type sseWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sseWriter {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(assistantStreamDeadline))
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	e := &sseWriter{w: w, rc: rc}
	_ = rc.Flush()
	return e
}

func (e *sseWriter) send(event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, b)
	_ = e.rc.Flush()
}

// heartbeat keeps proxies from closing the stream during long tool calls.
func (e *sseWriter) heartbeat(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.mu.Lock()
			_, _ = e.w.Write([]byte(": ping\n\n"))
			_ = e.rc.Flush()
			e.mu.Unlock()
		}
	}
}

// ---- tools: the MCP surface served in process with the caller's session ----

// inProcess serves tool HTTP calls with the api router.
type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	// The tool context descends from the chat request: drop its chi routing state so the router routes afresh.
	in := req.Clone(context.WithValue(req.Context(), chi.RouteCtxKey, nil))
	in.RequestURI = req.URL.RequestURI()
	in.RemoteAddr = "127.0.0.1:0"
	if in.Body == nil {
		in.Body = http.NoBody
	}
	t.h.ServeHTTP(rec, in)
	res := rec.Result()
	res.Request = req
	return res, nil
}

func (s *Server) innerHandler() http.Handler {
	s.innerOnce.Do(func() {
		if s.router == nil {
			s.router = s.Handler()
		}
	})
	return s.router
}

func (s *Server) assistantTools(ctx context.Context, r *http.Request, allowWrite bool) (assistant.Tools, []assistant.ToolDef, func(), error) {
	cookie, _ := r.Cookie(sessionCookie)
	project := strconv.FormatInt(principalOf(r.Context()).ProjectID, 10)
	cs, err := toolset.Connect(ctx, toolset.Options{
		BaseURL:    "http://api.internal",
		Transport:  inProcess{h: s.innerHandler()},
		AllowWrite: allowWrite,
		Log:        s.Log,
		Timeout:    60 * time.Second,
		Auth: func(h http.Header) {
			if cookie != nil {
				h.Set("Cookie", (&http.Cookie{Name: sessionCookie, Value: cookie.Value}).String())
			}
			h.Set(projectHeader, project)
		},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	tools := assistant.MCPTools{Session: cs}
	defs, err := tools.List(ctx)
	if err != nil {
		_ = cs.Close()
		return nil, nil, nil, err
	}
	return tools, append(defs, assistant.WidgetDef()), func() { _ = cs.Close() }, nil
}

// assistantQuota gates model requests with the org's and the user's AI credits.
type assistantQuota struct {
	s                  AssistantStore
	org, project, user int64
	thread             string
}

func (q assistantQuota) Reserve(ctx context.Context, modelID string, promptChars int, maxTokens int64) (bool, string, error) {
	d, err := q.s.AssistantReserve(ctx, q.org, q.project, q.user, q.s.AssistantEstimate(modelID, promptChars, maxTokens))
	if err != nil {
		return false, "", err
	}
	return d.Allowed, d.Reason, nil
}

func (q assistantQuota) Record(ctx context.Context, modelID string, u model.AssistantUsage) error {
	uid := q.user
	return q.s.AssistantRecord(ctx, aiquota.Usage{OrgID: q.org, ProjectID: q.project, UserID: &uid, Model: modelID,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, Credits: q.s.AssistantCredits(modelID, u.InputTokens, u.OutputTokens),
		Ref: "assistant:" + q.thread})
}

func (s *Server) agent(r *http.Request, threadID, messageID string) *assistant.Agent {
	c := s.assistantConfig()
	p := principalOf(r.Context())
	a := &assistant.Agent{Provider: c.Provider, MaxSteps: c.MaxSteps, MaxTokens: c.MaxTokens,
		Image: func(ctx context.Context, id string) (string, []byte, error) {
			return s.Store.GetAssistantAttachment(ctx, p.UserID, id)
		},
		Save: func(ctx context.Context, t *assistant.Turn) error {
			return s.Store.UpdateAssistantMessage(ctx, p.UserID, threadID, messageID, t.Content, &t.Transcript)
		},
	}
	if c.Provider.Name() != "mock" {
		a.Quota = assistantQuota{s: s.Store, org: p.OrgID, project: p.ProjectID, user: p.UserID, thread: threadID}
	}
	return a
}

func turnOf(m model.AssistantMessage) *assistant.Turn {
	t := &assistant.Turn{}
	_ = json.Unmarshal(m.Content, &t.Content)
	if m.Transcript != nil {
		t.Transcript = *m.Transcript
	}
	return t
}

// runTurn runs fn with tools and reports the outcome on the stream.
func (s *Server) runTurn(r *http.Request, sse *sseWriter, a *assistant.Agent, t *assistant.Turn, messageID string, fn func(ctx context.Context, in assistant.Input) error, history []model.TMsg) {
	ctx := r.Context()
	hb, stop := context.WithCancel(ctx)
	defer stop()
	go sse.heartbeat(hb)

	c := s.assistantConfig()
	tools, defs, closeTools, err := s.assistantTools(ctx, r, c.AllowWrite)
	if err == nil {
		defer closeTools()
		p := principalOf(ctx)
		err = fn(ctx, assistant.Input{System: assistant.SystemPrompt(p.ProjectID, s.Now(), c.AllowWrite), History: history, Tools: tools, Defs: defs})
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		code, msg := "provider_error", err.Error()
		switch {
		case errors.Is(err, assistant.ErrQuota):
			code, msg = "quota_exceeded", "The AI quota for this month is used up (Settings → AI usage)."
		case errors.Is(err, assistant.ErrMaxSteps):
			code, msg = "max_steps", "Stopped after the maximum number of steps; ask a narrower question or continue."
		default:
			s.Log.WarnContext(r.Context(), "assistant turn failed", "err", err)
		}
		t.Fail(msg)
		if a.Save != nil {
			_ = a.Save(context.WithoutCancel(ctx), t)
		}
		sse.send("error", map[string]string{"message": msg, "code": code})
	}
	sse.send("done", map[string]any{"messageId": messageID, "usage": t.Content.Usage})
}

func (s *Server) assistantSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := principalOf(ctx).UserID
	var in model.AssistantSendInput
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	content, err := s.assistantUserContent(ctx, uid, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	thread, err := s.Store.GetAssistantThread(ctx, uid, param(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.assistantLocks.lock(thread.ID) {
		s.fail(w, r, store.Conflict("this chat is already answering; wait or stop it first"))
		return
	}
	defer s.assistantLocks.unlock(thread.ID)
	msgs, err := s.Store.ListAssistantMessages(ctx, uid, thread.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A new message answers pending approvals with "not approved".
	if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
		if t := turnOf(msgs[n-1]); t.RejectPending() {
			if err := s.Store.UpdateAssistantMessage(ctx, uid, thread.ID, msgs[n-1].ID, t.Content, &t.Transcript); err != nil {
				s.fail(w, r, err)
				return
			}
			c, _ := json.Marshal(t.Content)
			msgs[n-1].Content, msgs[n-1].Transcript = c, &t.Transcript
		}
	}
	user, err := s.Store.AddAssistantMessage(ctx, uid, thread.ID, "user", content, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t := &assistant.Turn{Content: model.AssistantReplyContent{Blocks: []model.AssistantBlock{}}}
	reply, err := s.Store.AddAssistantMessage(ctx, uid, thread.ID, "assistant", t.Content, &t.Transcript)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sse := startSSE(w)
	sse.send("message", map[string]string{"id": user.ID, "role": "user"})
	sse.send("message", map[string]string{"id": reply.ID, "role": "assistant"})
	a := s.agent(r, thread.ID, reply.ID)
	history := assistant.History(append(msgs, user))
	s.runTurn(r, sse, a, t, reply.ID, func(ctx context.Context, in assistant.Input) error {
		return a.Run(ctx, in, t, sse.send)
	}, history)
}

func (s *Server) assistantApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := principalOf(ctx).UserID
	var in model.AssistantApprovalInput
	if err := decode(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	thread, err := s.Store.GetAssistantThread(ctx, uid, param(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.assistantLocks.lock(thread.ID) {
		s.fail(w, r, store.Conflict("this chat is already answering; wait or stop it first"))
		return
	}
	defer s.assistantLocks.unlock(thread.ID)
	msgs, err := s.Store.ListAssistantMessages(ctx, uid, thread.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id := param(r, "toolUseId")
	n := len(msgs)
	if n == 0 || msgs[n-1].Role != "assistant" {
		s.fail(w, r, store.Conflict(assistant.ErrNoApproval.Error()))
		return
	}
	last := msgs[n-1]
	t := turnOf(last)
	pending := false
	for _, pid := range t.PendingApprovals() {
		pending = pending || pid == id
	}
	if !pending {
		s.fail(w, r, store.Conflict(assistant.ErrNoApproval.Error()))
		return
	}
	sse := startSSE(w)
	a := s.agent(r, thread.ID, last.ID)
	s.runTurn(r, sse, a, t, last.ID, func(ctx context.Context, inp assistant.Input) error {
		return a.Resolve(ctx, inp, t, id, in.Approve, sse.send)
	}, assistant.History(msgs[:n-1]))
}

// assistantUserContent validates a message: text and/or images, known mention kinds, owned attachments.
func (s *Server) assistantUserContent(ctx context.Context, uid int64, in model.AssistantSendInput) (model.AssistantUserContent, error) {
	c := model.AssistantUserContent{Text: strings.TrimSpace(in.Text), Refs: []model.AssistantRef{}, Attachments: []model.AssistantAttachmentRef{}}
	if len(c.Text) > assistantMaxText {
		return c, store.Invalid("message is longer than 20,000 characters")
	}
	if len(in.Refs) > assistantMaxRefs {
		return c, store.Invalid("at most 20 mentions per message")
	}
	for _, ref := range in.Refs {
		if !assistantRefKinds[ref.Kind] || strings.TrimSpace(ref.ID) == "" || len(ref.ID) > 300 || len(ref.Label) > 300 || len(ref.Service) > 300 {
			return c, store.Invalid("invalid mention " + strconv.Quote(ref.Kind))
		}
		c.Refs = append(c.Refs, ref)
	}
	if len(in.Attachments) > assistantMaxAttachments {
		return c, store.Invalid("at most 4 images per message")
	}
	for _, id := range in.Attachments {
		mt, _, err := s.Store.GetAssistantAttachment(ctx, uid, id)
		if errors.Is(err, store.ErrNotFound) {
			return c, store.Invalid("unknown attachment " + strconv.Quote(id))
		}
		if err != nil {
			return c, err
		}
		c.Attachments = append(c.Attachments, model.AssistantAttachmentRef{ID: id, MediaType: mt})
	}
	if c.Text == "" && len(c.Attachments) == 0 {
		return c, store.Invalid("message is empty")
	}
	return c, nil
}
