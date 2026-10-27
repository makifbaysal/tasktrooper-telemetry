// Package llmprice estimates the USD cost of LLM calls (ADR-023, contracts §25). The default table
// (defaults.json, per 1M tokens) is an estimate of public list prices; orgs override rows in Postgres llm_prices.
//
// Matching: the provider (gen_ai.provider.name) is mapped to a family (azure.ai.openai → openai, gcp.* → google,
// aws.bedrock → the model's vendor) and the model is normalized (lower case, "vendor/" and Bedrock
// "<region>.<vendor>." prefixes dropped). A row matches on the exact model, otherwise on the longest row model that
// is a prefix of it ending at a non-alphanumeric boundary ("claude-sonnet-4-5-20250929" → "claude-sonnet-4-5",
// but "gpt-4o" never matches the "gpt-4" row). Org rows win over default rows.
package llmprice

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
)

//go:embed defaults.json
var defaultsJSON []byte

// Price is one table row; prices are USD per 1M tokens.
type Price struct {
	Provider          string   `json:"provider"`
	Model             string   `json:"model"`
	InputPerMTok      float64  `json:"inputPerMTok"`
	OutputPerMTok     float64  `json:"outputPerMTok"`
	CacheReadPerMTok  *float64 `json:"cacheReadPerMTok,omitempty"`
	CacheWritePerMTok *float64 `json:"cacheWritePerMTok,omitempty"`
	Source            string   `json:"source"` // default | org
}

// Usage is the token usage of one call or an aggregate of calls of one model.
type Usage struct {
	Input, Output, CacheRead, CacheWrite float64
}

type file struct {
	Updated string `json:"updated"`
	Prices  []struct {
		Provider   string   `json:"provider"`
		Model      string   `json:"model"`
		Input      float64  `json:"input"`
		Output     float64  `json:"output"`
		CacheRead  *float64 `json:"cacheRead"`
		CacheWrite *float64 `json:"cacheWrite"`
	} `json:"prices"`
}

var (
	defaults []Price
	updated  string
)

func init() {
	var f file
	if err := json.Unmarshal(defaultsJSON, &f); err != nil {
		panic("llmprice: defaults.json: " + err.Error())
	}
	updated = f.Updated
	for _, p := range f.Prices {
		defaults = append(defaults, Price{Provider: p.Provider, Model: strings.ToLower(p.Model), InputPerMTok: p.Input, OutputPerMTok: p.Output,
			CacheReadPerMTok: p.CacheRead, CacheWritePerMTok: p.CacheWrite, Source: "default"})
	}
}

// Updated is the date (YYYY-MM-DD) the default table was last reviewed.
func Updated() string { return updated }

// Defaults returns a copy of the default table.
func Defaults() []Price { return append([]Price(nil), defaults...) }

// Cost is the USD cost of u. Input tokens include cache reads and writes (OTel GenAI semconv, contracts §25):
// uncached input = input − cache read − cache write, clamped at 0.
// A missing cache price falls back to the input price.
func (p Price) Cost(u Usage) float64 {
	uncached := max(0, u.Input-u.CacheRead-u.CacheWrite)
	cr, cw := p.InputPerMTok, p.InputPerMTok
	if p.CacheReadPerMTok != nil {
		cr = *p.CacheReadPerMTok
	}
	if p.CacheWritePerMTok != nil {
		cw = *p.CacheWritePerMTok
	}
	return (uncached*p.InputPerMTok + u.Output*p.OutputPerMTok + u.CacheRead*cr + u.CacheWrite*cw) / 1e6
}

// Table is the default table plus one org's overrides.
type Table struct {
	org []Price
}

// New returns a table with the org rows (Source is set to "org").
func New(org []Price) *Table {
	t := &Table{}
	for _, p := range org {
		p.Source = "org"
		p.Model = strings.ToLower(strings.TrimSpace(p.Model))
		p.Provider = strings.TrimSpace(p.Provider)
		t.org = append(t.org, p)
	}
	return t
}

// List returns every row: org rows, then the default rows they do not replace (same provider + model).
func (t *Table) List() []Price {
	out := append([]Price(nil), t.org...)
	seen := map[string]bool{}
	for _, p := range t.org {
		seen[p.Provider+"\x00"+p.Model] = true
	}
	for _, p := range defaults {
		if !seen[p.Provider+"\x00"+p.Model] {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == "org"
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// Lookup finds the price of provider + model.
func (t *Table) Lookup(provider, model string) (Price, bool) {
	fam, m := Normalize(provider, model)
	if m == "" {
		return Price{}, false
	}
	raw := strings.ToLower(strings.TrimSpace(model))
	// org rows first so they win exact matches and equal-length prefixes; a longer default prefix still wins
	// ("gpt-4o-mini-…" → default gpt-4o-mini over an org gpt-4o row)
	var rows []Price
	if t != nil {
		for _, p := range t.org {
			if p.Provider == provider || p.Provider == fam {
				rows = append(rows, p)
			}
		}
	}
	for _, p := range defaults {
		if p.Provider == fam {
			rows = append(rows, p)
		}
	}
	if p, ok := match(rows, m, raw); ok {
		return p, true
	}
	// unknown provider (gateways, self-hosted proxies): model names are distinctive across vendors
	return match(defaults, m, raw)
}

// Cost is Lookup + Price.Cost; ok = false when the price is unknown.
func (t *Table) Cost(provider, model string, u Usage) (float64, bool) {
	p, ok := t.Lookup(provider, model)
	if !ok {
		return 0, false
	}
	return p.Cost(u), true
}

func match(rows []Price, m, raw string) (Price, bool) {
	for _, p := range rows {
		if p.Model != "" && (p.Model == m || p.Model == raw) {
			return p, true
		}
	}
	best, bestLen := -1, 0
	for i, p := range rows {
		if p.Model == "" {
			continue
		}
		if len(p.Model) > bestLen && prefixAt(m, p.Model) {
			best, bestLen = i, len(p.Model)
		}
	}
	if best < 0 {
		return Price{}, false
	}
	return rows[best], true
}

// prefixAt reports whether pre is a prefix of s that ends at a non-alphanumeric boundary.
func prefixAt(s, pre string) bool {
	if !strings.HasPrefix(s, pre) || len(s) == len(pre) {
		return false
	}
	c := s[len(pre)]
	return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9')
}

var families = map[string]string{
	"openai": "openai", "azure.ai.openai": "openai", "azure_openai": "openai", "az.ai.openai": "openai",
	"anthropic":  "anthropic",
	"gcp.gemini": "google", "gcp.vertex_ai": "google", "gcp.gen_ai": "google", "google": "google", "vertex_ai": "google", "gemini": "google",
	"mistral_ai": "mistral_ai", "mistral": "mistral_ai", "mistralai": "mistral_ai",
	"cohere": "cohere", "deepseek": "deepseek", "x_ai": "x_ai", "xai": "x_ai", "groq": "groq",
}

var bedrockVendors = map[string]string{
	"anthropic": "anthropic", "amazon": "amazon", "meta": "meta", "mistral": "mistral_ai", "cohere": "cohere",
	"deepseek": "deepseek", "openai": "openai",
}

// Normalize maps a gen_ai.provider.name to a price family and the model to its lookup form.
func Normalize(provider, model string) (family, m string) {
	p := strings.ToLower(strings.TrimSpace(provider))
	m = strings.ToLower(strings.TrimSpace(model))
	family = families[p]
	if family == "" {
		family = p
	}
	// "models/gemini-1.5-pro", "openai/gpt-4o" (gateways)
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		if v := m[:i]; family == p {
			if f, ok := families[v[strings.LastIndexByte(v, '/')+1:]]; ok {
				family = f
			}
		}
		m = m[i+1:]
	}
	// Bedrock ids: [<region>.]<vendor>.<model>[-v1:0], e.g. us.anthropic.claude-sonnet-4-20250514-v1:0
	if parts := strings.Split(m, "."); len(parts) >= 2 {
		for i := 0; i < len(parts)-1 && i < 2; i++ {
			if f, ok := bedrockVendors[parts[i]]; ok {
				if p == "aws.bedrock" || family == p {
					family = f
				}
				m = strings.Join(parts[i+1:], ".")
				break
			}
		}
	}
	return family, m
}
