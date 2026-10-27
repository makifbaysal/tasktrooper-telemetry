// Package model holds codeintel's shared types: repositories, inventory items and their enrichment.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Inventory kinds (inventory_items.kind).
const (
	KindEndpoint   = "endpoint"
	KindConsumer   = "consumer"
	KindCron       = "cron"
	KindView       = "view"
	KindDependency = "dependency"
)

// Criticality levels, most critical first (contracts §13).
const (
	Critical = "critical"
	High     = "high"
	Medium   = "medium"
	Low      = "low"
)

// CriticalityRank orders levels: critical=3 … low=0.
func CriticalityRank(c string) int {
	switch c {
	case Critical:
		return 3
	case High:
		return 2
	case Medium:
		return 1
	}
	return 0
}

// Repo is a repositories row joined with its project's org.
type Repo struct {
	ID                 int64
	ProjectID          int64
	OrgID              int64
	Provider           string // github | gitlab | bitbucket | local
	FullName           string
	DefaultBranch      string
	LastAnalyzedSHA    string
	Status             string
	InstallationID     int64
	CredentialsRef     string
	LocalPath          string
	WebhookSecretRef   string
	AnalyzeRequestedAt *time.Time
	AnalyzedAt         *time.Time
	AnalysisStartedAt  *time.Time
}

// Due reports whether the repo should be analyzed: never analyzed, or re-analysis requested after the last one.
func (r Repo) Due() bool {
	if r.AnalyzedAt == nil {
		return true
	}
	return r.AnalyzeRequestedAt != nil && r.AnalyzeRequestedAt.After(*r.AnalyzedAt)
}

// Item is one inventory entry extracted from code (or observed only, when Shadow).
type Item struct {
	ID        int64  `json:"id,omitempty"`
	Kind      string `json:"kind"`
	Key       string `json:"key"`              // endpoint "METHOD /route/:param", consumer process, cron job, view route, dependency "<kind> · <name>"
	Source    string `json:"source,omitempty"` // consumer topic/queue, cron schedule
	Service   string `json:"service"`
	File      string `json:"file,omitempty"` // repo-relative, slash separated
	Line      int    `json:"line,omitempty"`
	Framework string `json:"framework,omitempty"`
	Handler   string `json:"handler,omitempty"`
	Language  string `json:"language,omitempty"`

	// Snippet is the handler source (never persisted; LLM input and code hash).
	Snippet string `json:"-"`
	// Callees are signatures of functions the handler calls (LLM input).
	Callees []string `json:"-"`

	Criticality string     `json:"criticality"`
	Rationale   string     `json:"-"` // why the static rules chose Criticality
	Observed    bool       `json:"observed"`
	Monitored   bool       `json:"monitored"`
	Dead        bool       `json:"dead"`
	Shadow      bool       `json:"shadow"`
	CodeHash    string     `json:"codeHash,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	LLM         *LLMResult `json:"llm,omitempty"`
	Meta        map[string]any
	Stats       *Stats `json:"-"` // observed traffic, when matched
}

// Stats is observed traffic of an item over the matching window (7 days).
type Stats struct {
	Count     float64 // requests / processed messages
	Errors    float64
	P95       float64 // ms
	RPM       float64 // average per minute over the window
	ErrorRate float64
	Method    string // observed HTTP method (for ANY routes)
	Route     string // observed route as written by the writer
}

// Identity is the stable key of an item within a repository.
func (it Item) Identity() string { return it.Kind + "\x00" + it.Service + "\x00" + it.Key }

// ComputeHash sets CodeHash from key + snippet (+ file when there is no snippet).
func (it *Item) ComputeHash() {
	h := sha256.New()
	h.Write([]byte(it.Kind + "\x00" + it.Key + "\x00" + it.Source + "\x00"))
	if it.Snippet != "" {
		h.Write([]byte(it.Snippet))
	} else {
		h.Write([]byte(it.File))
	}
	it.CodeHash = hex.EncodeToString(h.Sum(nil))[:32]
}

// LLMResult is the structured enrichment of one item (inventory_items.llm).
type LLMResult struct {
	Criticality       string             `json:"criticality"`
	BusinessDomain    string             `json:"businessDomain,omitempty"`
	Summary           string             `json:"summary,omitempty"`
	FailureModes      []string           `json:"failureModes,omitempty"`
	SuggestedMonitors []SuggestedMonitor `json:"suggestedMonitors,omitempty"`
	LoadtestHint      *LoadtestHint      `json:"loadtestHint,omitempty"`
	Model             string             `json:"model,omitempty"`
}

// SuggestedMonitor is a template id the LLM recommends with its reason.
type SuggestedMonitor struct {
	Template string `json:"template"` // error_rate | latency | throughput_drop | error_new | consumer_lag | job_missed | dlq
	Reason   string `json:"reason"`
}

// LoadtestHint describes how to call an endpoint in a load test.
type LoadtestHint struct {
	PayloadExample json.RawMessage `json:"payloadExample,omitempty"` // JSON object example body
	Idempotent     bool            `json:"idempotent"`
	Notes          string          `json:"notes,omitempty"`
}
