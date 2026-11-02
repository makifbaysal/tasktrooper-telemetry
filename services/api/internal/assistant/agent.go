package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/mcp/toolset"
)

// MaxToolResultBytes caps a tool result kept in the transcript (the MCP server already shapes results to 64 KiB).
const MaxToolResultBytes = 16 << 10

// Tools runs tool calls.
type Tools interface {
	List(ctx context.Context) ([]ToolDef, error)
	Call(ctx context.Context, name string, input json.RawMessage) (content string, isError bool)
}

// MCPTools adapts an MCP client session.
type MCPTools struct{ Session *mcp.ClientSession }

// List returns the session's tools.
func (t MCPTools) List(ctx context.Context) ([]ToolDef, error) {
	res, err := t.Session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ToolDef, 0, len(res.Tools))
	for _, tl := range res.Tools {
		var schema map[string]any
		if b, err := json.Marshal(tl.InputSchema); err == nil {
			_ = json.Unmarshal(b, &schema)
		}
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, ToolDef{Name: tl.Name, Description: tl.Description, Schema: schema})
	}
	return out, nil
}

// Call runs one tool and flattens its text content.
func (t MCPTools) Call(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return "tool input must be a JSON object", true
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	res, err := t.Session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "tool call failed: " + err.Error(), true
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// Quota gates model requests (ADR-018). nil = no quota (offline mock).
type Quota interface {
	Reserve(ctx context.Context, model string, promptChars int, maxTokens int64) (ok bool, reason string, err error)
	Record(ctx context.Context, model string, u model.AssistantUsage) error
}

// Emit sends one stream event (.ai/assistant.md § Stream events).
type Emit func(event string, data any)

// Turn is an assistant message being built: UI blocks + model transcript.
type Turn struct {
	Content    model.AssistantReplyContent
	Transcript model.AssistantTurn
}

// Agent runs turns.
type Agent struct {
	Provider  Provider
	MaxSteps  int
	MaxTokens int64
	Quota     Quota
	Image     ImageLoader
	// Save persists the turn after every step (nil = no-op).
	Save func(ctx context.Context, t *Turn) error
}

// Input is what a turn runs on.
type Input struct {
	System  string
	History []model.TMsg // the conversation before this turn, ending with the user's message
	Tools   Tools
	Defs    []ToolDef // tool list offered to the model (Tools.List + WidgetDef)
}

// ErrQuota is returned when the AI quota denies a request.
var ErrQuota = errors.New("quota_exceeded")

// ErrMaxSteps is returned when a turn used all its model requests.
var ErrMaxSteps = errors.New("max_steps")

func (a *Agent) save(ctx context.Context, t *Turn) {
	if a.Save != nil {
		_ = a.Save(context.WithoutCancel(ctx), t)
	}
}

func (t *Turn) appendText(s string) {
	n := len(t.Content.Blocks)
	if n > 0 && t.Content.Blocks[n-1].Type == "text" {
		t.Content.Blocks[n-1].Text += s
		return
	}
	t.Content.Blocks = append(t.Content.Blocks, model.AssistantBlock{Type: "text", Text: s})
}

func (t *Turn) block(id, typ string) *model.AssistantBlock {
	for i := range t.Content.Blocks {
		if b := &t.Content.Blocks[i]; b.ID == id && b.Type == typ {
			return b
		}
	}
	return nil
}

func (t *Turn) addUsage(u model.AssistantUsage) {
	if t.Content.Usage == nil {
		t.Content.Usage = &model.AssistantUsage{}
	}
	t.Content.Usage.InputTokens += u.InputTokens
	t.Content.Usage.OutputTokens += u.OutputTokens
}

// Fail records a failed turn as an error block.
func (t *Turn) Fail(msg string) {
	t.Content.Blocks = append(t.Content.Blocks, model.AssistantBlock{Type: "error", Text: msg})
}

// PendingApprovals lists the approval blocks still waiting for the user.
func (t *Turn) PendingApprovals() []string {
	var ids []string
	for _, b := range t.Content.Blocks {
		if b.Type == "approval" && b.State == "pending" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

func capResult(s string) string {
	if len(s) <= MaxToolResultBytes {
		return s
	}
	return s[:MaxToolResultBytes] + "\n…[truncated; narrow the query for the rest]"
}

func promptChars(system string, msgs []model.TMsg, defs []ToolDef) int {
	n := len(system)
	for _, m := range msgs {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Input) + len(b.Content)
			if b.Type == "image" {
				n += 6400 // ~1.6k tokens per image
			}
		}
	}
	if b, err := json.Marshal(defs); err == nil {
		n += len(b)
	}
	return n
}

// Run continues the turn until the model stops calling tools, an approval is needed, or it fails.
func (a *Agent) Run(ctx context.Context, in Input, t *Turn, emit Emit) error {
	maxSteps := a.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 12
	}
	maxTokens := a.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	for step := 0; step < maxSteps; step++ {
		msgs := Normalize(append(append([]model.TMsg{}, in.History...), t.Transcript.Messages...))
		if a.Quota != nil {
			ok, reason, err := a.Quota.Reserve(ctx, a.Provider.Model(), promptChars(in.System, msgs, in.Defs), maxTokens)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: %s", ErrQuota, reason)
			}
		}
		res, err := a.Provider.Step(ctx, StepRequest{System: in.System, Messages: msgs, Tools: in.Defs, MaxTokens: maxTokens, Image: a.Image},
			func(s string) {
				t.appendText(s)
				emit("text", map[string]string{"delta": s})
			})
		if err != nil {
			a.save(ctx, t)
			return err
		}
		t.addUsage(res.Usage)
		if a.Quota != nil {
			_ = a.Quota.Record(context.WithoutCancel(ctx), a.Provider.Model(), res.Usage)
		}
		reply := model.TMsg{Role: "assistant", Blocks: res.Blocks}
		t.Transcript.Messages = append(t.Transcript.Messages, reply)
		var uses []model.TBlock
		for _, b := range res.Blocks {
			if b.Type == "tool_use" {
				uses = append(uses, b)
			}
		}
		if len(uses) == 0 {
			a.save(ctx, t)
			return nil
		}
		var results []model.TBlock
		waiting := false
		for _, u := range uses {
			switch {
			case u.Name == WidgetTool:
				w, err := SanitizeWidget(u.Input)
				if err != nil {
					results = append(results, model.TBlock{Type: "tool_result", ToolUseID: u.ID, Content: err.Error(), IsError: true})
					continue
				}
				t.Content.Blocks = append(t.Content.Blocks, model.AssistantBlock{Type: "widget", Widget: w})
				emit("widget", map[string]json.RawMessage{"widget": w})
				results = append(results, model.TBlock{Type: "tool_result", ToolUseID: u.ID, Content: "Widget shown to the user."})
			case toolset.NeedsApproval(u.Input):
				waiting = true
				t.Content.Blocks = append(t.Content.Blocks, model.AssistantBlock{Type: "approval", ID: u.ID, Name: u.Name, Input: u.Input, State: "pending"})
				emit("approval", map[string]any{"id": u.ID, "name": u.Name, "input": u.Input})
			default:
				results = append(results, a.call(ctx, in.Tools, t, u, emit))
			}
		}
		if waiting {
			t.Transcript.Pending = results
			a.save(ctx, t)
			return nil
		}
		t.Transcript.Messages = append(t.Transcript.Messages, model.TMsg{Role: "user", Blocks: results})
		a.save(ctx, t)
	}
	return ErrMaxSteps
}

func (a *Agent) call(ctx context.Context, tools Tools, t *Turn, u model.TBlock, emit Emit) model.TBlock {
	t.Content.Blocks = append(t.Content.Blocks, model.AssistantBlock{Type: "tool", ID: u.ID, Name: u.Name, Input: u.Input, State: "running"})
	emit("tool", map[string]any{"id": u.ID, "name": u.Name, "input": u.Input, "state": "running"})
	content, isErr := tools.Call(ctx, u.Name, u.Input)
	b := t.block(u.ID, "tool")
	b.State = "done"
	if isErr {
		b.State, b.Summary = "error", truncate(content, 300)
	}
	emit("tool", map[string]any{"id": u.ID, "name": u.Name, "input": u.Input, "state": b.State, "summary": b.Summary})
	return model.TBlock{Type: "tool_result", ToolUseID: u.ID, Content: capResult(content), IsError: isErr}
}

// ErrNoApproval is returned for an unknown or already decided approval.
var ErrNoApproval = errors.New("no pending approval with this id")

// Resolve applies the user's decision on a pending write call. When it was the last pending approval the collected
// results are sent and the turn continues.
func (a *Agent) Resolve(ctx context.Context, in Input, t *Turn, id string, approve bool, emit Emit) error {
	b := t.block(id, "approval")
	if b == nil || b.State != "pending" {
		return ErrNoApproval
	}
	var use model.TBlock
	if n := len(t.Transcript.Messages); n > 0 {
		for _, u := range t.Transcript.Messages[n-1].Blocks {
			if u.Type == "tool_use" && u.ID == id {
				use = u
			}
		}
	}
	if use.ID == "" {
		return ErrNoApproval
	}
	if approve {
		b.State = "approved"
		emit("approval", map[string]any{"id": id, "name": b.Name, "input": b.Input, "state": "approved"})
		t.Transcript.Pending = append(t.Transcript.Pending, a.call(ctx, in.Tools, t, use, emit))
	} else {
		b.State = "rejected"
		emit("approval", map[string]any{"id": id, "name": b.Name, "input": b.Input, "state": "rejected"})
		t.Transcript.Pending = append(t.Transcript.Pending, model.TBlock{Type: "tool_result", ToolUseID: id,
			Content: "The user rejected this action; nothing was changed. Do not retry it unless they ask."})
	}
	if len(t.PendingApprovals()) > 0 {
		a.save(ctx, t)
		return nil
	}
	t.flushPending()
	a.save(ctx, t)
	return a.Run(ctx, in, t, emit)
}

// RejectPending rejects every pending approval (the user sent a new message instead) without running the model.
func (t *Turn) RejectPending() bool {
	ids := t.PendingApprovals()
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		t.block(id, "approval").State = "rejected"
		t.Transcript.Pending = append(t.Transcript.Pending, model.TBlock{Type: "tool_result", ToolUseID: id,
			Content: "Not approved: the user sent a new message instead. Nothing was changed."})
	}
	t.flushPending()
	return true
}

// flushPending sends the collected results in the order of the tool calls.
func (t *Turn) flushPending() {
	n := len(t.Transcript.Messages)
	if n == 0 {
		return
	}
	byID := map[string]model.TBlock{}
	for _, r := range t.Transcript.Pending {
		byID[r.ToolUseID] = r
	}
	var results []model.TBlock
	for _, u := range t.Transcript.Messages[n-1].Blocks {
		if u.Type == "tool_use" {
			if r, ok := byID[u.ID]; ok {
				results = append(results, r)
			}
		}
	}
	t.Transcript.Pending = nil
	t.Transcript.Messages = append(t.Transcript.Messages, model.TMsg{Role: "user", Blocks: results})
}

// Normalize makes a message list valid for the model: every tool call gets a result (an interrupted turn gets an
// error result), consecutive messages of one role are merged, and the list starts with a user message.
func Normalize(in []model.TMsg) []model.TMsg {
	var out []model.TMsg
	for i := 0; i < len(in); i++ {
		m := in[i]
		out = append(out, model.TMsg{Role: m.Role, Blocks: append([]model.TBlock{}, m.Blocks...)})
		if m.Role != "assistant" {
			continue
		}
		var missing []model.TBlock
		have := map[string]bool{}
		if i+1 < len(in) && in[i+1].Role == "user" {
			for _, b := range in[i+1].Blocks {
				if b.Type == "tool_result" {
					have[b.ToolUseID] = true
				}
			}
		}
		for _, b := range m.Blocks {
			if b.Type == "tool_use" && !have[b.ID] {
				missing = append(missing, model.TBlock{Type: "tool_result", ToolUseID: b.ID, Content: "Interrupted: this call did not run.", IsError: true})
			}
		}
		if len(missing) > 0 {
			out = append(out, model.TMsg{Role: "user", Blocks: missing})
		}
	}
	// merge same-role neighbours; tool results stay first in a user message
	var merged []model.TMsg
	for _, m := range out {
		if len(m.Blocks) == 0 {
			continue
		}
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role {
			prev := &merged[n-1]
			var results, rest []model.TBlock
			for _, b := range append(prev.Blocks, m.Blocks...) {
				if b.Type == "tool_result" {
					results = append(results, b)
				} else {
					rest = append(rest, b)
				}
			}
			prev.Blocks = append(results, rest...)
			continue
		}
		merged = append(merged, m)
	}
	// drop tool results whose call is not in the preceding assistant message (e.g. after trimming)
	for i := range merged {
		if merged[i].Role != "user" {
			continue
		}
		calls := map[string]bool{}
		if i > 0 {
			for _, b := range merged[i-1].Blocks {
				if b.Type == "tool_use" {
					calls[b.ID] = true
				}
			}
		}
		kept := merged[i].Blocks[:0:0]
		for _, b := range merged[i].Blocks {
			if b.Type != "tool_result" || calls[b.ToolUseID] {
				kept = append(kept, b)
			}
		}
		if len(kept) == 0 {
			kept = []model.TBlock{{Type: "text", Text: "(continue)"}}
		}
		merged[i].Blocks = kept
	}
	if len(merged) > 0 && merged[0].Role != "user" {
		merged = append([]model.TMsg{{Role: "user", Blocks: []model.TBlock{{Type: "text", Text: "(conversation start)"}}}}, merged...)
	}
	return merged
}

// History rebuilds the model conversation from stored messages.
func History(msgs []model.AssistantMessage) []model.TMsg {
	var out []model.TMsg
	for _, m := range msgs {
		switch m.Role {
		case "user":
			var c model.AssistantUserContent
			if err := json.Unmarshal(m.Content, &c); err != nil {
				continue
			}
			out = append(out, model.TMsg{Role: "user", Blocks: UserBlocks(c)})
		case "assistant":
			if m.Transcript != nil {
				out = append(out, m.Transcript.Messages...)
			}
		}
	}
	return out
}
