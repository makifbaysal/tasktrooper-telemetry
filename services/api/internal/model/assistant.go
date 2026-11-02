package model

import (
	"encoding/json"
	"time"
)

// Dashboard assistant (ADR-033, .ai/assistant.md).

// AssistantThread is one chat of a user in a project.
type AssistantThread struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AssistantThreadInput is the body of POST / PATCH /assistant/threads.
type AssistantThreadInput struct {
	Title string `json:"title"`
}

// AssistantRef is an item the user mentioned with @.
type AssistantRef struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Label   string `json:"label"`
	Service string `json:"service,omitempty"`
}

// AssistantAttachmentRef points to an uploaded image.
type AssistantAttachmentRef struct {
	ID        string `json:"id"`
	MediaType string `json:"mediaType"`
}

// AssistantUserContent is the stored content of a user message.
type AssistantUserContent struct {
	Text        string                   `json:"text"`
	Refs        []AssistantRef           `json:"refs"`
	Attachments []AssistantAttachmentRef `json:"attachments"`
}

// AssistantBlock is one UI block of an assistant message: text | tool | widget | approval | error.
type AssistantBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	State   string          `json:"state,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Widget  json.RawMessage `json:"widget,omitempty"`
}

// AssistantUsage is the token usage of an assistant turn.
type AssistantUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

// AssistantReplyContent is the stored content of an assistant message.
type AssistantReplyContent struct {
	Blocks []AssistantBlock `json:"blocks"`
	Usage  *AssistantUsage  `json:"usage,omitempty"`
}

// AssistantMessage is a message as the API returns it: id, role, createdAt plus the content fields of its role.
type AssistantMessage struct {
	ID        string
	Role      string
	CreatedAt time.Time
	Content   json.RawMessage
	// Transcript is never sent to the browser.
	Transcript *AssistantTurn
}

// MarshalJSON flattens the content next to id/role/createdAt.
func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	if len(m.Content) > 0 {
		if err := json.Unmarshal(m.Content, &out); err != nil {
			return nil, err
		}
	}
	out["id"], out["role"], out["createdAt"] = m.ID, m.Role, m.CreatedAt
	return json.Marshal(out)
}

// AssistantThreadDetail is GET /assistant/threads/{id}.
type AssistantThreadDetail struct {
	Thread   AssistantThread    `json:"thread"`
	Messages []AssistantMessage `json:"messages"`
}

// AssistantSendInput is the body of POST /assistant/threads/{id}/messages.
type AssistantSendInput struct {
	Text        string         `json:"text"`
	Refs        []AssistantRef `json:"refs"`
	Attachments []string       `json:"attachments"`
}

// AssistantApprovalInput is the body of POST /assistant/threads/{id}/approvals/{toolUseId}.
type AssistantApprovalInput struct {
	Approve bool `json:"approve"`
}

// AssistantAttachmentInput is the body of POST /assistant/attachments (data is base64).
type AssistantAttachmentInput struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
}

// AssistantAttachment is the reply of POST /assistant/attachments.
type AssistantAttachment struct {
	ID        string `json:"id"`
	MediaType string `json:"mediaType"`
	Size      int    `json:"size"`
}

// AssistantStatus is GET /assistant/status.
type AssistantStatus struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	WriteTools bool   `json:"writeTools"`
}

// ---- model transcript (provider neutral, stored per assistant turn) ----

// AssistantTurn is the transcript of one assistant turn: the model-format messages it added (assistant replies with
// tool calls, user messages with tool results) and, while an approval is pending, the results already collected for
// the last assistant message.
type AssistantTurn struct {
	Messages []TMsg   `json:"messages"`
	Pending  []TBlock `json:"pending,omitempty"`
}

// TMsg is one model message.
type TMsg struct {
	Role   string   `json:"role"` // user | assistant
	Blocks []TBlock `json:"blocks"`
}

// TBlock is one content block: text | image | tool_use | tool_result.
type TBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	AttachmentID string          `json:"attachmentId,omitempty"` // image
	MediaType    string          `json:"mediaType,omitempty"`    // image
	ID           string          `json:"id,omitempty"`           // tool_use
	Name         string          `json:"name,omitempty"`         // tool_use
	Input        json.RawMessage `json:"input,omitempty"`        // tool_use
	ToolUseID    string          `json:"toolUseId,omitempty"`    // tool_result
	Content      string          `json:"content,omitempty"`      // tool_result
	IsError      bool            `json:"isError,omitempty"`      // tool_result
}
