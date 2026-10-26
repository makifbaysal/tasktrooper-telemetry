package vercel

import (
	"encoding/json"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/tasktrooper/telemetry/services/ingest/internal/norm"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
)

// ── log drain ──

type logEntry struct {
	ID                string    `json:"id"`
	DeploymentID      string    `json:"deploymentId"`
	Source            string    `json:"source"`
	Host              string    `json:"host"`
	Timestamp         flexTime  `json:"timestamp"`
	ProjectID         string    `json:"projectId"`
	ProjectName       string    `json:"projectName"`
	Level             string    `json:"level"`
	Message           string    `json:"message"`
	BuildID           string    `json:"buildId"`
	Entrypoint        string    `json:"entrypoint"`
	Destination       string    `json:"destination"`
	Path              string    `json:"path"`
	Type              string    `json:"type"`
	StatusCode        *int      `json:"statusCode"`
	RequestID         string    `json:"requestId"`
	Environment       string    `json:"environment"`
	CustomEnvironment string    `json:"customEnvironment"`
	Branch            string    `json:"branch"`
	EdgeType          string    `json:"edgeType"`
	ExecutionRegion   string    `json:"executionRegion"`
	TraceID           string    `json:"traceId"`
	SpanID            string    `json:"spanId"`
	TraceIDDot        string    `json:"trace.id"`
	SpanIDDot         string    `json:"span.id"`
	Proxy             *logProxy `json:"proxy"`
}

type logProxy struct {
	Timestamp        flexTime `json:"timestamp"`
	Method           string   `json:"method"`
	Host             string   `json:"host"`
	Path             string   `json:"path"`
	Region           string   `json:"region"`
	StatusCode       *int     `json:"statusCode"`
	Scheme           string   `json:"scheme"`
	ResponseByteSize int64    `json:"responseByteSize"`
	PathType         string   `json:"pathType"`
	VercelCache      string   `json:"vercelCache"`
	LambdaRegion     string   `json:"lambdaRegion"`
	WafAction        string   `json:"wafAction"`
	WafRuleID        string   `json:"wafRuleId"`
	// clientIp, userAgent, referer, ja3/ja4 digests are deliberately not read (PII).
}

func severity(level string, status int) (logspb.SeverityNumber, string) {
	switch strings.ToLower(level) {
	case "fatal", "critical":
		return logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, "fatal"
	case "error":
		return logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "error"
	case "warning", "warn":
		return logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "warn"
	case "debug":
		return logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, "debug"
	}
	if status == -1 { // "-1 means no response returned and the lambda crashed"
		return logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "error"
	}
	return logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "info"
}

func (c *converter) addLog(e *logEntry) {
	svc := c.service(e.ProjectID, e.ProjectName)
	env := e.Environment
	if e.CustomEnvironment != "" {
		env = e.CustomEnvironment
	}
	key := svc + "\x00" + env + "\x00" + e.DeploymentID
	rl := c.logRes[key]
	if rl == nil {
		extra := kvs{}
		extra.str("deployment.environment.name", env)
		extra.str("service.version", e.DeploymentID)
		extra.str("vercel.project_id", e.ProjectID)
		extra.str("vercel.project_name", e.ProjectName)
		rl = &logspb.ResourceLogs{Resource: resource(svc, c.in, extra...), ScopeLogs: []*logspb.ScopeLogs{{}}}
		c.logRes[key] = rl
	}

	status := 0
	if e.StatusCode != nil {
		status = *e.StatusCode
	} else if e.Proxy != nil && e.Proxy.StatusCode != nil {
		status = *e.Proxy.StatusCode
	}
	sevNum, sevText := severity(e.Level, status)

	a := kvs{}
	a.str("vercel.source", e.Source)
	a.str("vercel.deployment_id", e.DeploymentID)
	a.str("vercel.log_id", e.ID)
	a.str("vercel.type", e.Type)
	a.str("vercel.request_id", e.RequestID)
	a.str("vercel.host", e.Host)
	a.str("vercel.region", e.ExecutionRegion)
	a.str("vercel.entrypoint", e.Entrypoint)
	a.str("vercel.build_id", e.BuildID)
	a.str("vercel.branch", e.Branch)
	a.str("vercel.edge_type", e.EdgeType)
	a.str("vercel.destination", norm.StripQueryValues(e.Destination))
	route := Route(e.Path)
	method := ""
	urlPath := ""
	if p := e.Proxy; p != nil {
		method = strings.ToUpper(p.Method)
		if p.Path != "" {
			urlPath = norm.StripQueryValues(p.Path)
			if route == "" {
				route = Route(p.Path)
			}
		}
		a.str("vercel.cache", p.VercelCache)
		a.str("vercel.path_type", p.PathType)
		a.str("vercel.waf_action", p.WafAction)
		a.str("vercel.waf_rule_id", p.WafRuleID)
		a.str("vercel.edge_region", p.Region)
		a.str("vercel.lambda_region", p.LambdaRegion)
		if p.ResponseByteSize > 0 {
			a.int("http.response.body.size", p.ResponseByteSize)
		}
	}
	a.str("http.route", route)
	a.str("trooper.entry.route", route)
	a.str("http.request.method", method)
	a.str("url.path", urlPath)
	if status != 0 {
		a.int("http.response.status_code", int64(status))
	}

	body := e.Message
	if body == "" && (method != "" || urlPath != "") {
		body = strings.TrimSpace(method + " " + urlPath)
		if status != 0 {
			body += " " + strconv.Itoa(status)
		}
	}
	traceID, spanID := e.TraceID, e.SpanID
	if traceID == "" {
		traceID = e.TraceIDDot
	}
	if spanID == "" {
		spanID = e.SpanIDDot
	}
	ts := uint64(e.Timestamp.ms()) * 1e6
	rec := &logspb.LogRecord{
		TimeUnixNano: ts, ObservedTimeUnixNano: ts,
		SeverityNumber: sevNum, SeverityText: sevText,
		Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		Attributes: a,
		TraceId:    hexBytes(traceID, 16),
		SpanId:     hexBytes(spanID, 8),
	}
	rl.ScopeLogs[0].LogRecords = append(rl.ScopeLogs[0].LogRecords, rec)
}

// ── speed insights drain ──

type speedEntry struct {
	Schema            string     `json:"schema"`
	Timestamp         flexTime   `json:"timestamp"`
	ProjectID         string     `json:"projectId"`
	OwnerID           string     `json:"ownerId"`
	DeviceID          flexString `json:"deviceId"`
	MetricType        string     `json:"metricType"`
	Value             float64    `json:"value"`
	Origin            string     `json:"origin"`
	Path              string     `json:"path"`
	Route             string     `json:"route"`
	Country           string     `json:"country"`
	OSName            string     `json:"osName"`
	OSVersion         string     `json:"osVersion"`
	ClientName        string     `json:"clientName"`
	ClientVersion     string     `json:"clientVersion"`
	DeviceType        string     `json:"deviceType"`
	DeviceBrand       string     `json:"deviceBrand"`
	DeviceModel       string     `json:"deviceModel"`
	ConnectionSpeed   string     `json:"connectionSpeed"`
	SDKName           string     `json:"sdkName"`
	SDKVersion        string     `json:"sdkVersion"`
	VercelEnvironment string     `json:"vercelEnvironment"`
	DeploymentID      string     `json:"deploymentId"`
	Attribution       string     `json:"attribution"`
}

// client is the per-batch resource shared by speed-insights and analytics entries.
type client struct {
	app, env, version                      string
	device, brand, model, os, osv, br, brv string
	sdk, sdkv                              string
}

func (c *converter) batch(cl client) *rumpb.RumBatch {
	key := strings.Join([]string{cl.app, cl.env, cl.version, cl.device, cl.brand, cl.model, cl.os, cl.osv, cl.br, cl.brv, cl.sdk, cl.sdkv}, "\x00")
	b := c.rumBatch[key]
	if b != nil {
		return b
	}
	res := &rumpb.Resource{
		App: cl.app, Version: cl.version, Environment: cl.env, Platform: rumpb.Platform_PLATFORM_WEB,
		Attributes: map[string]string{"cloud.provider": "vercel"},
	}
	if c.in.ID != 0 {
		res.Attributes["trooper.integration.id"] = strconv.FormatInt(c.in.ID, 10)
	}
	if cl.sdk != "" {
		res.Sdk = &rumpb.Sdk{Name: cl.sdk, Version: cl.sdkv}
	}
	if cl.device != "" || cl.brand != "" || cl.model != "" {
		res.Device = &rumpb.Device{Type: strings.ToLower(cl.device), Manufacturer: cl.brand, Model: cl.model}
	}
	if cl.os != "" {
		res.Os = &rumpb.Os{Name: cl.os, Version: cl.osv}
	}
	if cl.br != "" {
		res.Browser = &rumpb.Browser{Name: cl.br, Version: cl.brv}
	}
	b = &rumpb.RumBatch{Resource: res}
	c.rumBatch[key] = b
	return b
}

// ids derives stable, salted anonymous/session ids from Vercel's device id. Vercel drains carry no session id:
// a session is approximated as one device on one UTC day.
func (c *converter) ids(owner, project string, device flexString, tsMs int64) (anon, session string) {
	if device == "" {
		return "", ""
	}
	salt := strconv.FormatInt(c.in.ProjectID, 10)
	anon = hashID("vercel-anon", salt, owner, project, string(device))
	session = hashID("vercel-session", anon, strconv.FormatInt(tsMs/86_400_000, 10))
	return anon, session
}

func connection(s string) string {
	switch strings.ToLower(s) {
	case "4g":
		return "4g"
	case "3g", "2g", "slow-2g":
		return "3g"
	case "wifi":
		return "wifi"
	case "cellular":
		return "cellular"
	case "offline":
		return "offline"
	case "":
		return ""
	}
	return "unknown"
}

var msVitals = map[string]bool{"LCP": true, "FCP": true, "TTFB": true, "INP": true, "FID": true}

func (c *converter) addSpeed(e *speedEntry) {
	app := c.service(e.ProjectID, "")
	b := c.batch(client{app: app, env: e.VercelEnvironment, version: e.DeploymentID, device: e.DeviceType, brand: e.DeviceBrand,
		model: e.DeviceModel, os: e.OSName, osv: e.OSVersion, br: e.ClientName, brv: e.ClientVersion, sdk: e.SDKName, sdkv: e.SDKVersion})
	ts := e.Timestamp.ms()
	anon, session := c.ids(e.OwnerID, e.ProjectID, e.DeviceID, ts)
	name := strings.ToUpper(e.MetricType)
	v := e.Value
	if c.in.VitalsInS && msVitals[name] {
		v *= 1000
	}
	view := Route(e.Route)
	if view == "" {
		view = Route(e.Path)
	}
	attrs := map[string]string{"vercel.source": "speed_insights"}
	put(attrs, "geo.country", strings.ToUpper(e.Country))
	put(attrs, "vercel.deployment_id", e.DeploymentID)
	put(attrs, "view.url", e.Origin+e.Path)
	put(attrs, "vital.attribution", e.Attribution)
	b.Events = append(b.Events, &rumpb.RumEvent{
		Type: rumpb.EventType_VITAL, TsMs: ts, SessionId: session, AnonymousId: anon, ViewName: view,
		Connection: connection(e.ConnectionSpeed),
		Payload:    &rumpb.RumEvent_Vital{Vital: &rumpb.VitalPayload{Name: name, Value: v, Rating: rating(name, v)}},
		Attributes: attrs,
	})
}

// rating follows the web-vitals thresholds (good / needs-improvement / poor).
func rating(name string, v float64) string {
	var good, poor float64
	switch name {
	case "LCP":
		good, poor = 2500, 4000
	case "FCP":
		good, poor = 1800, 3000
	case "TTFB":
		good, poor = 800, 1800
	case "INP":
		good, poor = 200, 500
	case "FID":
		good, poor = 100, 300
	case "CLS":
		good, poor = 0.1, 0.25
	default:
		return ""
	}
	switch {
	case v <= good:
		return "good"
	case v <= poor:
		return "needs-improvement"
	}
	return "poor"
}

func put(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// ── web analytics drain ──

type analyticsEntry struct {
	Schema            string     `json:"schema"`
	EventType         string     `json:"eventType"`
	EventName         string     `json:"eventName"`
	EventData         flexString `json:"eventData"`
	Timestamp         flexTime   `json:"timestamp"`
	ProjectID         string     `json:"projectId"`
	OwnerID           string     `json:"ownerId"`
	DeviceID          flexString `json:"deviceId"`
	SessionID         flexString `json:"sessionId"`
	Origin            string     `json:"origin"`
	Path              string     `json:"path"`
	Referrer          string     `json:"referrer"`
	QueryParams       string     `json:"queryParams"`
	Route             string     `json:"route"`
	Country           string     `json:"country"`
	OSName            string     `json:"osName"`
	OSVersion         string     `json:"osVersion"`
	ClientName        string     `json:"clientName"`
	ClientVersion     string     `json:"clientVersion"`
	DeviceType        string     `json:"deviceType"`
	DeviceBrand       string     `json:"deviceBrand"`
	DeviceModel       string     `json:"deviceModel"`
	SDKName           string     `json:"sdkName"`
	SDKVersion        string     `json:"sdkVersion"`
	VercelEnvironment string     `json:"vercelEnvironment"`
	Deployment        string     `json:"deployment"`
	DeploymentID      string     `json:"deploymentId"`
}

func (c *converter) addAnalytics(e *analyticsEntry) {
	app := c.service(e.ProjectID, "")
	dep := e.Deployment
	if dep == "" {
		dep = e.DeploymentID
	}
	b := c.batch(client{app: app, env: e.VercelEnvironment, version: dep, device: e.DeviceType, brand: e.DeviceBrand,
		model: e.DeviceModel, os: e.OSName, osv: e.OSVersion, br: e.ClientName, brv: e.ClientVersion, sdk: e.SDKName, sdkv: e.SDKVersion})
	ts := e.Timestamp.ms()
	anon, session := c.ids(e.OwnerID, e.ProjectID, e.DeviceID, ts)
	if e.SessionID != "" {
		session = hashID("vercel-session", strconv.FormatInt(c.in.ProjectID, 10), string(e.SessionID))
	}
	view := Route(e.Route)
	if view == "" {
		view = Route(e.Path)
	}
	attrs := map[string]string{"vercel.source": "web_analytics"}
	put(attrs, "geo.country", strings.ToUpper(e.Country))
	put(attrs, "vercel.deployment_id", dep)
	ev := &rumpb.RumEvent{TsMs: ts, SessionId: session, AnonymousId: anon, ViewName: view, Attributes: attrs}
	if strings.EqualFold(e.EventType, "pageview") {
		ev.Type = rumpb.EventType_VIEW_START
		ev.ViewId = hashID("vercel-view", session, e.Path, strconv.FormatInt(ts, 10))
		u := e.Origin + e.Path
		if e.QueryParams != "" {
			u += "?" + strings.TrimPrefix(e.QueryParams, "?")
		}
		ev.Payload = &rumpb.RumEvent_View{View: &rumpb.ViewPayload{Url: norm.StripQueryValues(u), Referrer: norm.StripQueryValues(e.Referrer)}}
	} else {
		ev.Type = rumpb.EventType_CUSTOM
		ev.Payload = &rumpb.RumEvent_Custom{Custom: &rumpb.CustomPayload{Name: e.EventName, Props: eventProps(string(e.EventData))}}
	}
	b.Events = append(b.Events, ev)
}

// eventProps flattens eventData (a JSON object encoded as a string) into string props.
func eventProps(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return map[string]string{"data": s}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case nil:
		default:
			b, _ := json.Marshal(x)
			out[k] = string(b)
		}
	}
	return out
}
