// Package vercel discovers Vercel projects and turns deployments into deploy_markers (contracts §10).
// Log / trace / speed-insights / web-analytics drains are received by ingest-gw (POST /v1/drains/vercel);
// this package can optionally create the drain (config.createDrain).
package vercel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/httpx"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// DefaultBase is the Vercel REST API.
const DefaultBase = "https://api.vercel.com"

// Config is integrations.config for provider vercel.
type Config struct {
	TeamID         string            `json:"teamId"`
	ProjectMap     map[string]string `json:"projectMap"` // vercel project name → service name
	DrainSecretRef string            `json:"drainSecretRef"`
	CreateDrain    bool              `json:"createDrain"`
	BackfillHours  int               `json:"backfillHours"` // first sync window (default 24)
}

// Options carry runtime settings the factory resolves.
type Options struct {
	Base          string
	IntegrationID int64
	// PublicIngestURL is TT_PUBLIC_INGEST_URL (e.g. https://ingest.tasktrooper.dev); used only with createDrain.
	PublicIngestURL string
	IngestKey       string
	DrainSecret     string
}

// Provider implements model.Provider.
type Provider struct {
	cfg Config
	o   Options
	c   *httpx.Client

	mu        sync.Mutex
	since     time.Time // deployments watermark (created time)
	drainDone bool
}

// New builds the provider; token is the resolved access token.
func New(raw json.RawMessage, token string, o Options) (*Provider, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("vercel config: %w", err)
		}
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("vercel: access token missing (credentials_ref)")
	}
	if cfg.BackfillHours <= 0 {
		cfg.BackfillHours = 24
	}
	if o.Base == "" {
		o.Base = DefaultBase
	}
	return &Provider{cfg: cfg, o: o, c: &httpx.Client{Base: strings.TrimRight(o.Base, "/"), Auth: httpx.Bearer(strings.TrimSpace(token))}}, nil
}

func (p *Provider) query() url.Values {
	q := url.Values{}
	if p.cfg.TeamID != "" {
		q.Set("teamId", p.cfg.TeamID)
	}
	return q
}

type project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Framework string `json:"framework"`
}

type state struct{ projects map[string]project } // by id

// Discover lists projects (counted in integrations.resources; nothing is written to databases/brokers).
func (p *Provider) Discover(ctx context.Context) (*model.Inventory, error) {
	st := &state{projects: map[string]project{}}
	q := p.query()
	q.Set("limit", "100")
	for page := 0; page < 100; page++ {
		var r struct {
			Projects   []project `json:"projects"`
			Pagination struct {
				Next *int64 `json:"next"`
			} `json:"pagination"`
		}
		if err := p.c.Get(ctx, "/v10/projects", q, &r); err != nil {
			return nil, err
		}
		for _, pr := range r.Projects {
			st.projects[pr.ID] = pr
		}
		if r.Pagination.Next == nil || len(r.Projects) == 0 {
			break
		}
		q.Set("from", strconv.FormatInt(*r.Pagination.Next, 10))
	}
	inv := &model.Inventory{Other: len(st.projects), State: st}
	var err error
	if p.cfg.CreateDrain {
		err = p.ensureDrain(ctx)
	}
	return inv, err
}

// DrainURL is the ingest-gw endpoint Vercel drains post to (contracts §10).
func DrainURL(publicIngest, key string, integrationID int64) string {
	return strings.TrimRight(publicIngest, "/") + "/v1/drains/vercel?key=" + url.QueryEscape(key) + "&integration=" + strconv.FormatInt(integrationID, 10)
}

func (p *Provider) ensureDrain(ctx context.Context) error {
	p.mu.Lock()
	done := p.drainDone
	p.mu.Unlock()
	if done {
		return nil
	}
	if p.o.PublicIngestURL == "" || p.o.IngestKey == "" {
		return errors.New("createDrain: TT_PUBLIC_INGEST_URL and an ingest key for the project are required")
	}
	marker := "/v1/drains/vercel?"
	var list struct {
		Drains []struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			Delivery struct {
				Endpoint string `json:"endpoint"`
			} `json:"delivery"`
		} `json:"drains"`
	}
	if err := p.c.Get(ctx, "/v1/drains", p.query(), &list); err != nil {
		return fmt.Errorf("list drains: %w", err)
	}
	want := "integration=" + strconv.FormatInt(p.o.IntegrationID, 10)
	for _, d := range list.Drains {
		u := d.Delivery.Endpoint + d.URL
		if strings.Contains(u, marker) && strings.Contains(u, want) {
			p.mu.Lock()
			p.drainDone = true
			p.mu.Unlock()
			return nil
		}
	}
	body := map[string]any{
		"name":     "tasktrooper",
		"projects": "all",
		"schemas": map[string]any{
			"log": map[string]string{"version": "v1"}, "trace": map[string]string{"version": "v1"},
			"speed_insights": map[string]string{"version": "v1"}, "analytics": map[string]string{"version": "v1"},
		},
		"delivery": map[string]any{
			"type": "http", "endpoint": DrainURL(p.o.PublicIngestURL, p.o.IngestKey, p.o.IntegrationID),
			"encoding": "json", "headers": map[string]string{},
		},
	}
	if p.o.DrainSecret != "" {
		body["secret"] = p.o.DrainSecret
	}
	if err := p.c.Do(ctx, "POST", "/v1/drains", p.query(), body, nil); err != nil {
		return fmt.Errorf("create drain: %w", err)
	}
	p.mu.Lock()
	p.drainDone = true
	p.mu.Unlock()
	return nil
}

// Deployment is the subset of /v6/deployments items we read.
type Deployment struct {
	UID        string            `json:"uid"`
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Created    int64             `json:"created"`
	CreatedAt  int64             `json:"createdAt"`
	State      string            `json:"state"`
	ReadyState string            `json:"readyState"`
	Target     *string           `json:"target"`
	ProjectID  string            `json:"projectId"`
	Meta       map[string]string `json:"meta"`
	Creator    struct {
		Username string `json:"username"`
	} `json:"creator"`
}

// Collect lists deployments created since the watermark (with 1 h overlap so state changes of recent
// deploys are picked up) and returns them as deploy markers.
func (p *Provider) Collect(ctx context.Context, inv *model.Inventory, now time.Time) (*model.Result, error) {
	st, _ := inv.State.(*state)
	p.mu.Lock()
	since := p.since
	p.mu.Unlock()
	if since.IsZero() {
		since = now.Add(-time.Duration(p.cfg.BackfillHours) * time.Hour)
	}
	q := p.query()
	q.Set("limit", "100")
	q.Set("since", strconv.FormatInt(since.UnixMilli(), 10))
	res := &model.Result{}
	newest := since
	for page := 0; page < 50; page++ {
		var r struct {
			Deployments []Deployment `json:"deployments"`
			Pagination  struct {
				Next *int64 `json:"next"`
			} `json:"pagination"`
		}
		if err := p.c.Get(ctx, "/v6/deployments", q, &r); err != nil {
			return res, err
		}
		for _, d := range r.Deployments {
			m := MapDeployment(d, p.cfg.ProjectMap, st)
			res.Deploys = append(res.Deploys, m)
			if m.TS.After(newest) {
				newest = m.TS
			}
		}
		if r.Pagination.Next == nil || len(r.Deployments) == 0 {
			break
		}
		q.Set("until", strconv.FormatInt(*r.Pagination.Next, 10))
	}
	// Next window: 1 h before the newest seen deploy (or now) so BUILDING deploys get their final state.
	wm := now.Add(-time.Hour)
	if newest.Before(wm) {
		wm = newest
	}
	if wm.Before(since) {
		wm = since
	}
	p.mu.Lock()
	p.since = wm
	p.mu.Unlock()
	return res, nil
}

// MapDeployment converts a Vercel deployment into a deploy marker.
func MapDeployment(d Deployment, projectMap map[string]string, st *state) model.DeployMarker {
	project := d.Name
	if st != nil {
		if pr, ok := st.projects[d.ProjectID]; ok && pr.Name != "" {
			project = pr.Name
		}
	}
	service := project
	if s := projectMap[project]; s != "" {
		service = s
	}
	env := "preview"
	if d.Target != nil && *d.Target != "" {
		env = *d.Target
	}
	created := d.Created
	if created == 0 {
		created = d.CreatedAt
	}
	m := d.Meta
	version := first(m["githubCommitSha"], m["gitlabCommitSha"], m["bitbucketCommitSha"], d.UID)
	u := d.URL
	if u != "" && !strings.HasPrefix(u, "http") {
		u = "https://" + u
	}
	return model.DeployMarker{
		Service:       service,
		Env:           env,
		Version:       version,
		Source:        "vercel",
		URL:           u,
		CommitMessage: first(m["githubCommitMessage"], m["gitlabCommitMessage"], m["bitbucketCommitMessage"]),
		Author:        first(m["githubCommitAuthorLogin"], m["githubCommitAuthorName"], m["gitlabCommitAuthorName"], m["bitbucketCommitAuthorName"], d.Creator.Username),
		State:         mapState(first(d.ReadyState, d.State)),
		TS:            time.UnixMilli(created).UTC(),
		ExternalID:    d.UID,
	}
}

func mapState(s string) string {
	switch strings.ToUpper(s) {
	case "READY":
		return "ready"
	case "ERROR":
		return "error"
	case "CANCELED":
		return "canceled"
	}
	return "building"
}

func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
