package model

import "encoding/json"

// ---------------------------------------------------------------- identity (ADR-016, contracts §15)

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type MeProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type MeOrg struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Role     string      `json:"role"` // owner | admin | member | viewer
	Projects []MeProject `json:"projects"`
}

// Me is GET /me. User is null for api key callers (Auth = "apiKey").
type Me struct {
	User             *User   `json:"user"`
	Orgs             []MeOrg `json:"orgs"`
	CurrentProjectID string  `json:"currentProjectId"`
	// Role in the current project's org.
	Role string `json:"role"`
	Auth string `json:"auth"` // session | apiKey
	// AI is the current org's AI usage this month (contracts §19); absent when there is no current org.
	AI *MeAI `json:"ai,omitempty"`
}

type AuthConfig struct {
	OIDC     bool `json:"oidc"`
	DevLogin bool `json:"devLogin"`
}

type Member struct {
	UserID      string `json:"userId"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	LastLoginAt int64  `json:"lastLoginAt,omitempty"`
	// AICredits: the member's monthly cap (null = none) and this month's usage (contracts §19).
	AICredits AILimitUsed `json:"aiCredits"`
}

// MemberInput adds a member by email (an unknown email creates the user; it is linked on first login).
type MemberInput struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role"`
}

type MemberPatch struct {
	Role string `json:"role"`
}

type Project struct {
	ID        string `json:"id"`
	OrgID     string `json:"orgId"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Role      string `json:"role,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

type ProjectInput struct {
	OrgID string `json:"orgId,omitempty"` // default: the current project's org
	Name  string `json:"name"`
	Slug  string `json:"slug,omitempty"`
}

// APIKey never carries the key except in the POST /api-keys response (Key).
type APIKey struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Kind           string   `json:"kind"` // ingest | api
	Prefix         string   `json:"prefix"`
	AllowedOrigins []string `json:"allowedOrigins"`
	CreatedAt      int64    `json:"createdAt"`
	LastUsedAt     int64    `json:"lastUsedAt,omitempty"`
	RevokedAt      int64    `json:"revokedAt,omitempty"`
	CreatedBy      string   `json:"createdBy,omitempty"`
	CreatedByEmail string   `json:"createdByEmail,omitempty"`
	Key            string   `json:"key,omitempty"`
}

type APIKeyInput struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
}

type AuditEntry struct {
	ID         string          `json:"id"`
	At         int64           `json:"at"`
	Action     string          `json:"action"` // "POST /monitors/{id}", "login", ...
	Target     string          `json:"target"`
	ProjectID  string          `json:"projectId,omitempty"`
	UserID     string          `json:"userId,omitempty"`
	UserEmail  string          `json:"userEmail,omitempty"`
	APIKeyID   string          `json:"apiKeyId,omitempty"`
	APIKeyName string          `json:"apiKeyName,omitempty"`
	Meta       json.RawMessage `json:"meta"`
}

// ---------------------------------------------------------------- Slack / GitHub apps (ADR-017)

type SlackStatus struct {
	Installed   bool   `json:"installed"`
	ID          string `json:"id,omitempty"`
	TeamID      string `json:"teamId,omitempty"`
	TeamName    string `json:"teamName,omitempty"`
	InstalledAt int64  `json:"installedAt,omitempty"`
}

type SlackChannel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"isPrivate"`
	IsMember  bool   `json:"isMember"`
}

type GitHubInstall struct {
	ID             string `json:"id"` // integration id
	InstallationID string `json:"installationId"`
	Account        string `json:"account"`
	AccountType    string `json:"accountType"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"createdAt"`
}

type GitHubRepo struct {
	FullName      string `json:"fullName"`
	DefaultBranch string `json:"defaultBranch"`
	Private       bool   `json:"private"`
	Language      string `json:"language"`
	Added         bool   `json:"added"`
}
