package azure

// Per-function metrics from Application Insights (contracts §22). The App Insights component of a Function App is
// found through ARM (Microsoft.Insights/components, hidden-link:<siteId> tag; Reader is enough) and queried through
// the ARM proxy (POST {componentId}/query) with the same ARM token. Every function of a connected app becomes its
// own serverless_functions row (slug azure-<app>-<function>, meta.parent = app slug); the app row stays and keeps
// the app-level Azure Monitor metrics.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/httpx"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

const (
	apiComponents = "2020-02-02"
	apiAIQuery    = "2018-04-20"

	// QueryInterval is the minimum gap between two App Insights queries of one component (ARM throttling: at most
	// one query per component per metrics tick; a little under a minute so a jittered 60 s tick never skips).
	QueryInterval = 50 * time.Second
	// Lookback of every query: App Insights data arrives late; faas_1m keeps the max per minute, so resending the
	// last 15 minutes every minute is idempotent.
	Lookback = 15 * time.Minute

	// Settings read with config.readAppSettings (nothing else is ever stored).
	SettingService     = "TASKTROOPER_SERVICE"
	SettingOTelService = "OTEL_SERVICE_NAME"
)

// KQL is the per-function query (contracts §22). requests.duration is in milliseconds.
const KQL = `requests | where timestamp > ago(15m) | summarize invocations=sum(itemCount), errors=sumif(itemCount, success == false), ` +
	`avg=avg(duration), p95=percentile(duration,95), p99=percentile(duration,99), max=max(duration) ` +
	`by bin(timestamp, 1m), operation_Name, cloud_RoleName`

// FunctionSlug is the serverless_functions slug of one function inside a Function App.
func FunctionSlug(appName, function string) string { return model.Slug("azure", appName+"-"+function) }

// component is a Microsoft.Insights/components resource (subset).
type component struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags"`
}

// components indexes App Insights components by linked site id (lower-cased) and by name.
type components struct {
	bySite map[string]component
	byName map[string]component // fallback: component named like the app (portal default), same subscription
}

func (c *components) add(x component) {
	for k := range x.Tags {
		if id, ok := strings.CutPrefix(k, "hidden-link:"); ok && strings.Contains(strings.ToLower(id), "/providers/microsoft.web/sites/") {
			c.bySite[strings.ToLower(strings.TrimRight(id, "/"))] = x
		}
	}
	c.byName[strings.ToLower(subscriptionOf(x.ID)+"/"+x.Name)] = x
}

// find returns the component linked to a site (hidden-link tag, case-insensitive id), else one with the app's name.
func (c *components) find(s site) (component, bool) {
	if c == nil {
		return component{}, false
	}
	if x, ok := c.bySite[strings.ToLower(strings.TrimRight(s.ID, "/"))]; ok {
		return x, true
	}
	x, ok := c.byName[strings.ToLower(subscriptionOf(s.ID)+"/"+s.Name)]
	return x, ok
}

// listComponents lists the App Insights components of the subscription (or of the configured resource groups).
// A 403/404 (no access, provider not registered) is not an error: the apps just stay "unavailable".
func (p *Provider) listComponents(ctx context.Context, sub string, into *components) error {
	paths := []string{"/subscriptions/" + url.PathEscape(sub) + "/providers/Microsoft.Insights/components"}
	if len(p.cfg.ResourceGroups) > 0 {
		paths = paths[:0]
		for _, rg := range p.cfg.ResourceGroups {
			paths = append(paths, "/subscriptions/"+url.PathEscape(sub)+"/resourceGroups/"+url.PathEscape(rg)+"/providers/Microsoft.Insights/components")
		}
	}
	var errs []string
	for _, path := range paths {
		err := p.list(ctx, path, api(apiComponents), func(raw json.RawMessage) error {
			var x component
			if json.Unmarshal(raw, &x) == nil && x.ID != "" {
				into.add(x)
			}
			return nil
		})
		if err != nil && !httpx.IsStatus(err, http.StatusForbidden) && !httpx.IsStatus(err, http.StatusNotFound) {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("list app insights components: %s", strings.Join(errs, "; "))
	}
	return nil
}

// aiRow is one (minute, operation, role) row of the KQL result.
type aiRow struct {
	TS                             time.Time
	Operation, Role                string
	Invocations, Errors            float64
	Avg, P95, P99, Max             float64
	hasAvg, hasP95, hasP99, hasMax bool
}

// queryResponse is the App Insights REST API answer.
type queryResponse struct {
	Tables []struct {
		Name    string `json:"name"`
		Columns []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
		Rows [][]any `json:"rows"`
	} `json:"tables"`
}

// ParseQuery turns the first table of a query response into rows (columns are matched by name).
func ParseQuery(r *queryResponse) []aiRow {
	if r == nil || len(r.Tables) == 0 {
		return nil
	}
	t := r.Tables[0]
	col := map[string]int{}
	for i, c := range t.Columns {
		col[strings.ToLower(c.Name)] = i
	}
	get := func(row []any, name string) (any, bool) {
		i, ok := col[strings.ToLower(name)]
		if !ok || i >= len(row) || row[i] == nil {
			return nil, false
		}
		return row[i], true
	}
	num := func(row []any, name string) (float64, bool) {
		v, ok := get(row, name)
		if !ok {
			return 0, false
		}
		switch x := v.(type) {
		case float64:
			return x, true
		case string: // "NaN", "Infinity" or numbers as strings
			var f float64
			if _, err := fmt.Sscanf(x, "%g", &f); err == nil && f == f {
				return f, true
			}
		}
		return 0, false
	}
	str := func(row []any, name string) string {
		v, _ := get(row, name)
		s, _ := v.(string)
		return s
	}
	var out []aiRow
	for _, row := range t.Rows {
		ts, err := time.Parse(time.RFC3339Nano, str(row, "timestamp"))
		if err != nil {
			continue
		}
		r := aiRow{TS: ts.UTC().Truncate(time.Minute), Operation: str(row, "operation_Name"), Role: str(row, "cloud_RoleName")}
		r.Invocations, _ = num(row, "invocations")
		r.Errors, _ = num(row, "errors")
		r.Avg, r.hasAvg = num(row, "avg")
		r.P95, r.hasP95 = num(row, "p95")
		r.P99, r.hasP99 = num(row, "p99")
		r.Max, r.hasMax = num(row, "max")
		if r.Operation == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// aiEntry is the last query of one component (throttling + hand-over from discovery to the metrics step).
type aiEntry struct {
	run     sync.Mutex // held while a query is in flight
	mu      sync.Mutex
	at      time.Time // time of the last query attempt
	rows    []aiRow
	err     error
	emitted bool // rows already turned into samples
}

type aiCache struct {
	mu sync.Mutex
	m  map[string]*aiEntry
}

func (c *aiCache) entry(id string) *aiEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*aiEntry{}
	}
	k := strings.ToLower(id)
	if c.m[k] == nil {
		c.m[k] = &aiEntry{}
	}
	return c.m[k]
}

// query returns the rows of the component's last query, querying again only when QueryInterval has elapsed and
// no query is in flight. fresh reports whether the returned rows have not been emitted yet; take marks them
// emitted (metrics step).
func (p *Provider) query(ctx context.Context, componentID string, now time.Time, take bool) (rows []aiRow, fresh bool, err error) {
	e := p.ai.entry(componentID)
	if e.run.TryLock() {
		e.mu.Lock()
		due := e.at.IsZero() || now.Sub(e.at) >= QueryInterval
		e.mu.Unlock()
		if due {
			var r queryResponse
			qerr := p.arm.Do(ctx, http.MethodPost, componentID+"/query", api(apiAIQuery),
				map[string]string{"query": KQL, "timespan": "PT15M"}, &r)
			e.mu.Lock()
			e.at, e.err = now, qerr
			if qerr == nil {
				e.rows, e.emitted = ParseQuery(&r), false
			}
			e.mu.Unlock()
		}
		e.run.Unlock()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rows, fresh, err = e.rows, !e.emitted, e.err
	if take {
		e.emitted = true
	}
	return rows, fresh, err
}

// child is one function of a connected app (metrics mapping).
type child struct{ Slug, Name string }

// roleOf returns the most frequent cloud_RoleName (by invocations) of an operation; "" = no data. With op == ""
// all operations of the app count.
func roleOf(rows []aiRow, op string) string {
	n := map[string]float64{}
	for _, r := range rows {
		if r.Role != "" && (op == "" || strings.EqualFold(r.Operation, op)) {
			n[r.Role] += max(r.Invocations, 1e-9)
		}
	}
	best, bestN := "", -1.0
	for role, c := range n {
		if c > bestN || (c == bestN && role < best) {
			best, bestN = role, c
		}
	}
	return best
}

// fnInfo is one entry of {site}/functions.
type fnInfo struct {
	Name, Trigger, Language string
	Disabled                bool
}

// children builds the per-function rows of a connected app. appFn is the (finished) app row.
func (p *Provider) children(appFn model.Function, fns []fnInfo, rows []aiRow, override string) ([]model.Function, []child) {
	tag := ""
	if t, ok := appFn.Meta["tags"].(map[string]string); ok {
		tag = strings.TrimSpace(t[ServiceTag])
	}
	appRole := roleOf(rows, "")
	var out []model.Function
	var ch []child
	for _, f := range fns {
		slug := FunctionSlug(appFn.App, f.Name)
		fn := model.Function{
			Slug: slug, Name: f.Name, App: appFn.App, Region: appFn.Region, Runtime: appFn.Runtime, MemoryMB: appFn.MemoryMB,
			Triggers: []string{},
			Meta: map[string]any{"parent": appFn.Slug, "source": "appinsights", "trigger": f.Trigger,
				"appId": appFn.Meta["id"], "resourceGroup": appFn.Meta["resourceGroup"], "subscriptionId": appFn.Meta["subscriptionId"]},
		}
		if f.Trigger != "" {
			fn.Triggers = []string{f.Trigger}
		}
		if f.Disabled {
			fn.Meta["disabled"] = true
		}
		if pl, ok := appFn.Meta["plan"]; ok {
			fn.Meta["plan"] = pl
		}
		if f.Trigger == "http" && appFn.URL != "" {
			fn.URL = appFn.URL + "/api/" + f.Name
		}
		if f.Language != "" {
			fn.Meta["language"] = f.Language
		}
		// service: app setting > cloud_RoleName (unless it is just the app name, the Functions default) > tag > app.
		role := roleOf(rows, f.Name)
		if role == "" {
			role = appRole
		}
		if role != "" {
			p.rememberRole(slug, role)
		} else {
			role = p.role(slug)
		}
		switch {
		case override != "":
			fn.Service = override
		case role != "" && !(strings.EqualFold(role, appFn.App) && tag != ""):
			fn.Service = role
		case tag != "":
			fn.Service = tag
		default:
			fn.Service = appFn.App
		}
		out = append(out, fn)
		ch = append(ch, child{Slug: slug, Name: f.Name})
	}
	return out, ch
}

func (p *Provider) rememberRole(slug, role string) {
	p.rolesMu.Lock()
	defer p.rolesMu.Unlock()
	if p.roles == nil {
		p.roles = map[string]string{}
	}
	p.roles[slug] = role
}

func (p *Provider) role(slug string) string {
	p.rolesMu.Lock()
	defer p.rolesMu.Unlock()
	return p.roles[slug]
}

// appSettingsService reads TASKTROOPER_SERVICE / OTEL_SERVICE_NAME (config.readAppSettings). No permission
// (Website Contributor missing) → "" and logged once per app; no other setting is kept.
func (p *Provider) appSettingsService(ctx context.Context, s site) (string, error) {
	var r struct {
		Props map[string]any `json:"properties"`
	}
	err := p.arm.Do(ctx, http.MethodPost, s.ID+"/config/appsettings/list", api(apiWeb), nil, &r)
	if httpx.IsStatus(err, http.StatusForbidden) || httpx.IsStatus(err, http.StatusUnauthorized) || httpx.IsStatus(err, http.StatusNotFound) {
		if _, logged := p.settingsLogged.LoadOrStore(strings.ToLower(s.ID), true); !logged && p.log != nil {
			p.log.Info("azure: cannot read app settings (needs Website Contributor); service name from tags/App Insights", "app", s.Name)
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, k := range []string{SettingService, SettingOTelService} {
		for name, v := range r.Props {
			if s, ok := v.(string); ok && strings.EqualFold(name, k) && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), nil
			}
		}
	}
	return "", nil
}

// collectFunctions emits trooper.faas.* samples of the functions of one connected app from its last App Insights
// query (querying again when due). Minutes at or after end (incomplete) are dropped.
func (p *Provider) collectFunctions(ctx context.Context, a app, now, end time.Time) ([]model.Sample, error) {
	if a.Component == "" || len(a.Children) == 0 {
		return nil, nil
	}
	rows, fresh, err := p.query(ctx, a.Component, now, true)
	if !fresh {
		return nil, err // throttled or already sent; err is the state of the last query
	}
	return MapRows(a, rows, end), err
}

// MapRows aggregates rows per function minute (several cloud_RoleNames per operation are merged: counts summed,
// avg weighted by invocations, max/percentiles maxed) and maps them to trooper.faas.* gauges (ms).
func MapRows(a app, rows []aiRow, end time.Time) []model.Sample {
	bySlug := map[string]child{}
	for _, c := range a.Children {
		bySlug[strings.ToLower(c.Name)] = c
	}
	type key struct {
		slug string
		ts   time.Time
	}
	type agg struct {
		c                      child
		inv, errs, wsum, wn    float64
		max, p95, p99          float64
		hasMax, hasP95, hasP99 bool
	}
	m := map[key]*agg{}
	for _, r := range rows {
		c, ok := bySlug[strings.ToLower(r.Operation)]
		if !ok || (!end.IsZero() && !r.TS.Before(end)) {
			continue
		}
		k := key{c.Slug, r.TS}
		g := m[k]
		if g == nil {
			g = &agg{c: c}
			m[k] = g
		}
		g.inv += r.Invocations
		g.errs += r.Errors
		if r.hasAvg {
			w := max(r.Invocations, 1)
			g.wsum += r.Avg * w
			g.wn += w
		}
		if r.hasMax {
			g.max, g.hasMax = max(g.max, r.Max), true
		}
		if r.hasP95 {
			g.p95, g.hasP95 = max(g.p95, r.P95), true
		}
		if r.hasP99 {
			g.p99, g.hasP99 = max(g.p99, r.P99), true
		}
	}
	keys := make([]key, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].slug != keys[j].slug {
			return keys[i].slug < keys[j].slug
		}
		return keys[i].ts.Before(keys[j].ts)
	})
	var out []model.Sample
	for _, k := range keys {
		g := m[k]
		attrs := model.FaaSAttrs(g.c.Slug, g.c.Name)
		add := func(name string, v float64) {
			out = append(out, model.Sample{Name: name, Value: v, TS: k.ts, Region: a.Region, Attrs: attrs})
		}
		add(model.FaaSInvocations, g.inv)
		add(model.FaaSErrors, g.errs)
		if g.wn > 0 {
			add(model.FaaSDurationAvg, g.wsum/g.wn)
		}
		if g.hasMax {
			add(model.FaaSDurationMax, g.max)
		}
		if g.hasP95 {
			add(model.FaaSDurationP95, g.p95)
		}
		if g.hasP99 {
			add(model.FaaSDurationP99, g.p99)
		}
	}
	return out
}
