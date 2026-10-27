// Package server is the api's HTTP layer: routing, auth, middleware and handlers. Handlers only parse
// parameters and call Store, so they are tested with a fake store.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tasktrooper/telemetry/services/api/internal/auth"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/rules"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/api/internal/symbolicate"
	"github.com/tasktrooper/telemetry/services/api/internal/timerange"
)

// Store is everything the handlers need; *store.DB implements it.
type Store interface {
	// APM
	ListServices(ctx context.Context, q store.Q) ([]model.Service, error)
	GetService(ctx context.Context, q store.Q, id string) (*model.Service, error)
	RedSeries(ctx context.Context, q store.Q, f store.RedFilter) (model.SeriesSet, error)
	ListEndpoints(ctx context.Context, q store.Q, service string) ([]model.Endpoint, error)
	ResolveEndpoint(ctx context.Context, q store.Q, service, id string) (*model.Endpoint, error)
	Callers(ctx context.Context, q store.Q, service string, ref *store.EndpointRef) ([]model.CallerTraffic, error)
	CallerSeries(ctx context.Context, q store.Q, service string, ref *store.EndpointRef) (model.SeriesSet, error)
	Dependencies(ctx context.Context, q store.Q, service string) ([]model.Dependency, error)
	DependencySeries(ctx context.Context, q store.Q, service, dep string) (model.SeriesSet, error)
	EndpointCalls(ctx context.Context, q store.Q, service string, ref store.EndpointRef) ([]model.EndpointCall, error)
	Breakdown(ctx context.Context, q store.Q, service string, ref *store.EndpointRef) (model.TimeBreakdown, error)
	BreakdownSeries(ctx context.Context, q store.Q, service string, ref *store.EndpointRef) (model.SeriesSet, error)
	EndpointRca(ctx context.Context, q store.Q, service string, ref store.EndpointRef, baseline string) (*model.EndpointRca, error)
	SearchTraces(ctx context.Context, q store.Q, f store.TraceFilter) ([]model.TraceSummary, error)
	GetTrace(ctx context.Context, id string) (*model.Trace, error)
	ListErrors(ctx context.Context, q store.Q, f store.ErrorFilter) ([]model.ErrorIssue, error)
	SetErrorStatus(ctx context.Context, fingerprint, status string) error
	GetError(ctx context.Context, q store.Q, fingerprint string) (*model.ErrorDetail, error)
	ListDeploys(ctx context.Context, q store.Q, f store.DeployFilter) ([]model.DeployMarker, error)
	// logs
	SearchLogs(ctx context.Context, q store.Q, f store.LogFilter) (model.LogPage, error)
	LogSeries(ctx context.Context, q store.Q, f store.LogFilter) (model.SeriesSet, error)
	LogPatterns(ctx context.Context, q store.Q, f store.LogFilter) ([]model.LogPattern, error)
	// RUM
	AppSummary(ctx context.Context, q store.Q, app string) (model.AppSummary, error)
	RumPages(ctx context.Context, q store.Q, app string) ([]model.RumPage, error)
	RumSessions(ctx context.Context, q store.Q, app string, limit int) ([]model.Session, error)
	RumErrors(ctx context.Context, q store.Q, app string) ([]model.RumError, error)
	GetRumError(ctx context.Context, q store.Q, app, fingerprint string) (*model.RumErrorDetail, error)
	RumResources(ctx context.Context, q store.Q, app string) ([]model.RumResource, error)
	AppStarts(ctx context.Context, q store.Q, app string) ([]model.AppStart, error)
	AppAPICalls(ctx context.Context, q store.Q, app string) ([]model.AppAPICall, error)
	RumSeries(ctx context.Context, q store.Q, app, view string) (model.SeriesSet, error)
	VitalSeries(ctx context.Context, q store.Q, app, vital, view string) (model.SeriesSet, error)
	// alerting
	ListMonitors(ctx context.Context, f store.MonitorFilter) ([]model.Monitor, error)
	GetMonitor(ctx context.Context, id string) (*model.Monitor, error)
	CreateMonitor(ctx context.Context, in model.MonitorInput) (*model.Monitor, error)
	UpdateMonitor(ctx context.Context, id string, in model.MonitorInput) (*model.Monitor, error)
	PatchMonitor(ctx context.Context, id string, p model.MonitorPatch) (*model.Monitor, error)
	DeleteMonitor(ctx context.Context, id string) error
	LagInputs(ctx context.Context, w timerange.Window, r model.QueueRule) (rules.LagInputs, error)
	PreviewSeries(ctx context.Context, q store.Q, kind string, t model.MonitorTarget, query json.RawMessage, c model.MonitorCondition) (data, volume []float64, err error)
	ListMaintenanceWindows(ctx context.Context, now time.Time, f store.MaintenanceFilter) ([]model.MaintenanceWindow, error)
	GetMaintenanceWindow(ctx context.Context, now time.Time, id string) (*model.MaintenanceWindow, error)
	CreateMaintenanceWindow(ctx context.Context, now time.Time, in model.MaintenanceWindowInput) (*model.MaintenanceWindow, error)
	UpdateMaintenanceWindow(ctx context.Context, now time.Time, id string, in model.MaintenanceWindowInput) (*model.MaintenanceWindow, error)
	DeleteMaintenanceWindow(ctx context.Context, id string) error
	ListNotificationChannels(ctx context.Context) ([]model.NotificationChannel, error)
	GetNotificationChannel(ctx context.Context, id string) (*model.NotificationChannel, error)
	CreateNotificationChannel(ctx context.Context, in model.NotificationChannelInput) (*model.NotificationChannel, error)
	UpdateNotificationChannel(ctx context.Context, id string, in model.NotificationChannelInput) (*model.NotificationChannel, error)
	DeleteNotificationChannel(ctx context.Context, id string) error
	ListIncidents(ctx context.Context, f store.IncidentFilter) ([]model.Incident, error)
	GetIncident(ctx context.Context, id string) (*model.Incident, error)
	TransitionIncident(ctx context.Context, id, action string) (*model.Incident, error)
	IncidentEvents(ctx context.Context, id string) ([]model.IncidentEvent, error)
	// KPIs
	ListKpis(ctx context.Context, now time.Time, f store.KpiFilter) ([]model.Kpi, error)
	GetKpi(ctx context.Context, now time.Time, id string) (*model.Kpi, error)
	CreateKpi(ctx context.Context, now time.Time, in model.KpiInput) (*model.Kpi, error)
	UpdateKpi(ctx context.Context, now time.Time, id string, in model.KpiInput) (*model.Kpi, error)
	DeleteKpi(ctx context.Context, id string) error
	KpiSeries(ctx context.Context, q store.Q, id string) (model.SeriesSet, error)
	// infrastructure
	ListDatabases(ctx context.Context, q store.Q) ([]model.Database, error)
	GetDatabase(ctx context.Context, q store.Q, id string) (*model.Database, error)
	CreateDatabase(ctx context.Context, q store.Q, in model.DatabaseInput) (*model.Database, error)
	DeleteDatabase(ctx context.Context, id string) error
	DatabaseConn(ctx context.Context, id string) (model.ConnTestInput, error)
	DatabaseSeries(ctx context.Context, q store.Q, id, metric, node string) (model.SeriesSet, error)
	DatabaseQueries(ctx context.Context, q store.Q, id string) ([]model.DbQueryRow, error)
	DatabaseUsers(ctx context.Context, q store.Q, id string) ([]model.DbUser, error)
	ListClusters(ctx context.Context, now time.Time) ([]model.Cluster, error)
	GetCluster(ctx context.Context, now time.Time, id string) (*model.Cluster, error)
	CreateCluster(ctx context.Context, now time.Time, in model.ClusterInput) (*model.Cluster, error)
	DeleteCluster(ctx context.Context, id string) error
	ClusterConn(ctx context.Context, id string) (kind, endpoint, vhost string, err error)
	ListTopics(ctx context.Context, now time.Time, cluster string) ([]model.Topic, error)
	ListGroups(ctx context.Context, now time.Time, f store.GroupFilter) ([]model.ConsumerGroup, error)
	SetGroupOwner(ctx context.Context, cluster, group string, in model.GroupOwnerInput) error
	GroupLagSeries(ctx context.Context, q store.Q, cluster, group, topic string, byPartition bool) (model.SeriesSet, error)
	TopicSeries(ctx context.Context, q store.Q, cluster, topic string) (model.SeriesSet, error)
	BrokerNodeSeries(ctx context.Context, q store.Q, cluster, metric string) (model.SeriesSet, error)
	CollectorConfig(ctx context.Context) (model.CollectorConfig, error)
	// integrations (ADR-012)
	ListIntegrations(ctx context.Context) ([]model.Integration, error)
	GetIntegration(ctx context.Context, id string) (*model.Integration, error)
	CreateIntegration(ctx context.Context, in model.IntegrationInput) (*model.Integration, error)
	UpdateIntegration(ctx context.Context, id string, p model.IntegrationPatch) (*model.Integration, error)
	DeleteIntegration(ctx context.Context, id string) error
	RequestIntegrationSync(ctx context.Context, id string, now time.Time) (*model.Integration, error)
	// load tests
	ListScenarios(ctx context.Context) ([]model.LoadTestScenario, error)
	GetScenario(ctx context.Context, id string) (*model.LoadTestScenario, error)
	CreateScenario(ctx context.Context, in model.ScenarioInput) (*model.LoadTestScenario, error)
	UpdateScenario(ctx context.Context, id string, in model.ScenarioInput) (*model.LoadTestScenario, error)
	DeleteScenario(ctx context.Context, id string) error
	ListRuns(ctx context.Context, scenarioID string) ([]model.LoadTestRun, error)
	GetRun(ctx context.Context, id string) (*model.LoadTestRun, error)
	SetScenarioState(ctx context.Context, id, state string) (*model.LoadTestScenario, error)
	CreateRun(ctx context.Context, scenarioID string, requestedBy int64) (*model.LoadTestRun, error)
	StopRun(ctx context.Context, id string) (*model.LoadTestRun, error)
	RunSeries(ctx context.Context, id string, step time.Duration, now time.Time) (model.RunSeries, error)
	RunTarget(ctx context.Context, id, service string, now time.Time) (model.RunTarget, error)
	ListDomains(ctx context.Context) ([]model.LoadTestDomain, error)
	CreateDomain(ctx context.Context, in model.LoadTestDomainInput) (*model.LoadTestDomain, error)
	DeleteDomain(ctx context.Context, id string) error
	RequestDomainVerify(ctx context.Context, id, method string) (*model.LoadTestDomain, error)
	// code intelligence
	ListRepos(ctx context.Context) ([]model.Repo, error)
	GetRepo(ctx context.Context, id string) (*model.Repo, error)
	CreateRepo(ctx context.Context, in model.RepoInput) (*model.Repo, error)
	DeleteRepo(ctx context.Context, id string) error
	AnalyzeRepo(ctx context.Context, id string) (*model.Repo, error)
	ListInventory(ctx context.Context, f store.InventoryFilter) ([]model.InventoryItem, error)
	ListSuggestions(ctx context.Context, state string) ([]model.CodeSuggestion, error)
	AcceptSuggestion(ctx context.Context, id string, o model.SuggestionAccept) (*model.SuggestionAccepted, error)
	RejectSuggestion(ctx context.Context, id string) (*model.CodeSuggestion, error)

	IdentityStore
	ChatOpsStore
}

var _ Store = (*store.DB)(nil)

// ConnTester runs connection tests (conntest.Run in production).
type ConnTester func(ctx context.Context, in model.ConnTestInput) model.ConnTestResult

// CloudTester checks cloud credentials (cloudcheck.Run in production).
type CloudTester func(ctx context.Context, provider string, config json.RawMessage, secret string) model.IntegrationTestResult

// Server holds dependencies.
type Server struct {
	Store   Store
	Auth    auth.Resolver
	Log     *slog.Logger
	Origins []string
	Test    ConnTester
	// CloudTest backs POST /integrations/test (nil → 501).
	CloudTest CloudTester
	// Symbolicator resolves error stacks (TT_SYMBOLICATOR_URL); nil = raw stacks.
	Symbolicator *symbolicate.Client
	Now          func() time.Time
	// Ready reports dependency health for /healthz (nil = always ok).
	Ready func(ctx context.Context) error

	// OIDC enables /auth/login + /auth/callback (nil = off, TT_OIDC_ISSUER empty).
	OIDC *auth.OIDC
	// DevLogin enables POST /auth/dev-login (TT_AUTH_DEV=true).
	DevLogin bool
	// SessionTTL is the sliding session lifetime (default 7 days).
	SessionTTL time.Duration
	// WebURL is the dashboard base URL login redirects go to (TT_WEB_URL, default http://localhost:5173).
	WebURL string
	// CookieSecure forces Secure cookies (they are Secure whenever the request is https anyway).
	CookieSecure bool

	// Slack / GitHub enable the App integrations (nil = 501 on their endpoints).
	Slack  *SlackApp
	GitHub *GitHubApp
	// PublicAPIURL is this api as reachable by browsers and Slack (OAuth redirect URIs; TT_PUBLIC_API_URL).
	PublicAPIURL string
	// StateKey signs install states (derived from TT_SECRETS_KEY).
	StateKey []byte

	channels channelCache
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.recoverer, s.requestLog, s.cors, middleware.Compress(5, "application/json"))
	r.Get("/healthz", s.healthz)
	r.Route("/api/v1", func(r chi.Router) {
		s.authRoutes(r)          // anonymous
		s.chatOpsPublicRoutes(r) // signed state / Slack signature
		r.Group(func(r chi.Router) {
			r.Use(s.authenticate, s.guard)
			s.identityRoutes(r)
			s.chatOpsRoutes(r)
			s.routes(r)
		})
	})
	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Ready(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------- middleware

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.Error("panic", "err", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, apiError{Error: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		lvl := slog.LevelInfo
		if ww.Status() >= 500 {
			lvl = slog.LevelError
		}
		s.Log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"bytes", ww.BytesWritten(), "ms", float64(time.Since(start).Microseconds())/1000, "request_id", middleware.GetReqID(r.Context()))
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && OriginAllowed(s.Origins, o) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", o)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true") // tt_session cookie; the origin is echoed exactly, never "*"
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+projectHeader)
			h.Set("Access-Control-Max-Age", "600")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// OriginAllowed matches an Origin against the allow list: exact entries, "*", or a "scheme://host:*" entry that
// accepts any port on that host (the dev default allows every localhost port the UI dev servers pick).
func OriginAllowed(allowed []string, origin string) bool {
	for _, a := range allowed {
		if a == "*" || a == origin {
			return true
		}
		if base, ok := strings.CutSuffix(a, ":*"); ok {
			if origin == base {
				return true
			}
			if port, ok := strings.CutPrefix(origin, base+":"); ok && port != "" && strings.Trim(port, "0123456789") == "" {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- helpers

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// Marshal before writing the header: a NaN/Inf float would otherwise leave a 200 with an empty body.
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("response encode failed", "err", err)
		status = http.StatusInternalServerError
		b, _ = json.Marshal(apiError{Error: "response encode failed: " + err.Error()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// fail maps store errors to HTTP statuses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var inv store.ErrInvalid
	var conflict store.ConflictMsg
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, apiError{Error: conflict.Msg})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, apiError{Error: "not found"})
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, apiError{Error: inv.Msg})
	case errors.Is(err, store.ErrConflict):
		writeJSON(w, http.StatusConflict, apiError{Error: "conflict with the current state"})
	case errors.Is(err, context.Canceled):
		w.WriteHeader(499)
	default:
		s.Log.Error("request failed", "path", r.URL.Path, "err", err, "request_id", middleware.GetReqID(r.Context()))
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "internal error"})
	}
}

// reply writes v or the error.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) created(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) noContent(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return store.Invalid("invalid JSON body: " + err.Error())
	}
	return nil
}

// query parses the common range/env parameters.
func (s *Server) query(r *http.Request) (store.Q, error) {
	rg, err := timerange.Parse(r.URL.Query().Get("range"))
	if err != nil {
		return store.Q{}, store.Invalid(err.Error())
	}
	now := s.Now()
	return store.Q{W: rg.Window(now), Env: r.URL.Query().Get("env"), Now: now}, nil
}
