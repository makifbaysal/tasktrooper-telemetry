package model

import (
	"github.com/tasktrooper/telemetry/services/api/internal/flame"
	"github.com/tasktrooper/telemetry/services/api/internal/llmprice"
)

// ---------------------------------------------------------------- session replay (ADR-021, contracts §23)

// ReplayUser is who the session belongs to (user id once identified, else the anonymous id).
type ReplayUser struct {
	ID          string `json:"id,omitempty"`
	AnonymousID string `json:"anonymousId"`
}

// ReplaySummary is one recorded session; user / device fields come from rum_events.
type ReplaySummary struct {
	SessionID  string     `json:"sessionId"`
	App        string     `json:"app"`
	Platform   string     `json:"platform"`
	Format     string     `json:"format"` // rrweb | wireframe
	StartedAt  int64      `json:"startedAt"`
	DurationMs float64    `json:"durationMs"`
	Segments   int        `json:"segments"`
	User       ReplayUser `json:"user"`
	Country    string     `json:"country,omitempty"`
	Device     string     `json:"device,omitempty"`
	Browser    string     `json:"browser,omitempty"`
	OS         string     `json:"os,omitempty"`
	AppVersion string     `json:"appVersion,omitempty"`
	FirstView  string     `json:"firstView"`
	Views      int        `json:"views"`
	Errors     int        `json:"errors"`
	Clicks     int        `json:"clicks"`
	RageClicks int        `json:"rageClicks"`
}

// ReplayList is GET /replays.
type ReplayList struct {
	Replays []ReplaySummary `json:"replays"`
}

// ReplaySegment is one uploaded segment (body: GET /replays/{sessionId}/segments/{segment}).
type ReplaySegment struct {
	Segment         int   `json:"segment"`
	StartTs         int64 `json:"startTs"`
	EndTs           int64 `json:"endTs"`
	Events          int   `json:"events"`
	Bytes           int   `json:"bytes"`
	HasFullSnapshot bool  `json:"hasFullSnapshot"`
}

// TimelineItem is one RUM event of the session, for the player's timeline.
type TimelineItem struct {
	Ts          int64    `json:"ts"`
	Kind        string   `json:"kind"` // view | action | error | resource | vital
	Label       string   `json:"label"`
	Detail      string   `json:"detail,omitempty"`
	ViewName    string   `json:"viewName,omitempty"`
	TraceID     string   `json:"traceId,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Status      int      `json:"status,omitempty"`
	DurationMs  *float64 `json:"durationMs,omitempty"`
}

// ReplayDetail is GET /replays/{sessionId}.
type ReplayDetail struct {
	Summary  ReplaySummary   `json:"summary"`
	Segments []ReplaySegment `json:"segments"`
	Timeline []TimelineItem  `json:"timeline"`
}

// ReplayRef points an error sample at the replay that recorded it.
type ReplayRef struct {
	SessionID string `json:"sessionId"`
	OffsetMs  int64  `json:"offsetMs"` // ms from the replay's first segment start
}

// ---------------------------------------------------------------- continuous profiling (ADR-022, contracts §24)

// ProfileType is one profile type that has data for a service.
type ProfileType struct {
	Type     string   `json:"type"`
	Unit     string   `json:"unit"`
	Runtime  string   `json:"runtime"`
	Hosts    []string `json:"hosts"`
	Versions []string `json:"versions"`
	Routes   int      `json:"routes"` // number of route-tagged (route, method) pairs
}

// ProfileTypes is GET /profiles/types.
type ProfileTypes struct {
	Types []ProfileType `json:"types"`
}

// Flamegraph is GET /profiles/flamegraph.
type Flamegraph struct {
	Type     string      `json:"type"`
	Unit     string      `json:"unit"`
	Total    int64       `json:"total"`
	Profiles int         `json:"profiles"`
	From     int64       `json:"from"`
	To       int64       `json:"to"`
	Root     *flame.Node `json:"root"`
}

// FlameDiff is GET /profiles/diff.
type FlameDiff struct {
	Type      string          `json:"type"`
	Unit      string          `json:"unit"`
	Total     int64           `json:"total"`
	BaseTotal int64           `json:"baseTotal"`
	Root      *flame.DiffNode `json:"root"`
}

// TopFunctions is GET /profiles/top.
type TopFunctions struct {
	Unit      string     `json:"unit"`
	Total     int64      `json:"total"`
	Functions []flame.Fn `json:"functions"`
}

// ---------------------------------------------------------------- AI / LLM monitoring (ADR-023, contracts §25)

// AITotals are the headline numbers. CostUsd sums the priced models (null when no model has a price);
// CostPartial = some calls have no price.
type AITotals struct {
	Calls           float64  `json:"calls"`
	Errors          float64  `json:"errors"`
	ErrorRate       float64  `json:"errorRate"`
	InputTokens     float64  `json:"inputTokens"`
	OutputTokens    float64  `json:"outputTokens"`
	CacheReadTokens float64  `json:"cacheReadTokens"`
	CostUsd         *float64 `json:"costUsd"`
	CostPartial     bool     `json:"costPartial"`
	P50Ms           float64  `json:"p50Ms"`
	P95Ms           float64  `json:"p95Ms"`
	TtftP95Ms       *float64 `json:"ttftP95Ms,omitempty"`
}

// AISeries are the overview charts (values per bucket).
type AISeries struct {
	Calls   SeriesSet `json:"calls"`   // calls, errors
	Tokens  SeriesSet `json:"tokens"`  // input, output
	Cost    SeriesSet `json:"cost"`    // USD per model (≤ 8 + other)
	Latency SeriesSet `json:"latency"` // p95 (ms)
}

type AIModelRow struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Calls        float64  `json:"calls"`
	Errors       float64  `json:"errors"`
	InputTokens  float64  `json:"inputTokens"`
	OutputTokens float64  `json:"outputTokens"`
	CostUsd      *float64 `json:"costUsd"`
	P95Ms        float64  `json:"p95Ms"`
	PriceKnown   bool     `json:"priceKnown"`
}

type AIServiceRow struct {
	Service      string   `json:"service"`
	Calls        float64  `json:"calls"`
	InputTokens  float64  `json:"inputTokens"`
	OutputTokens float64  `json:"outputTokens"`
	CostUsd      *float64 `json:"costUsd"`
	Models       []string `json:"models"`
}

type AIEndpointRow struct {
	Service         string   `json:"service"`
	Route           string   `json:"route"`
	Method          string   `json:"method"`
	Calls           float64  `json:"calls"`
	AvgInputTokens  float64  `json:"avgInputTokens"`
	AvgOutputTokens float64  `json:"avgOutputTokens"`
	CostUsd         *float64 `json:"costUsd"`
	CostPerCallUsd  *float64 `json:"costPerCallUsd"`
	P95Ms           float64  `json:"p95Ms"`
}

// AIOverview is GET /ai/overview.
type AIOverview struct {
	Totals     AITotals        `json:"totals"`
	Series     AISeries        `json:"series"`
	ByModel    []AIModelRow    `json:"byModel"`
	ByService  []AIServiceRow  `json:"byService"`
	ByEndpoint []AIEndpointRow `json:"byEndpoint"`
}

// AICall is one LLM span.
type AICall struct {
	Ts           int64    `json:"ts"`
	TraceID      string   `json:"traceId"`
	SpanID       string   `json:"spanId"`
	Service      string   `json:"service"`
	Route        string   `json:"route"`
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Operation    string   `json:"operation"`
	InputTokens  float64  `json:"inputTokens"`
	OutputTokens float64  `json:"outputTokens"`
	CostUsd      *float64 `json:"costUsd"`
	DurationMs   float64  `json:"durationMs"`
	TtftMs       *float64 `json:"ttftMs,omitempty"`
	Error        string   `json:"error,omitempty"`
	FinishReason string   `json:"finishReason,omitempty"`
	HasContent   bool     `json:"hasContent"`
}

// AICalls is GET /ai/calls.
type AICalls struct {
	Calls []AICall `json:"calls"`
}

// AIContent is the captured prompt / completion (JSON strings: GenAI message arrays).
type AIContent struct {
	System string `json:"system,omitempty"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

// AICallDetail is GET /ai/calls/{traceId}/{spanId}.
type AICallDetail struct {
	Call       AICall            `json:"call"`
	Attributes map[string]string `json:"attributes"`
	Content    *AIContent        `json:"content,omitempty"`
}

// AIPrices is GET /ai/prices.
type AIPrices struct {
	Prices  []llmprice.Price `json:"prices"`
	Updated string           `json:"updated"` // date the default table was reviewed
}

// AIPriceInput is PUT /ai/prices.
type AIPriceInput struct {
	Provider          string   `json:"provider"`
	Model             string   `json:"model"`
	InputPerMTok      float64  `json:"inputPerMTok"`
	OutputPerMTok     float64  `json:"outputPerMTok"`
	CacheReadPerMTok  *float64 `json:"cacheReadPerMTok,omitempty"`
	CacheWritePerMTok *float64 `json:"cacheWritePerMTok,omitempty"`
}

// SpanLLM is the GenAI summary of an LLM span in GET /traces/{id}.
type SpanLLM struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Operation    string   `json:"operation"`
	InputTokens  float64  `json:"inputTokens"`
	OutputTokens float64  `json:"outputTokens"`
	CostUsd      *float64 `json:"costUsd"`
	TtftMs       *float64 `json:"ttftMs,omitempty"`
	HasContent   bool     `json:"hasContent"`
}

// ServiceDetail is GET /services/{id}: the service + whether it makes LLM calls.
type ServiceDetail struct {
	*Service
	HasAi bool `json:"hasAi"`
}
