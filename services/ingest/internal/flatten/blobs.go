package flatten

import (
	"time"

	"github.com/tasktrooper/telemetry/services/ingest/internal/profiling"
	"github.com/tasktrooper/telemetry/services/ingest/internal/replay"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

// ── Session replay (contracts §23) ──

// ReplayRow is one row of telemetry.replay_segments.
type ReplayRow struct {
	OrgID, ProjectID              uint32
	SessionID                     string
	Segment                       uint32
	Service, Env, Version         string
	Platform, Format              string
	StartTS, EndTS                time.Time
	Events, Bytes                 uint32
	BlobKey                       string
	HasFullSnapshot               uint8
	Clicks, Errors, RageClicks    uint16
	ViewName, UserID, AnonymousID string
	ReceivedAt                    time.Time
}

// ReplayColumns is the INSERT column list matching ReplayRow.Values.
const ReplayColumns = "org_id, project_id, session_id, segment, service, env, version, platform, format, start_ts, end_ts, events, " +
	"bytes, blob_key, has_full_snapshot, clicks, errors, rage_clicks, view_name, user_id, anonymous_id, received_at"

// Values returns the row in ReplayColumns order.
func (r *ReplayRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.SessionID, r.Segment, r.Service, r.Env, r.Version, r.Platform, r.Format, r.StartTS, r.EndTS,
		r.Events, r.Bytes, r.BlobKey, r.HasFullSnapshot, r.Clicks, r.Errors, r.RageClicks, r.ViewName, r.UserID, r.AnonymousID, r.ReceivedAt}
}

// Replay builds the index row of a decoded segment whose gzip'ed blob has compressedBytes bytes.
func Replay(t tenant.Tenant, s *replay.Segment, compressedBytes int, received time.Time) ReplayRow {
	start, end := s.Window(received)
	r := ReplayRow{
		OrgID: t.OrgID, ProjectID: t.ProjectID,
		SessionID: s.SessionID, Segment: uint32(*s.Segment),
		Service: replay.Cap(s.Resource.Service), Env: replay.Cap(s.Resource.Env), Version: replay.Cap(s.Resource.Version),
		Platform: replay.Cap(s.Resource.Platform), Format: s.Format,
		StartTS: start, EndTS: end,
		Events: uint32(min(len(s.Events), 1<<31)), Bytes: uint32(min(compressedBytes, 1<<31)),
		BlobKey: replay.Key(t.OrgID, t.ProjectID, start, s.SessionID, *s.Segment),
		Clicks:  u16(s.Stats.Clicks), Errors: u16(s.Stats.Errors), RageClicks: u16(s.Stats.RageClicks),
		ViewName: replay.Cap(s.ViewName), UserID: replay.Cap(s.UserID), AnonymousID: replay.Cap(s.AnonymousID),
		ReceivedAt: received,
	}
	if r.Service == "" {
		r.Service = "unknown_app" // same default as RUM
	}
	if r.Platform == "" && s.Format == "rrweb" {
		r.Platform = "web"
	}
	if s.HasFullSnapshot {
		r.HasFullSnapshot = 1
	}
	return r
}

func u16(n int64) uint16 { return uint16(min(max(n, 0), 65535)) }

// ── Continuous profiling (contracts §24) ──

// ProfileMeta is the profile identity from the tt.profiles record headers.
type ProfileMeta struct {
	Service, Env, Version, Host, Runtime string
	Start, End                           time.Time
}

// ProfileStackRow is one row of telemetry.profile_stacks (stack_hash is computed by ClickHouse: DEFAULT cityHash64(stack)).
type ProfileStackRow struct {
	OrgID, ProjectID                     uint32
	TS                                   time.Time
	Service, Env, Version, Host, Runtime string
	Type, Unit, Route, Method            string
	Stack                                []string
	Value                                int64
}

// ProfileStackColumns is the INSERT column list matching ProfileStackRow.Values.
const ProfileStackColumns = "org_id, project_id, ts, service, env, version, host, runtime, type, unit, route, method, stack, value"

// Values returns the row in ProfileStackColumns order.
func (r *ProfileStackRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.TS, r.Service, r.Env, r.Version, r.Host, r.Runtime, r.Type, r.Unit, r.Route, r.Method, r.Stack, r.Value}
}

// ProfileRow is one row of telemetry.profiles.
type ProfileRow struct {
	OrgID, ProjectID                     uint32
	TS, EndTS                            time.Time
	Service, Env, Version, Host, Runtime string
	Type, Unit                           string
	Total                                int64
	Stacks                               uint32
}

// ProfileColumns is the INSERT column list matching ProfileRow.Values.
const ProfileColumns = "org_id, project_id, ts, end_ts, service, env, version, host, runtime, type, unit, total, stacks"

// Values returns the row in ProfileColumns order.
func (r *ProfileRow) Values() []any {
	return []any{r.OrgID, r.ProjectID, r.TS, r.EndTS, r.Service, r.Env, r.Version, r.Host, r.Runtime, r.Type, r.Unit, r.Total, r.Stacks}
}

// Profiles appends the rows of one parsed profile (one profiles row per type, one profile_stacks row per stack).
func Profiles(t tenant.Tenant, m ProfileMeta, series []profiling.Series, stacks []ProfileStackRow, profiles []ProfileRow) ([]ProfileStackRow, []ProfileRow) {
	ts := m.Start.UTC().Truncate(time.Second)
	for _, s := range series {
		profiles = append(profiles, ProfileRow{OrgID: t.OrgID, ProjectID: t.ProjectID, TS: m.Start.UTC(), EndTS: m.End.UTC(),
			Service: m.Service, Env: m.Env, Version: m.Version, Host: m.Host, Runtime: m.Runtime,
			Type: s.Type, Unit: s.Unit, Total: s.Total, Stacks: uint32(len(s.Stacks))})
		for _, st := range s.Stacks {
			stacks = append(stacks, ProfileStackRow{OrgID: t.OrgID, ProjectID: t.ProjectID, TS: ts,
				Service: m.Service, Env: m.Env, Version: m.Version, Host: m.Host, Runtime: m.Runtime,
				Type: s.Type, Unit: s.Unit, Route: st.Route, Method: st.Method, Stack: st.Frames, Value: st.Value})
		}
	}
	return stacks, profiles
}
