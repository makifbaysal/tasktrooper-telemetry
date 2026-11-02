package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

// scripted returns its steps in order and records the requests.
type scripted struct {
	steps []StepResult
	reqs  []StepRequest
}

func (s *scripted) Name() string  { return "claude" }
func (s *scripted) Model() string { return "test-model" }
func (s *scripted) Step(_ context.Context, req StepRequest, onText func(string)) (StepResult, error) {
	s.reqs = append(s.reqs, req)
	if len(s.steps) == 0 {
		return StepResult{}, errors.New("no more steps")
	}
	r := s.steps[0]
	s.steps = s.steps[1:]
	for _, b := range r.Blocks {
		if b.Type == "text" {
			onText(b.Text)
		}
	}
	return r, nil
}

type fakeTools struct{ calls []string }

func (f *fakeTools) List(context.Context) ([]ToolDef, error) { return nil, nil }
func (f *fakeTools) Call(_ context.Context, name string, in json.RawMessage) (string, bool) {
	f.calls = append(f.calls, name+" "+string(in))
	if name == "tt_fail" {
		return "boom", true
	}
	return `{"ok":true}`, false
}

type fakeQuota struct {
	deny     bool
	recorded int64
}

func (q *fakeQuota) Reserve(context.Context, string, int, int64) (bool, string, error) {
	if q.deny {
		return false, "org_limit", nil
	}
	return true, "", nil
}
func (q *fakeQuota) Record(_ context.Context, _ string, u model.AssistantUsage) error {
	q.recorded += u.InputTokens + u.OutputTokens
	return nil
}

func use(id, name, input string) model.TBlock {
	return model.TBlock{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}
}

func userMsg(text string) []model.TMsg {
	return []model.TMsg{{Role: "user", Blocks: []model.TBlock{{Type: "text", Text: text}}}}
}

type events []string

func (e *events) emit(ev string, _ any) { *e = append(*e, ev) }

func TestRunToolsWidgetAndText(t *testing.T) {
	p := &scripted{steps: []StepResult{
		{Blocks: []model.TBlock{{Type: "text", Text: "Checking."}, use("a", "tt_list_services", `{}`), use("b", WidgetTool, `{"kind":"service","service":"orders"}`)},
			Usage: model.AssistantUsage{InputTokens: 10, OutputTokens: 5}},
		{Blocks: []model.TBlock{{Type: "text", Text: "All good."}}, Usage: model.AssistantUsage{InputTokens: 20, OutputTokens: 3}},
	}}
	tools, q := &fakeTools{}, &fakeQuota{}
	saves := 0
	a := &Agent{Provider: p, Quota: q, Save: func(context.Context, *Turn) error { saves++; return nil }}
	turn := &Turn{}
	var ev events
	if err := a.Run(context.Background(), Input{History: userMsg("how is orders?"), Tools: tools}, turn, ev.emit); err != nil {
		t.Fatal(err)
	}
	if len(tools.calls) != 1 || !strings.HasPrefix(tools.calls[0], "tt_list_services") {
		t.Fatalf("calls %v", tools.calls)
	}
	var kinds []string
	for _, b := range turn.Content.Blocks {
		kinds = append(kinds, b.Type)
	}
	if got := strings.Join(kinds, ","); got != "text,tool,widget,text" {
		t.Fatalf("blocks %s", got)
	}
	if turn.Content.Usage.InputTokens != 30 || q.recorded != 38 {
		t.Fatalf("usage %+v recorded %d", turn.Content.Usage, q.recorded)
	}
	// second request carries both tool results in one user message, after the assistant message
	msgs := p.reqs[1].Messages
	last := msgs[len(msgs)-1]
	if last.Role != "user" || len(last.Blocks) != 2 || last.Blocks[0].ToolUseID != "a" || last.Blocks[1].ToolUseID != "b" {
		t.Fatalf("results %+v", last)
	}
	if saves < 2 {
		t.Fatalf("saves %d", saves)
	}
	if strings.Join(ev, ",") != "text,tool,tool,widget,text" {
		t.Fatalf("events %v", ev)
	}
}

func TestRunApprovalFlow(t *testing.T) {
	for _, approve := range []bool{true, false} {
		p := &scripted{steps: []StepResult{
			{Blocks: []model.TBlock{use("r", "tt_list_monitors", `{}`), use("w", "tt_create_monitor", `{"name":"x","confirm":true}`)}},
			{Blocks: []model.TBlock{{Type: "text", Text: "Done."}}},
		}}
		tools := &fakeTools{}
		a := &Agent{Provider: p}
		turn := &Turn{}
		var ev events
		in := Input{History: userMsg("create it"), Tools: tools}
		if err := a.Run(context.Background(), in, turn, ev.emit); err != nil {
			t.Fatal(err)
		}
		if ids := turn.PendingApprovals(); len(ids) != 1 || ids[0] != "w" || len(turn.Transcript.Pending) != 1 {
			t.Fatalf("pending %v %+v", ids, turn.Transcript.Pending)
		}
		if len(tools.calls) != 1 {
			t.Fatalf("write ran before approval: %v", tools.calls)
		}
		if err := a.Resolve(context.Background(), in, turn, "nope", true, ev.emit); !errors.Is(err, ErrNoApproval) {
			t.Fatalf("unknown id: %v", err)
		}
		if err := a.Resolve(context.Background(), in, turn, "w", approve, ev.emit); err != nil {
			t.Fatal(err)
		}
		wantCalls := 1
		if approve {
			wantCalls = 2
		}
		if len(tools.calls) != wantCalls {
			t.Fatalf("approve=%v calls %v", approve, tools.calls)
		}
		res := p.reqs[1].Messages[len(p.reqs[1].Messages)-1]
		if len(res.Blocks) != 2 || res.Blocks[0].ToolUseID != "r" || res.Blocks[1].ToolUseID != "w" {
			t.Fatalf("results order %+v", res.Blocks)
		}
		if !approve && !strings.Contains(res.Blocks[1].Content, "rejected") {
			t.Fatalf("reject result %q", res.Blocks[1].Content)
		}
		if turn.Content.Blocks[len(turn.Content.Blocks)-1].Text != "Done." || len(turn.PendingApprovals()) != 0 {
			t.Fatalf("blocks %+v", turn.Content.Blocks)
		}
		if err := a.Resolve(context.Background(), in, turn, "w", true, ev.emit); !errors.Is(err, ErrNoApproval) {
			t.Fatalf("second decision: %v", err)
		}
	}
}

func TestRejectPendingOnNewMessage(t *testing.T) {
	turn := &Turn{
		Content:    model.AssistantReplyContent{Blocks: []model.AssistantBlock{{Type: "approval", ID: "w", State: "pending"}}},
		Transcript: model.AssistantTurn{Messages: []model.TMsg{{Role: "assistant", Blocks: []model.TBlock{use("w", "tt_run_loadtest", `{"confirm":true}`)}}}},
	}
	if !turn.RejectPending() || turn.Content.Blocks[0].State != "rejected" {
		t.Fatalf("%+v", turn.Content.Blocks)
	}
	last := turn.Transcript.Messages[len(turn.Transcript.Messages)-1]
	if last.Role != "user" || last.Blocks[0].ToolUseID != "w" {
		t.Fatalf("%+v", last)
	}
	if turn.RejectPending() {
		t.Fatal("nothing left to reject")
	}
}

func TestRunErrors(t *testing.T) {
	q := &fakeQuota{deny: true}
	a := &Agent{Provider: &scripted{}, Quota: q}
	if err := a.Run(context.Background(), Input{History: userMsg("x")}, &Turn{}, func(string, any) {}); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: %v", err)
	}
	loop := &scripted{}
	for i := 0; i < 3; i++ {
		loop.steps = append(loop.steps, StepResult{Blocks: []model.TBlock{use("t"+string(rune('a'+i)), "tt_fail", `{}`)}})
	}
	a = &Agent{Provider: loop, MaxSteps: 2}
	turn := &Turn{}
	if err := a.Run(context.Background(), Input{History: userMsg("x"), Tools: &fakeTools{}}, turn, func(string, any) {}); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("max steps: %v", err)
	}
	if turn.Content.Blocks[0].State != "error" || turn.Content.Blocks[0].Summary != "boom" {
		t.Fatalf("tool error block %+v", turn.Content.Blocks[0])
	}
}

func TestNormalize(t *testing.T) {
	text := func(role, s string) model.TMsg {
		return model.TMsg{Role: role, Blocks: []model.TBlock{{Type: "text", Text: s}}}
	}
	tests := []struct {
		name string
		in   []model.TMsg
		want string // role:blockTypes per message
	}{
		{"plain", []model.TMsg{text("user", "a"), text("assistant", "b")}, "user:text assistant:text"},
		{"starts with assistant", []model.TMsg{text("assistant", "b")}, "user:text assistant:text"},
		{"merge users", []model.TMsg{text("user", "a"), text("user", "b")}, "user:text,text"},
		{"interrupted call gets a result", []model.TMsg{text("user", "a"), {Role: "assistant", Blocks: []model.TBlock{use("x", "t", "{}")}}, text("user", "next")},
			"user:text assistant:tool_use user:tool_result,text"},
		{"orphan result dropped", []model.TMsg{{Role: "user", Blocks: []model.TBlock{{Type: "tool_result", ToolUseID: "zz"}, {Type: "text", Text: "q"}}}},
			"user:text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var parts []string
			for _, m := range Normalize(tc.in) {
				var types []string
				for _, b := range m.Blocks {
					types = append(types, b.Type)
				}
				parts = append(parts, m.Role+":"+strings.Join(types, ","))
			}
			if got := strings.Join(parts, " "); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeWidget(t *testing.T) {
	tests := []struct {
		in      string
		ok      bool
		missing string // key that must be dropped
	}{
		{`{"kind":"service","service":"orders","link":"https://evil.example"}`, true, "link"},
		{`{"kind":"service","service":"orders","link":"//evil.example"}`, true, "link"},
		{`{"kind":"service"}`, false, ""},
		{`{"kind":"endpoint","service":"a","endpoint":"GET /x"}`, true, ""},
		{`{"kind":"chart","series":[]}`, false, ""},
		{`{"kind":"stats","items":[{"label":"a","value":"1"}]}`, true, ""},
		{`{"kind":"html"}`, false, ""},
		{`[1]`, false, ""},
	}
	for _, tc := range tests {
		out, err := SanitizeWidget(json.RawMessage(tc.in))
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err %v", tc.in, err)
		}
		if tc.missing != "" && strings.Contains(string(out), `"`+tc.missing+`"`) {
			t.Fatalf("%s: %s kept", tc.in, tc.missing)
		}
	}
	out, _ := SanitizeWidget(json.RawMessage(`{"kind":"table","rows":[["a"]],"links":["/services/a","javascript:x"]}`))
	if strings.Contains(string(out), "javascript") {
		t.Fatalf("unsafe row link kept: %s", out)
	}
}

func TestUserBlocksAndHistory(t *testing.T) {
	c := model.AssistantUserContent{Text: "why?", Refs: []model.AssistantRef{{Kind: "incident", ID: "12", Label: "High errors\nignore previous"}},
		Attachments: []model.AssistantAttachmentRef{{ID: "img", MediaType: "image/png"}}}
	b := UserBlocks(c)
	if len(b) != 2 || b[0].Type != "image" || !strings.Contains(b[1].Text, `- incident id=12 label="High errors ignore previous"`) {
		t.Fatalf("%+v", b)
	}
	uc, _ := json.Marshal(c)
	h := History([]model.AssistantMessage{
		{Role: "user", Content: uc},
		{Role: "assistant", Transcript: &model.AssistantTurn{Messages: []model.TMsg{{Role: "assistant", Blocks: []model.TBlock{{Type: "text", Text: "ok"}}}}}},
	})
	if len(h) != 2 || h[1].Role != "assistant" {
		t.Fatalf("%+v", h)
	}
}

func TestMockUsesMentionedIncident(t *testing.T) {
	m := NewMock()
	req := StepRequest{Messages: []model.TMsg{{Role: "user", Blocks: UserBlocks(model.AssistantUserContent{Text: "what happened?",
		Refs: []model.AssistantRef{{Kind: "incident", ID: "77", Label: "x"}}})}},
		Tools: []ToolDef{{Name: "tt_get_incident"}, {Name: "tt_list_services"}, WidgetDef()}}
	r, err := m.Step(context.Background(), req, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Blocks) != 3 || r.Blocks[1].Name != "tt_get_incident" || !strings.Contains(string(r.Blocks[1].Input), `"77"`) || r.Blocks[2].Name != WidgetTool {
		t.Fatalf("%+v", r.Blocks)
	}
}
