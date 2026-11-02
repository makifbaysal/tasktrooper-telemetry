package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
	"github.com/tasktrooper/telemetry/services/api/internal/assistant"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/aiquota"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// assistantFake keeps threads, messages and attachments in memory on top of the session fake (user 5).
type assistantFake struct {
	*identityFake
	amu      sync.Mutex
	threads  map[string]*model.AssistantThread
	owner    map[string]int64
	msgs     map[string][]model.AssistantMessage
	files    map[string][2]string
	nextID   int
	listTen  tenant.Tenant
	reserved int
	usage    []aiquota.Usage
}

func newAssistantFake() *assistantFake {
	return &assistantFake{identityFake: newIdentityFake(), threads: map[string]*model.AssistantThread{}, owner: map[string]int64{},
		msgs: map[string][]model.AssistantMessage{}, files: map[string][2]string{}}
}

func (f *assistantFake) id() string { f.nextID++; return strconv.Itoa(f.nextID) }

func (f *assistantFake) ListServices(ctx context.Context, q store.Q) ([]model.Service, error) {
	f.listTen, _ = tenant.From(ctx)
	return []model.Service{{ID: "checkout", Name: "checkout"}}, nil
}

func (f *assistantFake) ListAssistantThreads(_ context.Context, uid int64) ([]model.AssistantThread, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	out := []model.AssistantThread{}
	for id, t := range f.threads {
		if f.owner[id] == uid {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *assistantFake) CreateAssistantThread(_ context.Context, uid int64, title string) (*model.AssistantThread, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	t := &model.AssistantThread{ID: f.id(), Title: store.AssistantTitle(title)}
	f.threads[t.ID], f.owner[t.ID] = t, uid
	return t, nil
}

func (f *assistantFake) GetAssistantThread(_ context.Context, uid int64, id string) (*model.AssistantThread, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	if t, ok := f.threads[id]; ok && f.owner[id] == uid {
		c := *t
		return &c, nil
	}
	return nil, store.ErrNotFound
}

func (f *assistantFake) ListAssistantMessages(_ context.Context, uid int64, id string) ([]model.AssistantMessage, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	if f.owner[id] != uid {
		return nil, store.ErrNotFound
	}
	return append([]model.AssistantMessage{}, f.msgs[id]...), nil
}

func (f *assistantFake) AddAssistantMessage(_ context.Context, uid int64, id, role string, content any, tr *model.AssistantTurn) (model.AssistantMessage, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	c, _ := json.Marshal(content)
	m := model.AssistantMessage{ID: f.id(), Role: role, Content: c, Transcript: clone(tr)}
	f.msgs[id] = append(f.msgs[id], m)
	if u, ok := content.(model.AssistantUserContent); ok && f.threads[id].Title == "" {
		f.threads[id].Title = store.UserTitle(u)
	}
	return m, nil
}

func clone(t *model.AssistantTurn) *model.AssistantTurn {
	if t == nil {
		return nil
	}
	b, _ := json.Marshal(t)
	var c model.AssistantTurn
	_ = json.Unmarshal(b, &c)
	return &c
}

func (f *assistantFake) UpdateAssistantMessage(_ context.Context, uid int64, id, mid string, content any, tr *model.AssistantTurn) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	for i, m := range f.msgs[id] {
		if m.ID == mid {
			c, _ := json.Marshal(content)
			f.msgs[id][i].Content, f.msgs[id][i].Transcript = c, clone(tr)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *assistantFake) CreateAssistantAttachment(_ context.Context, uid int64, mt string, data []byte) (*model.AssistantAttachment, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	id := "img-" + f.id()
	f.files[id] = [2]string{mt, string(data)}
	return &model.AssistantAttachment{ID: id, MediaType: mt, Size: len(data)}, nil
}

func (f *assistantFake) GetAssistantAttachment(_ context.Context, uid int64, id string) (string, []byte, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	v, ok := f.files[id]
	if !ok {
		return "", nil, store.ErrNotFound
	}
	return v[0], []byte(v[1]), nil
}

func (f *assistantFake) AssistantEstimate(string, int, int64) float64  { return 1 }
func (f *assistantFake) AssistantCredits(string, int64, int64) float64 { return 1 }
func (f *assistantFake) AssistantReserve(context.Context, int64, int64, int64, float64) (aiquota.Decision, error) {
	f.reserved++
	return aiquota.Decision{Allowed: true}, nil
}
func (f *assistantFake) AssistantRecord(_ context.Context, u aiquota.Usage) error {
	f.usage = append(f.usage, u)
	return nil
}

// writeScript asks for a monitor (approval), then answers.
type writeScript struct{ n int }

func (w *writeScript) Name() string  { return "claude" }
func (w *writeScript) Model() string { return "test-model" }
func (w *writeScript) Step(_ context.Context, req assistant.StepRequest, onText func(string)) (assistant.StepResult, error) {
	w.n++
	if w.n == 1 {
		return assistant.StepResult{Blocks: []model.TBlock{{Type: "tool_use", ID: "w1", Name: "tt_create_monitor",
			Input: json.RawMessage(`{"confirm":true,"monitor":{"name":"checkout errors","kind":"error_rate","target":{"service":"checkout"},"condition":{"op":">","threshold":0.02},"severity":"critical","channels":[]}}`)}}}, nil
	}
	last := req.Messages[len(req.Messages)-1]
	onText("result: " + last.Blocks[0].Content)
	return assistant.StepResult{Blocks: []model.TBlock{{Type: "text", Text: "result: " + last.Blocks[0].Content}},
		Usage: model.AssistantUsage{InputTokens: 3, OutputTokens: 2}}, nil
}

func newAssistantServer(t *testing.T, f *assistantFake, cfg *AssistantConfig) http.Handler {
	f.t = t
	s := &Server{Store: f, AI: ai.NewMock(), Auth: auth.Static{"good": {OrgID: 7, ProjectID: 42}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Origins: []string{"http://localhost:5173"},
		Now: func() time.Time { return testNow }, Assistant: cfg}
	return s.Handler()
}

type sseEvent struct {
	Event string
	Data  map[string]any
}

func readSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	var ev string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var d map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d); err != nil {
				t.Fatalf("bad data %q", line)
			}
			out = append(out, sseEvent{ev, d})
		}
	}
	return out
}

func names(evs []sseEvent) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Event)
	}
	return strings.Join(s, ",")
}

func sessionDo(h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	return do(h, method, path, "", body, append([]string{"Cookie", "tt_session=sess-5", "Content-Type", "application/json"}, hdr...)...)
}

func TestAssistantMockTurnUsesToolsInProcess(t *testing.T) {
	f := newAssistantFake()
	h := newAssistantServer(t, f, nil)

	if w := do(h, "GET", "/api/v1/assistant/threads", "good", ""); w.Code != http.StatusForbidden {
		t.Fatalf("api key: %d", w.Code)
	}
	w := sessionDo(h, "GET", "/api/v1/assistant/status", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"provider":"mock"`) || !strings.Contains(w.Body.String(), `"writeTools":true`) {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	w = sessionDo(h, "POST", "/api/v1/assistant/threads", "{}")
	if w.Code != http.StatusCreated {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var th model.AssistantThread
	_ = json.Unmarshal(w.Body.Bytes(), &th)

	w = sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"   "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty message: %d", w.Code)
	}
	w = sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"how are my services?","refs":[{"kind":"bogus","id":"x","label":"x"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad ref: %d", w.Code)
	}
	w = sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"how are my services?"}`)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("send %d %s", w.Code, w.Body)
	}
	evs := readSSE(t, w.Body.String())
	if got := names(evs); got != "message,message,text,tool,tool,text,done" {
		t.Fatalf("events %s\n%s", got, w.Body)
	}
	if evs[4].Data["state"] != "done" || evs[4].Data["name"] != "tt_list_services" {
		t.Fatalf("tool %+v", evs[4].Data)
	}
	// the tool reached the store through the router with the user's session and project
	if f.listTen.OrgID != 7 || f.listTen.ProjectID != 42 {
		t.Fatalf("inner tenant %+v", f.listTen)
	}
	w = sessionDo(h, "GET", "/api/v1/assistant/threads/"+th.ID, "")
	var detail struct {
		Thread   model.AssistantThread `json:"thread"`
		Messages []map[string]any      `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &detail)
	if detail.Thread.Title != "how are my services?" || len(detail.Messages) != 2 || detail.Messages[1]["role"] != "assistant" {
		t.Fatalf("detail %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "transcript") || strings.Contains(w.Body.String(), "Transcript") {
		t.Fatalf("transcript leaked: %s", w.Body)
	}
	if blocks := detail.Messages[1]["blocks"].([]any); len(blocks) != 3 {
		t.Fatalf("blocks %v", blocks)
	}
}

func TestAssistantApprovalCreatesMonitorAsUser(t *testing.T) {
	f := newAssistantFake()
	h := newAssistantServer(t, f, &AssistantConfig{Provider: &writeScript{}, AllowWrite: true})
	th, _ := f.CreateAssistantThread(context.Background(), 5, "")

	w := sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"alert on checkout errors"}`)
	evs := readSSE(t, w.Body.String())
	if got := names(evs); got != "message,message,approval,done" {
		t.Fatalf("events %s\n%s", got, w.Body)
	}
	if len(f.created) != 0 {
		t.Fatal("monitor created before approval")
	}
	if w := sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/approvals/nope", `{"approve":true}`); w.Code != http.StatusConflict {
		t.Fatalf("unknown approval %d", w.Code)
	}
	w = sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/approvals/w1", `{"approve":true}`)
	evs = readSSE(t, w.Body.String())
	if got := names(evs); got != "approval,tool,tool,text,done" {
		t.Fatalf("events %s\n%s", got, w.Body)
	}
	if len(f.created) != 1 || f.created[0].Source != "agent" {
		t.Fatalf("created %+v body %s", f.created, w.Body)
	}
	if f.reserved != 2 || len(f.usage) != 2 || f.usage[0].Ref != "assistant:"+th.ID {
		t.Fatalf("quota reserved %d usage %+v", f.reserved, f.usage)
	}
	if w := sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/approvals/w1", `{"approve":true}`); w.Code != http.StatusConflict {
		t.Fatalf("second approval %d", w.Code)
	}
}

func TestAssistantNewMessageRejectsPending(t *testing.T) {
	f := newAssistantFake()
	h := newAssistantServer(t, f, &AssistantConfig{Provider: &writeScript{}, AllowWrite: true})
	th, _ := f.CreateAssistantThread(context.Background(), 5, "")
	_ = sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"alert on checkout errors"}`)
	w := sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"never mind"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(f.created) != 0 {
		t.Fatal("rejected write ran")
	}
	if !strings.Contains(string(f.msgs[th.ID][1].Content), `"state":"rejected"`) {
		t.Fatalf("approval not rejected: %s", f.msgs[th.ID][1].Content)
	}
}

func TestAssistantAttachmentsAndViewer(t *testing.T) {
	f := newAssistantFake()
	h := newAssistantServer(t, f, nil)
	png := []byte("\x89PNG\r\n\x1a\n0000IHDR")
	body := `{"mediaType":"image/png","data":"` + base64.StdEncoding.EncodeToString(png) + `"}`
	w := sessionDo(h, "POST", "/api/v1/assistant/attachments", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload %d %s", w.Code, w.Body)
	}
	var att model.AssistantAttachment
	_ = json.Unmarshal(w.Body.Bytes(), &att)
	if w := sessionDo(h, "POST", "/api/v1/assistant/attachments", `{"mediaType":"image/jpeg","data":"`+base64.StdEncoding.EncodeToString(png)+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("type mismatch %d", w.Code)
	}
	if w := sessionDo(h, "GET", "/api/v1/assistant/attachments/"+att.ID, ""); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("get %d", w.Code)
	}
	// a viewer (project 80) can chat; the tool calls it makes stay read only
	w = sessionDo(h, "POST", "/api/v1/assistant/threads", `{"title":"viewer chat"}`, "X-Trooper-Project", "80")
	if w.Code != http.StatusCreated {
		t.Fatalf("viewer create %d %s", w.Code, w.Body)
	}
	var th model.AssistantThread
	_ = json.Unmarshal(w.Body.Bytes(), &th)
	w = sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"look","attachments":["`+att.ID+`"]}`, "X-Trooper-Project", "80")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: done") {
		t.Fatalf("viewer send %d %s", w.Code, w.Body)
	}
	if w := sessionDo(h, "POST", "/api/v1/assistant/threads/"+th.ID+"/messages", `{"text":"x","attachments":["img-missing"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown attachment %d", w.Code)
	}
}
