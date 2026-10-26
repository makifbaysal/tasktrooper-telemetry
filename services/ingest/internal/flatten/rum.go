package flatten

import (
	"strconv"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/ingest/internal/norm"
	"github.com/tasktrooper/telemetry/services/pkg/rumpb"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// MaxClockSkew: a batch whose newest event is further than this in the future of received_at is shifted back (contracts §9).
const MaxClockSkew = 5 * time.Minute

// MaxOfflineAge: events queued offline (IndexedDB / disk) keep their own time up to this age; a batch whose newest
// event is older than that is assumed to come from a wrong client clock and is shifted.
const MaxOfflineAge = 24 * time.Hour

// RumRow is one row of telemetry.rum_events.
type RumRow struct {
	OrgID, ProjectID               uint32
	TS, ReceivedAt                 time.Time
	App, Env, AppVersion, Platform string
	Type                           string
	SessionID, ViewID, ViewName    string
	AnonymousID, UserID            string
	Country, DeviceType, OS        string
	Browser, Connection            string
	DurationMs                     float64
	VitalName                      string
	VitalValue                     float64
	ErrorType, ErrorMessage        string
	ErrorStack                     string
	ErrorFingerprint               uint64
	IsCrash                        uint8
	ResourceURL, ResourceMethod    string
	StatusCode                     uint16
	TraceID                        []byte
	ActionName                     string
	Rage, Dead                     uint8
	Name                           string
	Attrs                          map[string]string
}

// RumColumns is the INSERT column list matching RumRow.Values.
const RumColumns = "org_id, project_id, ts, received_at, app, env, app_version, platform, type, session_id, view_id, view_name, " +
	"anonymous_id, user_id, country, device_type, os, browser, connection, duration_ms, vital_name, vital_value, error_type, " +
	"error_message, error_stack, error_fingerprint, is_crash, resource_url, resource_method, status_code, trace_id, action_name, " +
	"rage, dead, name, attrs"

// Values returns the row in RumColumns order.
func (r *RumRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.TS, r.ReceivedAt, r.App, r.Env, r.AppVersion, r.Platform, r.Type, r.SessionID, r.ViewID,
		r.ViewName, r.AnonymousID, r.UserID, r.Country, r.DeviceType, r.OS, r.Browser, r.Connection, r.DurationMs, r.VitalName,
		r.VitalValue, r.ErrorType, r.ErrorMessage, r.ErrorStack, r.ErrorFingerprint, r.IsCrash, r.ResourceURL, r.ResourceMethod,
		r.StatusCode, r.TraceID, r.ActionName, r.Rage, r.Dead, r.Name, r.Attrs}
}

var platformNames = map[rumpb.Platform]string{
	rumpb.Platform_PLATFORM_WEB: "web", rumpb.Platform_PLATFORM_ANDROID: "android", rumpb.Platform_PLATFORM_IOS: "ios",
	rumpb.Platform_PLATFORM_FLUTTER: "flutter", rumpb.Platform_PLATFORM_REACT_NATIVE: "react_native",
}

var eventTypeNames = map[rumpb.EventType]string{
	rumpb.EventType_SESSION_START: "session_start", rumpb.EventType_VIEW_START: "view_start", rumpb.EventType_VIEW_END: "view_end",
	rumpb.EventType_VITAL: "vital", rumpb.EventType_ERROR: "error", rumpb.EventType_RESOURCE: "resource",
	rumpb.EventType_ACTION: "action", rumpb.EventType_APP_START: "app_start", rumpb.EventType_CUSTOM: "custom",
}

// RumMeta is what ingest-gw attached to the record.
type RumMeta struct {
	ReceivedAt time.Time
	UserAgent  string
	Country    string // from the GeoIP lookup of client_ip (writer); an event attribute geo.country wins
}

// Rum appends one RumRow per event in b.
func Rum(t tenant.Tenant, b *rumpb.RumBatch, meta RumMeta, out []RumRow) []RumRow {
	res := b.GetResource()
	received := meta.ReceivedAt.UTC().Truncate(time.Millisecond)
	shift := clockShift(b.GetEvents(), received)

	app := res.GetApp()
	if app == "" {
		app = "unknown_app"
	}
	platform := platformNames[res.GetPlatform()]
	device, osName, browser := res.GetDevice().GetType(), joinVer(res.GetOs().GetName(), res.GetOs().GetVersion()), joinVer(res.GetBrowser().GetName(), res.GetBrowser().GetVersion())
	if device == "" || osName == "" || (browser == "" && (platform == "web" || platform == "")) {
		ua := norm.ParseUA(meta.UserAgent)
		if device == "" {
			device = ua.DeviceType
		}
		if osName == "" {
			osName = joinVer(ua.OS, ua.OSVersion)
		}
		if browser == "" && (platform == "web" || platform == "") {
			browser = joinVer(ua.Browser, ua.BrowserVersion)
		}
	}
	if platform == "" {
		platform = "web"
	}

	resAttrs := res.GetAttributes()
	for _, ev := range b.GetEvents() {
		r := RumRow{
			OrgID: t.OrgID, ProjectID: t.ProjectID,
			ReceivedAt: received,
			App:        app, Env: res.GetEnvironment(), AppVersion: res.GetVersion(), Platform: platform,
			Type:      eventTypeNames[ev.GetType()],
			SessionID: ev.GetSessionId(), ViewID: ev.GetViewId(), ViewName: ev.GetViewName(),
			AnonymousID: ev.GetAnonymousId(), UserID: ev.GetUser().GetId(),
			Country: meta.Country, DeviceType: device, OS: osName, Browser: browser,
			Connection: ev.GetConnection(),
			TraceID:    fixedID(nil, 16),
		}
		if r.Type == "" {
			r.Type = "custom"
		}
		ts := received
		if ms := ev.GetTsMs(); ms > 0 {
			ts = time.UnixMilli(ms).UTC().Add(shift)
		}
		r.TS = ts

		attrs := make(map[string]string, len(ev.GetAttributes())+len(resAttrs)+4)
		for k, v := range resAttrs {
			putAttr(attrs, k, v, MaxAttrValueLen)
		}
		for k, v := range ev.GetAttributes() {
			putAttr(attrs, k, v, MaxAttrValueLen)
		}
		for k, v := range ev.GetUser().GetTraits() {
			putAttr(attrs, "user."+k, v, MaxAttrValueLen)
		}
		if s := res.GetSdk(); s != nil {
			putAttr(attrs, "sdk.name", s.GetName(), MaxAttrValueLen)
			putAttr(attrs, "sdk.version", s.GetVersion(), MaxAttrValueLen)
		}
		if d := res.GetDevice(); d != nil {
			putAttr(attrs, "device.model", strings.TrimSpace(d.GetManufacturer()+" "+d.GetModel()), MaxAttrValueLen)
		}

		switch p := ev.GetPayload().(type) {
		case *rumpb.RumEvent_View:
			v := p.View
			if r.Type == "view_end" {
				r.DurationMs = v.GetTimeSpentMs()
			} else {
				r.DurationMs = v.GetLoadingTimeMs()
			}
			if u := v.GetUrl(); u != "" {
				putAttr(attrs, "view.url", norm.StripQueryValues(u), MaxAttrValueLen)
				if r.ViewName == "" {
					r.ViewName = norm.Route(u)
				}
			}
			putAttr(attrs, "view.referrer", norm.StripQueryValues(v.GetReferrer()), MaxAttrValueLen)
			if v.GetIsInitial() {
				attrs["view.is_initial"] = "true"
			}
		case *rumpb.RumEvent_Vital:
			r.VitalName, r.VitalValue = strings.ToUpper(p.Vital.GetName()), p.Vital.GetValue()
			putAttr(attrs, "vital.rating", p.Vital.GetRating(), MaxAttrValueLen)
		case *rumpb.RumEvent_Error:
			e := p.Error
			r.ErrorType, r.ErrorMessage = e.GetType(), capValue(e.GetMessage(), 4096)
			r.ErrorStack = capValue(e.GetStack(), maxStackLen)
			if e.GetIsCrash() {
				r.IsCrash = 1
			}
			r.ErrorFingerprint = norm.ErrorFingerprint(app, e.GetType(), e.GetMessage(), e.GetStack())
			putAttr(attrs, "error.source", e.GetSource(), MaxAttrValueLen)
			putAttr(attrs, "error.thread", e.GetThread(), MaxAttrValueLen)
			putAttr(attrs, "error.client_fingerprint", e.GetFingerprint(), MaxAttrValueLen)
		case *rumpb.RumEvent_Resource:
			rs := p.Resource
			r.ResourceURL = norm.ResourceURL(rs.GetUrl())
			r.ResourceMethod = strings.ToUpper(rs.GetMethod())
			if sc := rs.GetStatusCode(); sc > 0 && sc < 65536 {
				r.StatusCode = uint16(sc)
			}
			r.DurationMs = rs.GetDurationMs()
			r.TraceID = hexID(rs.GetTraceId(), 16)
			putAttr(attrs, "resource.span_id", rs.GetSpanId(), MaxAttrValueLen)
			putAttr(attrs, "resource.initiator", rs.GetInitiator(), MaxAttrValueLen)
			if n := rs.GetRequestBytes(); n > 0 {
				attrs["resource.request_bytes"] = strconv.FormatInt(n, 10)
			}
			if n := rs.GetResponseBytes(); n > 0 {
				attrs["resource.response_bytes"] = strconv.FormatInt(n, 10)
			}
		case *rumpb.RumEvent_Action:
			a := p.Action
			r.ActionName = capValue(a.GetName(), MaxAttrValueLen)
			if a.GetRage() {
				r.Rage = 1
			}
			if a.GetDead() {
				r.Dead = 1
			}
			putAttr(attrs, "action.target", a.GetTarget(), MaxAttrValueLen)
		case *rumpb.RumEvent_AppStart:
			r.DurationMs = p.AppStart.GetDurationMs()
			putAttr(attrs, "app_start.kind", p.AppStart.GetKind(), MaxAttrValueLen)
		case *rumpb.RumEvent_Custom:
			c := p.Custom
			r.Name = c.GetName()
			r.DurationMs = c.GetValue()
			for k, v := range c.GetProps() {
				putAttr(attrs, k, v, MaxAttrValueLen)
			}
		}
		if !finite(r.DurationMs) || r.DurationMs < 0 {
			r.DurationMs = 0
		}
		if !finite(r.VitalValue) {
			r.VitalValue = 0
		}
		if c := attrs["geo.country"]; len(c) == 2 { // set by drains (Vercel) that know the end user's country
			r.Country = strings.ToUpper(c)
		}
		r.Attrs = attrs
		out = append(out, r)
	}
	return out
}

// clockShift returns the correction to apply to client timestamps. Only real clock skew is corrected: if the newest
// event of the batch is more than MaxClockSkew in the future, or more than MaxOfflineAge in the past, the whole batch
// is shifted so that event lands on received_at (spacing preserved). Older-but-plausible batches (offline queues,
// backfills ≤ 24 h) keep their own timestamps.
func clockShift(evs []*rumpb.RumEvent, received time.Time) time.Duration {
	var newest int64
	for _, ev := range evs {
		newest = max(newest, ev.GetTsMs())
	}
	if newest <= 0 {
		return 0
	}
	d := received.Sub(time.UnixMilli(newest)) // > 0: newest event is in the past
	if d < -MaxClockSkew || d > MaxOfflineAge {
		return d
	}
	return 0
}

func joinVer(name, ver string) string {
	switch {
	case name == "":
		return ""
	case ver == "":
		return name
	}
	return name + " " + ver
}
