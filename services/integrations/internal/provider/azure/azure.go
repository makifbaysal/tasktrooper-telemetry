// Package azure discovers Azure Function Apps (ARM) and reads their Azure Monitor metrics (ADR-020,
// contracts §21). One serverless_functions row per Function App (slug azure-<app>); the functions inside an
// app go to meta.functions because Azure Monitor metrics are app-level.
package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tasktrooper/telemetry/services/integrations/internal/httpx"
	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// Defaults (TT_AZURE_LOGIN_URL / TT_AZURE_ARM_URL override them for tests).
const (
	DefaultLoginURL = "https://login.microsoftonline.com"
	DefaultARMURL   = "https://management.azure.com"
	// Scope is always the public ARM scope, also when the ARM URL is overridden.
	Scope = "https://management.azure.com/.default"

	apiSubscriptions = "2022-12-01"
	apiWeb           = "2023-12-01"
	apiMetrics       = "2023-10-01"
)

// ServiceTag is the Function App tag that overrides the span service name.
const ServiceTag = "tasktrooper.service"

// Config is integrations.config for provider azure.
type Config struct {
	TenantID        string   `json:"tenantId"`
	ClientID        string   `json:"clientId"`
	SubscriptionIDs []string `json:"subscriptionIds"` // empty = every subscription the principal can read
	ResourceGroups  []string `json:"resourceGroups"`  // optional filter (case-insensitive)
	Env             string   `json:"env"`
	// MaxApps caps Function Apps per integration (default 500).
	MaxApps int `json:"maxApps"`
}

// Options are runtime settings (tests).
type Options struct {
	LoginURL string
	ARMURL   string
	HTTP     *http.Client
	// BaseDelay of the HTTP retry backoff (tests use a few ms).
	BaseDelay time.Duration
}

// Provider implements model.Provider.
type Provider struct {
	cfg    Config
	arm    *httpx.Client
	tokens *tokenSource
	// unsupported metric names per app (Flex plans, Linux Consumption gaps).
	unsupported unsupported
}

// ParseSecret accepts the client secret as plain text or as JSON {"clientSecret": "..."}.
func ParseSecret(b []byte) (string, error) {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "{") {
		var j struct {
			ClientSecret string `json:"clientSecret"`
		}
		if json.Unmarshal([]byte(s), &j) != nil || j.ClientSecret == "" {
			return "", errors.New(`azure: credentials must be the client secret or JSON {"clientSecret"}`)
		}
		return j.ClientSecret, nil
	}
	if s == "" {
		return "", errors.New("azure: client secret missing (credentials_ref)")
	}
	return s, nil
}

// New builds the provider. secret is the resolved credentials_ref (client secret).
func New(raw json.RawMessage, secret []byte, o Options) (*Provider, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("azure config: %w", err)
		}
	}
	if cfg.TenantID == "" || cfg.ClientID == "" {
		return nil, errors.New("azure: config.tenantId and config.clientId are required")
	}
	cs, err := ParseSecret(secret)
	if err != nil {
		return nil, err
	}
	if cfg.MaxApps <= 0 {
		cfg.MaxApps = 500
	}
	if o.LoginURL == "" {
		o.LoginURL = DefaultLoginURL
	}
	if o.ARMURL == "" {
		o.ARMURL = DefaultARMURL
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	ts := &tokenSource{url: strings.TrimRight(o.LoginURL, "/") + "/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/token",
		clientID: cfg.ClientID, secret: cs, http: hc}
	arm := &httpx.Client{Base: strings.TrimRight(o.ARMURL, "/"), HTTP: hc, BaseDelay: o.BaseDelay, Auth: func(r *http.Request) error {
		tok, err := ts.token(r.Context())
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}}
	return &Provider{cfg: cfg, arm: arm, tokens: ts}, nil
}

// tokenSource is an OAuth2 client-credentials token, cached until shortly before it expires.
type tokenSource struct {
	url, clientID, secret string
	http                  *http.Client

	mu      sync.Mutex
	tok     string
	expires time.Time
	// Fetches counts token requests (tests).
	Fetches int
}

func (t *tokenSource) token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Now().Before(t.expires) {
		return t.tok, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {t.clientID}, "client_secret": {t.secret}, "scope": {Scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	t.Fetches++
	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("azure token: %s", t.scrub(err.Error()))
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"` // number (v2) or string (v1)
		Error       string          `json:"error"`
		Description string          `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		msg := body.Error
		if d := strings.SplitN(body.Description, "\r\n", 2)[0]; d != "" {
			msg += ": " + d
		}
		return "", fmt.Errorf("azure token: HTTP %d %s", resp.StatusCode, t.scrub(msg))
	}
	secs := 3600.0
	var n float64
	var s string
	if json.Unmarshal(body.ExpiresIn, &n) == nil && n > 0 {
		secs = n
	} else if json.Unmarshal(body.ExpiresIn, &s) == nil {
		_, _ = fmt.Sscanf(s, "%g", &secs)
	}
	t.tok = body.AccessToken
	// Refresh 2 minutes early (at least half the lifetime for very short tokens).
	t.expires = time.Now().Add(time.Duration(secs*float64(time.Second)) - min(2*time.Minute, time.Duration(secs/2*float64(time.Second))))
	return t.tok, nil
}

func (t *tokenSource) scrub(s string) string {
	if t.secret != "" {
		s = strings.ReplaceAll(s, t.secret, "***")
	}
	return s
}

// list GETs an ARM collection and follows nextLink (same host only — the token is never sent elsewhere).
func (p *Provider) list(ctx context.Context, path string, q url.Values, each func(json.RawMessage) error) error {
	for path != "" {
		var page struct {
			Value    []json.RawMessage `json:"value"`
			NextLink string            `json:"nextLink"`
		}
		if err := p.arm.Get(ctx, path, q, &page); err != nil {
			return err
		}
		for _, v := range page.Value {
			if err := each(v); err != nil {
				return err
			}
		}
		path, q = "", nil
		if page.NextLink != "" && strings.HasPrefix(page.NextLink, p.arm.Base+"/") {
			path = strings.TrimPrefix(page.NextLink, p.arm.Base)
		}
	}
	return nil
}

func api(v string) url.Values { return url.Values{"api-version": {v}} }

// ARM shapes (subset).
type site struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags"`
	Props    struct {
		State           string `json:"state"`
		DefaultHostName string `json:"defaultHostName"`
		ResourceGroup   string `json:"resourceGroup"`
		ServerFarmID    string `json:"serverFarmId"`
		SKU             string `json:"sku"`
		Reserved        bool   `json:"reserved"` // Linux
		SiteConfig      *struct {
			LinuxFxVersion string `json:"linuxFxVersion"`
		} `json:"siteConfig"`
		FunctionAppConfig *struct {
			Runtime *struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"runtime"`
			ScaleAndConcurrency *struct {
				InstanceMemoryMB int `json:"instanceMemoryMB"`
			} `json:"scaleAndConcurrency"`
		} `json:"functionAppConfig"`
	} `json:"properties"`
}

type webProps struct {
	LinuxFxVersion        string `json:"linuxFxVersion"`
	NetFrameworkVersion   string `json:"netFrameworkVersion"`
	PowerShellVersion     string `json:"powerShellVersion"`
	JavaVersion           string `json:"javaVersion"`
	NodeVersion           string `json:"nodeVersion"`
	PythonVersion         string `json:"pythonVersion"`
	FunctionAppScaleLimit int    `json:"functionAppScaleLimit"`
}

type webConfig struct {
	Props webProps `json:"properties"`
}

type function struct {
	Name  string `json:"name"` // <app>/<function>
	Props struct {
		Name       string `json:"name"`
		IsDisabled bool   `json:"isDisabled"`
		Language   string `json:"language"`
		Config     struct {
			Bindings []map[string]any `json:"bindings"`
		} `json:"config"`
	} `json:"properties"`
}

// app is the discovery state the metrics step needs.
type app struct {
	ID, Name, Slug, Region, Plan string
}

type state struct{ apps []app }

// Slug is the serverless_functions slug of a Function App.
func Slug(appName string) string { return model.Slug("azure", appName) }

// Plan maps a site SKU to the contract's plan names.
func Plan(sku string) string {
	switch strings.ToLower(sku) {
	case "dynamic":
		return "Consumption"
	case "flexconsumption":
		return "Flex"
	case "elasticpremium":
		return "Premium"
	case "":
		return ""
	}
	return "Dedicated"
}

// Region normalizes an ARM location ("West Europe" → "westeurope").
func Region(loc string) string { return strings.ToLower(strings.ReplaceAll(loc, " ", "")) }

func isFunctionApp(kind string) bool {
	k := strings.ToLower(kind)
	return strings.Contains(k, "functionapp") && !strings.Contains(k, "workflowapp")
}

// Discover implements model.Provider.
func (p *Provider) Discover(ctx context.Context) (*model.Inventory, error) {
	subs := p.cfg.SubscriptionIDs
	var errs error
	if len(subs) == 0 {
		err := p.list(ctx, "/subscriptions", api(apiSubscriptions), func(raw json.RawMessage) error {
			var s struct {
				SubscriptionID string `json:"subscriptionId"`
				State          string `json:"state"`
			}
			if json.Unmarshal(raw, &s) == nil && s.SubscriptionID != "" && (s.State == "" || s.State == "Enabled") {
				subs = append(subs, s.SubscriptionID)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list subscriptions: %w", err)
		}
		if len(subs) == 0 {
			return nil, errors.New("azure: the service principal can read no subscription (assign Reader + Monitoring Reader)")
		}
	}
	var sites []site
	for _, sub := range subs {
		paths := []string{"/subscriptions/" + url.PathEscape(sub) + "/providers/Microsoft.Web/sites"}
		if len(p.cfg.ResourceGroups) > 0 {
			paths = paths[:0]
			for _, rg := range p.cfg.ResourceGroups {
				paths = append(paths, "/subscriptions/"+url.PathEscape(sub)+"/resourceGroups/"+url.PathEscape(rg)+"/providers/Microsoft.Web/sites")
			}
		}
		for _, path := range paths {
			err := p.list(ctx, path, api(apiWeb), func(raw json.RawMessage) error {
				var s site
				if json.Unmarshal(raw, &s) == nil && isFunctionApp(s.Kind) && len(sites) < p.cfg.MaxApps {
					sites = append(sites, s)
				}
				return nil
			})
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("subscription %s: list sites: %w", sub, err))
			}
		}
	}
	st := &state{}
	inv := &model.Inventory{State: st}
	fns := make([]model.Function, len(sites))
	fnErrs := make([]error, len(sites))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, s := range sites {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			fns[i], fnErrs[i] = p.describe(ctx, s)
		}()
	}
	wg.Wait()
	for i, s := range sites {
		if fnErrs[i] != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", s.Name, fnErrs[i]))
		}
		st.apps = append(st.apps, app{ID: s.ID, Name: s.Name, Slug: fns[i].Slug, Region: fns[i].Region, Plan: Plan(s.Props.SKU)})
	}
	inv.Functions = fns
	if len(sites) == 0 && errs != nil {
		return nil, errs
	}
	return inv, errs
}

// describe builds the serverless_functions row of one Function App (functions list + runtime).
func (p *Provider) describe(ctx context.Context, s site) (model.Function, error) {
	rg := s.Props.ResourceGroup
	if rg == "" {
		rg = resourceGroupOf(s.ID)
	}
	fn := model.Function{
		Slug: Slug(s.Name), Name: s.Name, App: s.Name, Region: Region(s.Location), Service: s.Name,
		Meta: map[string]any{"id": s.ID, "resourceGroup": rg, "subscriptionId": subscriptionOf(s.ID), "kind": s.Kind, "state": s.Props.State},
	}
	if pl := Plan(s.Props.SKU); pl != "" {
		fn.Meta["plan"] = pl
	}
	if s.Props.Reserved {
		fn.Meta["os"] = "linux"
	} else {
		fn.Meta["os"] = "windows"
	}
	if s.Props.DefaultHostName != "" {
		fn.URL = "https://" + s.Props.DefaultHostName
	}
	if len(s.Tags) > 0 {
		fn.Meta["tags"] = s.Tags
		if v := strings.TrimSpace(s.Tags[ServiceTag]); v != "" {
			fn.Service = v
		}
	}
	if fc := s.Props.FunctionAppConfig; fc != nil {
		if fc.Runtime != nil && fc.Runtime.Name != "" {
			fn.Runtime = strings.TrimSpace(strings.ToLower(fc.Runtime.Name) + " " + fc.Runtime.Version)
		}
		if fc.ScaleAndConcurrency != nil && fc.ScaleAndConcurrency.InstanceMemoryMB > 0 {
			fn.MemoryMB = &fc.ScaleAndConcurrency.InstanceMemoryMB
		}
	}
	if fn.Runtime == "" && s.Props.SiteConfig != nil {
		fn.Runtime = Runtime(webConfig{Props: webProps{LinuxFxVersion: s.Props.SiteConfig.LinuxFxVersion}})
	}
	var errs error
	if fn.Runtime == "" {
		var wc webConfig
		if err := p.arm.Get(ctx, s.ID+"/config/web", api(apiWeb), &wc); err == nil {
			fn.Runtime = Runtime(wc)
		} else if !httpx.IsStatus(err, http.StatusNotFound) {
			errs = errors.Join(errs, fmt.Errorf("site config: %w", err))
		}
	}
	var triggers []string
	var list []map[string]any
	err := p.list(ctx, s.ID+"/functions", api(apiWeb), func(raw json.RawMessage) error {
		var f function
		if json.Unmarshal(raw, &f) != nil {
			return nil
		}
		name := f.Props.Name
		if name == "" {
			name = f.Name[strings.LastIndex(f.Name, "/")+1:]
		}
		t := Trigger(f.Props.Config.Bindings)
		if t != "" && !f.Props.IsDisabled {
			triggers = append(triggers, t)
		}
		item := map[string]any{"name": name, "trigger": t}
		if f.Props.IsDisabled {
			item["disabled"] = true
		}
		if fn.Runtime == "" && f.Props.Language != "" {
			fn.Runtime = strings.ToLower(f.Props.Language)
		}
		list = append(list, item)
		return nil
	})
	if err != nil && !httpx.IsStatus(err, http.StatusNotFound) {
		errs = errors.Join(errs, fmt.Errorf("list functions: %w", err))
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["name"].(string) < list[j]["name"].(string) })
	if list == nil {
		list = []map[string]any{}
	}
	fn.Meta["functions"] = list
	sort.Strings(triggers)
	fn.Triggers = slices.Compact(triggers)
	if fn.Triggers == nil {
		fn.Triggers = []string{}
	}
	return fn, errs
}

// Runtime turns a site config into "python 3.11" / "node 20" / "dotnet-isolated 8.0" style strings.
func Runtime(wc webConfig) string {
	c := wc.Props
	if v := c.LinuxFxVersion; v != "" {
		name, ver, _ := strings.Cut(v, "|")
		name = strings.ToLower(name)
		if name == "docker" || name == "dockerimage" {
			return "container"
		}
		return strings.TrimSpace(name + " " + ver)
	}
	switch {
	case c.NodeVersion != "":
		return "node " + strings.TrimPrefix(c.NodeVersion, "~")
	case c.PowerShellVersion != "":
		return "powershell " + strings.TrimPrefix(c.PowerShellVersion, "~")
	case c.JavaVersion != "":
		return "java " + c.JavaVersion
	case c.PythonVersion != "":
		return "python " + c.PythonVersion
	case c.NetFrameworkVersion != "":
		return "dotnet " + strings.TrimPrefix(c.NetFrameworkVersion, "v")
	}
	return ""
}

// Trigger maps a function's trigger binding to a trigger string: http, schedule, queue:<q>, servicebus:<q|topic>,
// eventhub:<hub>, blob, cosmosdb:<container>, eventgrid, … ("" if the function has no trigger binding).
func Trigger(bindings []map[string]any) string {
	for _, b := range bindings {
		t, _ := b["type"].(string)
		lt := strings.ToLower(t)
		if !strings.HasSuffix(lt, "trigger") {
			continue
		}
		str := func(keys ...string) string {
			for _, k := range keys {
				if v, ok := b[k].(string); ok && v != "" && !strings.HasPrefix(v, "%") {
					return v
				}
			}
			return ""
		}
		with := func(prefix string, keys ...string) string {
			if v := str(keys...); v != "" {
				return prefix + ":" + v
			}
			return prefix
		}
		switch lt {
		case "httptrigger":
			return "http"
		case "timertrigger":
			return "schedule"
		case "queuetrigger":
			return with("queue", "queueName")
		case "servicebustrigger":
			return with("servicebus", "queueName", "topicName")
		case "eventhubtrigger":
			return with("eventhub", "eventHubName", "path")
		case "blobtrigger":
			return "blob"
		case "cosmosdbtrigger":
			return with("cosmosdb", "containerName", "collectionName")
		case "eventgridtrigger":
			return "eventgrid"
		case "kafkatrigger":
			return with("kafka", "topic")
		case "rabbitmqtrigger":
			return with("rabbitmq", "queueName")
		}
		return strings.TrimSuffix(lt, "trigger")
	}
	return ""
}

func subscriptionOf(id string) string  { return segment(id, "subscriptions") }
func resourceGroupOf(id string) string { return segment(id, "resourceGroups") }

func segment(id, key string) string {
	parts := strings.Split(id, "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], key) {
			return parts[i+1]
		}
	}
	return ""
}
