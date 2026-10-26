// Package neon discovers Neon compute endpoints (`databases` rows, engine postgres) and maps what the Neon
// API exposes to trooper metrics (contracts §10). Neon has no public CPU/memory/connection API: only
// trooper.db.disk (logical size / storage limit) lands in db_1m, the rest go to the custom metrics table.
package neon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/httpx"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// DefaultBase is the Neon API v2.
const DefaultBase = "https://console.neon.tech/api/v2"

// Config is integrations.config for provider neon.
type Config struct {
	OrgID      string   `json:"orgId"`
	ProjectIDs []string `json:"projectIds"` // empty = all
	// StorageLimitGB is the plan's storage limit per branch, used for trooper.db.disk when the project has no quota.
	StorageLimitGB float64 `json:"storageLimitGb"`
	Env            string  `json:"env"`
}

// Provider implements model.Provider.
type Provider struct {
	cfg Config
	c   *httpx.Client
}

// New builds the provider. apiKey is the resolved credential; base "" = DefaultBase.
func New(raw json.RawMessage, apiKey, base string) (*Provider, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("neon config: %w", err)
		}
	}
	if apiKey == "" {
		return nil, errors.New("neon: API key missing (credentials_ref)")
	}
	if base == "" {
		base = DefaultBase
	}
	return &Provider{cfg: cfg, c: &httpx.Client{Base: strings.TrimRight(base, "/"), Auth: httpx.Bearer(strings.TrimSpace(apiKey))}}, nil
}

// API shapes (subset).
type project struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	RegionID             string `json:"region_id"`
	PgVersion            int    `json:"pg_version"`
	ActiveTimeSeconds    int64  `json:"active_time_seconds"`
	ComputeTimeSeconds   int64  `json:"compute_time_seconds"`
	CPUUsedSec           int64  `json:"cpu_used_sec"`
	WrittenDataBytes     int64  `json:"written_data_bytes"`
	DataTransferBytes    int64  `json:"data_transfer_bytes"`
	SyntheticStorageSize int64  `json:"synthetic_storage_size"`
	Settings             struct {
		Quota struct {
			LogicalSizeBytes int64 `json:"logical_size_bytes"`
		} `json:"quota"`
	} `json:"settings"`
}

type branch struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Default      bool   `json:"default"`
	Primary      bool   `json:"primary"`
	LogicalSize  int64  `json:"logical_size"`
	CurrentState string `json:"current_state"`
}

type endpoint struct {
	ID       string  `json:"id"`
	Host     string  `json:"host"`
	BranchID string  `json:"branch_id"`
	Type     string  `json:"type"` // read_write | read_only
	State    string  `json:"current_state"`
	MinCU    float64 `json:"autoscaling_limit_min_cu"`
	MaxCU    float64 `json:"autoscaling_limit_max_cu"`
	RegionID string  `json:"region_id"`
	Disabled bool    `json:"disabled"`
}

type projectState struct {
	project   project
	branches  map[string]branch
	endpoints []endpoint
}

type state struct{ projects []projectState }

func (p *Provider) listProjects(ctx context.Context) ([]project, error) {
	if len(p.cfg.ProjectIDs) > 0 {
		var out []project
		var errs error
		for _, id := range p.cfg.ProjectIDs {
			var r struct {
				Project project `json:"project"`
			}
			if err := p.c.Get(ctx, "/projects/"+url.PathEscape(id), nil, &r); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			out = append(out, r.Project)
		}
		return out, errs
	}
	var out []project
	cursor := ""
	for page := 0; page < 50; page++ {
		q := url.Values{"limit": {"400"}}
		if p.cfg.OrgID != "" {
			q.Set("org_id", p.cfg.OrgID)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var r struct {
			Projects   []project `json:"projects"`
			Pagination struct {
				Cursor string `json:"cursor"`
			} `json:"pagination"`
		}
		if err := p.c.Get(ctx, "/projects", q, &r); err != nil {
			return out, err
		}
		out = append(out, r.Projects...)
		if len(r.Projects) < 400 || r.Pagination.Cursor == "" || r.Pagination.Cursor == cursor {
			break
		}
		cursor = r.Pagination.Cursor
	}
	return out, nil
}

func (p *Provider) loadProject(ctx context.Context, pr project) (projectState, error) {
	ps := projectState{project: pr, branches: map[string]branch{}}
	var br struct {
		Branches []branch `json:"branches"`
	}
	if err := p.c.Get(ctx, "/projects/"+url.PathEscape(pr.ID)+"/branches", nil, &br); err != nil {
		return ps, err
	}
	for _, b := range br.Branches {
		ps.branches[b.ID] = b
	}
	var ep struct {
		Endpoints []endpoint `json:"endpoints"`
	}
	if err := p.c.Get(ctx, "/projects/"+url.PathEscape(pr.ID)+"/endpoints", nil, &ep); err != nil {
		return ps, err
	}
	ps.endpoints = ep.Endpoints
	return ps, nil
}

func (p *Provider) load(ctx context.Context, projects []project) (*state, error) {
	st := &state{}
	var errs error
	for _, pr := range projects {
		ps, err := p.loadProject(ctx, pr)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("project %s: %w", pr.ID, err))
			continue
		}
		st.projects = append(st.projects, ps)
	}
	return st, errs
}

// Discover implements model.Provider.
func (p *Provider) Discover(ctx context.Context) (*model.Inventory, error) {
	projects, err := p.listProjects(ctx)
	if err != nil && len(projects) == 0 {
		return nil, err
	}
	st, lerr := p.load(ctx, projects)
	inv := &model.Inventory{State: st}
	for _, ps := range st.projects {
		for _, e := range ps.endpoints {
			inv.Databases = append(inv.Databases, p.database(ps, e))
		}
	}
	return inv, errors.Join(err, lerr)
}

// DBName is the databases.name of an endpoint: "<project>-<branch>" (+ "-ro-<endpoint>" for read replicas).
func DBName(ps projectState, e endpoint) string {
	b := ps.branches[e.BranchID]
	name := ps.project.Name + "-" + nonEmpty(b.Name, e.BranchID)
	if e.Type == "read_only" {
		name += "-ro-" + e.ID
	}
	return name
}

func (p *Provider) database(ps projectState, e endpoint) model.Database {
	b := ps.branches[e.BranchID]
	region := nonEmpty(e.RegionID, ps.project.RegionID)
	d := model.Database{
		Name:            DBName(ps, e),
		Engine:          "postgres",
		Host:            e.Host,
		Port:            5432,
		Source:          "Neon · " + region,
		Zone:            region,
		InstanceClass:   cuRange(e.MinCU, e.MaxCU),
		CloudResourceID: e.ID,
		Env:             p.cfg.Env,
		Meta: map[string]any{
			"project": ps.project.Name, "projectId": ps.project.ID, "branch": b.Name, "branchId": e.BranchID,
			"state": e.State, "computeId": e.ID, "type": e.Type,
		},
	}
	if ps.project.PgVersion > 0 {
		d.Version = "PostgreSQL " + strconv.Itoa(ps.project.PgVersion)
	}
	if b.LogicalSize > 0 {
		d.DiskGB = model.F(float64(b.LogicalSize) / (1 << 30))
	}
	return d
}

func cuRange(lo, hi float64) string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	if lo == 0 && hi == 0 {
		return ""
	}
	if lo == hi {
		return f(lo) + " CU"
	}
	return f(lo) + "–" + f(hi) + " CU"
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Collect implements model.Provider: re-reads projects/branches/endpoints (state, size, consumption).
func (p *Provider) Collect(ctx context.Context, inv *model.Inventory, now time.Time) (*model.Result, error) {
	old, _ := inv.State.(*state)
	var projects []project
	var errs error
	if old != nil {
		for _, ps := range old.projects {
			var r struct {
				Project project `json:"project"`
			}
			if err := p.c.Get(ctx, "/projects/"+url.PathEscape(ps.project.ID), nil, &r); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			projects = append(projects, r.Project)
		}
	}
	st, err := p.load(ctx, projects)
	return MapMetrics(st, p.cfg.StorageLimitGB, now), errors.Join(errs, err)
}

// MapMetrics turns the Neon state into samples (exported for tests).
func MapMetrics(st *state, storageLimitGB float64, now time.Time) *model.Result {
	ts := now.Truncate(time.Minute)
	res := &model.Result{DBMeta: map[string]map[string]any{}}
	add := func(name string, v float64, region string, attrs map[string]string) {
		res.Samples = append(res.Samples, model.Sample{Name: name, Value: v, TS: ts, Region: region, Attrs: attrs})
	}
	for _, ps := range st.projects {
		pr := ps.project
		pa := map[string]string{"trooper.neon.project": pr.Name, "trooper.neon.project_id": pr.ID}
		add("trooper.cloud.neon.compute_time_seconds", float64(pr.ComputeTimeSeconds), pr.RegionID, pa)
		add("trooper.cloud.neon.active_time_seconds", float64(pr.ActiveTimeSeconds), pr.RegionID, pa)
		add("trooper.cloud.neon.cpu_used_seconds", float64(pr.CPUUsedSec), pr.RegionID, pa)
		add("trooper.cloud.neon.written_data_bytes", float64(pr.WrittenDataBytes), pr.RegionID, pa)
		add("trooper.cloud.neon.data_transfer_bytes", float64(pr.DataTransferBytes), pr.RegionID, pa)
		add("trooper.cloud.neon.synthetic_storage_bytes", float64(pr.SyntheticStorageSize), pr.RegionID, pa)
		limit := float64(pr.Settings.Quota.LogicalSizeBytes)
		if limit <= 0 && storageLimitGB > 0 {
			limit = storageLimitGB * (1 << 30)
		}
		for _, e := range ps.endpoints {
			name := DBName(ps, e)
			slug := model.Slug("neon", name)
			role := "primary"
			if e.Type == "read_only" {
				role = "replica"
			}
			region := nonEmpty(e.RegionID, pr.RegionID)
			attrs := model.DBAttrs(slug, name, "postgresql", e.ID, role)
			b := ps.branches[e.BranchID]
			if limit > 0 && b.LogicalSize > 0 {
				add(model.DBDisk, float64(b.LogicalSize)/limit, region, attrs)
			}
			active := 0.0
			if e.State == "active" {
				active = 1
			}
			add("trooper.cloud.neon.compute_active", active, region, attrs)
			add("trooper.cloud.neon.compute_cu_max", e.MaxCU, region, attrs)
			if b.LogicalSize > 0 {
				add("trooper.cloud.neon.logical_size_bytes", float64(b.LogicalSize), region, attrs)
			}
			res.DBMeta[slug] = map[string]any{"state": e.State}
		}
	}
	return res
}
