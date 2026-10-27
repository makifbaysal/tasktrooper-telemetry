package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/rules"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

// qh is a handler that needs the common query parameters.
type qh func(w http.ResponseWriter, r *http.Request, q store.Q)

func (s *Server) withQ(h qh) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := s.query(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r, q)
	}
}

// eh is a handler scoped to one endpoint/process of a service.
type eh func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef)

func (s *Server) withEndpoint(h eh) http.HandlerFunc {
	return s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		svc := chi.URLParam(r, "service")
		ep, err := s.Store.ResolveEndpoint(r.Context(), q, svc, chi.URLParam(r, "endpoint"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r, q, svc, ep, store.RefOf(ep))
	})
}

func param(r *http.Request, k string) string { return chi.URLParam(r, k) }

func qs(r *http.Request, k string) string { return r.URL.Query().Get(k) }

func qint(r *http.Request, k string) int {
	n, _ := strconv.Atoi(qs(r, k))
	return n
}

// qlist reads a repeatable / comma-separated parameter.
func qlist(r *http.Request, k string) []string {
	var out []string
	for _, v := range r.URL.Query()[k] {
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
	}
	return out
}

// refParam parses ?endpoint= ("METHOD /route" or a process name).
func refParam(r *http.Request) *store.EndpointRef {
	e := qs(r, "endpoint")
	if e == "" {
		return nil
	}
	if m, rt, ok := strings.Cut(e, " "); ok && strings.HasPrefix(rt, "/") {
		return &store.EndpointRef{Method: m, Route: rt}
	}
	return &store.EndpointRef{Route: e, Process: true}
}

func (s *Server) routes(r chi.Router) {
	// ---- APM
	r.Get("/overview/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.RedSeries(r.Context(), q, store.RedFilter{})
		s.reply(w, r, v, err)
	}))
	r.Get("/services", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ListServices(r.Context(), q)
		if err == nil && qs(r, "type") != "" {
			types := map[string]bool{}
			for _, t := range qlist(r, "type") {
				if t == "api" {
					t = "http"
				}
				types[t] = true
			}
			out := v[:0]
			for _, x := range v {
				if types[x.Type] {
					out = append(out, x)
				}
			}
			v = out
		}
		s.reply(w, r, v, err)
	}))
	r.Route("/services/{service}", func(r chi.Router) {
		r.Get("/", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.GetService(r.Context(), q, param(r, "service"))
			s.reply(w, r, v, err)
		}))
		r.Get("/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.RedSeries(r.Context(), q, store.RedFilter{Service: param(r, "service")})
			s.reply(w, r, v, err)
		}))
		r.Get("/endpoints", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.ListEndpoints(r.Context(), q, param(r, "service"))
			s.reply(w, r, v, err)
		}))
		r.Get("/callers", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.Callers(r.Context(), q, param(r, "service"), nil)
			s.reply(w, r, v, err)
		}))
		r.Get("/callers/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.CallerSeries(r.Context(), q, param(r, "service"), nil)
			s.reply(w, r, v, err)
		}))
		r.Get("/dependencies", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.Dependencies(r.Context(), q, param(r, "service"))
			s.reply(w, r, v, err)
		}))
		r.Get("/dependencies/{dep}/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.DependencySeries(r.Context(), q, param(r, "service"), param(r, "dep"))
			s.reply(w, r, v, err)
		}))
		r.Get("/breakdown", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.Breakdown(r.Context(), q, param(r, "service"), nil)
			s.reply(w, r, v, err)
		}))
		r.Get("/breakdown/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.BreakdownSeries(r.Context(), q, param(r, "service"), nil)
			s.reply(w, r, v, err)
		}))
		r.Get("/deploys", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.ListDeploys(r.Context(), q, store.DeployFilter{Service: param(r, "service"), Source: qs(r, "source")})
			s.reply(w, r, v, err)
		}))
		r.Route("/endpoints/{endpoint}", func(r chi.Router) {
			r.Get("/", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				writeJSON(w, http.StatusOK, ep)
			}))
			r.Get("/series", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				v, err := s.Store.RedSeries(r.Context(), q, store.RedFilter{Service: svc, Ref: &ref})
				s.reply(w, r, v, err)
			}))
			r.Get("/callers", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				v, err := s.Store.Callers(r.Context(), q, svc, &ref)
				s.reply(w, r, v, err)
			}))
			r.Get("/callers/series", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				v, err := s.Store.CallerSeries(r.Context(), q, svc, &ref)
				s.reply(w, r, v, err)
			}))
			r.Get("/calls", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				v, err := s.Store.EndpointCalls(r.Context(), q, svc, ref)
				s.reply(w, r, v, err)
			}))
			r.Get("/breakdown", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				v, err := s.Store.Breakdown(r.Context(), q, svc, &ref)
				s.reply(w, r, v, err)
			}))
			r.Get("/breakdown/series", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				v, err := s.Store.BreakdownSeries(r.Context(), q, svc, &ref)
				s.reply(w, r, v, err)
			}))
			r.Get("/rca", s.withEndpoint(func(w http.ResponseWriter, r *http.Request, q store.Q, svc string, ep *model.Endpoint, ref store.EndpointRef) {
				v, err := s.Store.EndpointRca(r.Context(), q, svc, ref, qs(r, "baseline"))
				s.reply(w, r, v, err)
			}))
		})
	})
	r.Get("/deploys", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ListDeploys(r.Context(), q, store.DeployFilter{Service: qs(r, "service"), Source: qs(r, "source")})
		s.reply(w, r, v, err)
	}))
	r.Get("/traces", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		minMs, _ := strconv.ParseFloat(qs(r, "minMs"), 64)
		v, err := s.Store.SearchTraces(r.Context(), q, store.TraceFilter{
			Service: qs(r, "service"), Ref: refParam(r), ErrorsOnly: qs(r, "errors") == "true", MinMs: minMs,
			Slowest: qs(r, "sort") == "slowest", Limit: qint(r, "limit"),
		})
		s.reply(w, r, v, err)
	}))
	r.Get("/traces/{traceId}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetTrace(r.Context(), strings.ToLower(param(r, "traceId")))
		s.reply(w, r, v, err)
	})
	r.Get("/errors", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ListErrors(r.Context(), q, store.ErrorFilter{Service: qs(r, "service"), Route: qs(r, "route"), Status: qs(r, "status"), Limit: qint(r, "limit")})
		s.reply(w, r, v, err)
	}))
	r.Get("/errors/{fingerprint}", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.GetError(r.Context(), q, param(r, "fingerprint"))
		if err == nil {
			v.Stack, v.Frames, v.Symbolicated = s.symbolicate(r, v.Service, v.Release, v.Platform, v.Stack)
		}
		s.reply(w, r, v, err)
	}))
	r.Patch("/errors/{fingerprint}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		if err := decode(r, &body); err != nil {
			s.fail(w, r, err)
			return
		}
		err := s.Store.SetErrorStatus(r.Context(), param(r, "fingerprint"), body.Status)
		s.reply(w, r, map[string]string{"fingerprint": param(r, "fingerprint"), "status": body.Status}, err)
	})

	// ---- logs
	logFilter := func(r *http.Request) store.LogFilter {
		before, _ := strconv.ParseInt(qs(r, "before"), 10, 64)
		return store.LogFilter{Services: qlist(r, "service"), Levels: qlist(r, "level"), Text: qs(r, "q"), TraceID: strings.ToLower(qs(r, "traceId")),
			Route: qs(r, "route"), Source: qs(r, "source"), Before: before, Limit: qint(r, "limit")}
	}
	r.Get("/logs", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.SearchLogs(r.Context(), q, logFilter(r))
		s.reply(w, r, v, err)
	}))
	r.Get("/logs/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.LogSeries(r.Context(), q, logFilter(r))
		s.reply(w, r, v, err)
	}))
	r.Get("/logs/patterns", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.LogPatterns(r.Context(), q, logFilter(r))
		s.reply(w, r, v, err)
	}))

	// ---- RUM
	r.Route("/apps/{app}", func(r chi.Router) {
		app := func(r *http.Request) string { return param(r, "app") }
		r.Get("/summary", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.AppSummary(r.Context(), q, app(r))
			s.reply(w, r, v, err)
		}))
		r.Get("/pages", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.RumPages(r.Context(), q, app(r))
			s.reply(w, r, v, err)
		}))
		r.Get("/sessions", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.RumSessions(r.Context(), q, app(r), qint(r, "limit"))
			s.reply(w, r, v, err)
		}))
		r.Get("/errors", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.RumErrors(r.Context(), q, app(r))
			s.reply(w, r, v, err)
		}))
		r.Get("/errors/{fingerprint}", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.GetRumError(r.Context(), q, app(r), param(r, "fingerprint"))
			if err == nil {
				v.Stack, v.Frames, v.Symbolicated = s.symbolicate(r, v.App, v.AppVersion, v.Platform, v.Stack)
			}
			s.reply(w, r, v, err)
		}))
		r.Get("/resources", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.RumResources(r.Context(), q, app(r))
			s.reply(w, r, v, err)
		}))
		r.Get("/app-starts", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.AppStarts(r.Context(), q, app(r))
			s.reply(w, r, v, err)
		}))
		r.Get("/api-calls", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.AppAPICalls(r.Context(), q, app(r))
			s.reply(w, r, v, err)
		}))
		r.Get("/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.RumSeries(r.Context(), q, app(r), qs(r, "view"))
			s.reply(w, r, v, err)
		}))
		r.Get("/vitals/{vital}/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.VitalSeries(r.Context(), q, app(r), param(r, "vital"), qs(r, "view"))
			s.reply(w, r, v, err)
		}))
	})

	// ---- monitors & incidents
	r.Get("/monitors", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListMonitors(r.Context(), store.MonitorFilter{Service: qs(r, "service"), Endpoint: qs(r, "endpoint"), Kind: qs(r, "kind"),
			ClusterID: qs(r, "clusterId"), Group: qs(r, "group")})
		s.reply(w, r, v, err)
	})
	r.Post("/monitors", func(w http.ResponseWriter, r *http.Request) {
		var in model.MonitorInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateMonitor(r.Context(), in)
		s.created(w, r, v, err)
	})
	r.Post("/monitors/preview", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		var in model.MonitorInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.preview(r, q, in)
		s.reply(w, r, v, err)
	}))
	r.Get("/monitors/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetMonitor(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Put("/monitors/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in model.MonitorInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.UpdateMonitor(r.Context(), param(r, "id"), in)
		s.reply(w, r, v, err)
	})
	r.Patch("/monitors/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := decodeMonitorPatch(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.PatchMonitor(r.Context(), param(r, "id"), p)
		s.reply(w, r, v, err)
	})
	r.Delete("/monitors/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.noContent(w, r, s.Store.DeleteMonitor(r.Context(), param(r, "id")))
	})
	r.Get("/monitors/{id}/preview", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		m, err := s.Store.GetMonitor(r.Context(), param(r, "id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.preview(r, q, model.MonitorInput{Kind: m.Kind, Target: m.TargetSpec, Query: m.Query, Condition: m.Rule, Queue: m.Queue})
		s.reply(w, r, v, err)
	}))
	r.Get("/incidents", func(w http.ResponseWriter, r *http.Request) {
		var since time.Time
		if ms, err := strconv.ParseInt(qs(r, "since"), 10, 64); err == nil && ms > 0 {
			since = time.UnixMilli(ms)
		}
		v, err := s.Store.ListIncidents(r.Context(), store.IncidentFilter{State: qs(r, "state"), Service: qs(r, "service"), Endpoint: qs(r, "endpoint"),
			MonitorID: qs(r, "monitorId"), KpiID: qs(r, "kpiId"), Since: since, Limit: qint(r, "limit"), IncludeQuiet: qs(r, "includeQuiet") == "true"})
		s.reply(w, r, v, err)
	})
	r.Get("/incidents/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetIncident(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Post("/incidents/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.TransitionIncident(r.Context(), param(r, "id"), "ack")
		s.reply(w, r, v, err)
	})
	r.Post("/incidents/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.TransitionIncident(r.Context(), param(r, "id"), "resolve")
		s.reply(w, r, v, err)
	})
	r.Get("/incidents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.IncidentEvents(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})

	// ---- KPIs
	r.Get("/kpis", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListKpis(r.Context(), s.Now(), store.KpiFilter{ServiceID: qs(r, "serviceId")})
		s.reply(w, r, v, err)
	})
	r.Post("/kpis", func(w http.ResponseWriter, r *http.Request) {
		var in model.KpiInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateKpi(r.Context(), s.Now(), in)
		s.created(w, r, v, err)
	})
	r.Get("/kpis/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetKpi(r.Context(), s.Now(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Put("/kpis/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in model.KpiInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.UpdateKpi(r.Context(), s.Now(), param(r, "id"), in)
		s.reply(w, r, v, err)
	})
	r.Delete("/kpis/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.noContent(w, r, s.Store.DeleteKpi(r.Context(), param(r, "id")))
	})
	r.Get("/kpis/{id}/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.KpiSeries(r.Context(), q, param(r, "id"))
		s.reply(w, r, v, err)
	}))

	s.infraRoutes(r)
	s.phase6Routes(r)
	s.loadTestRoutes(r)
	s.adminRoutes(r)
	s.codeInsightRoutes(r)
}

// preview replays a monitor rule over the range: the series it compares, its threshold and how many times it
// would have fired (same shape as evaluate() in apps/web/src/lib/lagAlerts.ts).
func (s *Server) preview(r *http.Request, q store.Q, in model.MonitorInput) (model.Preview, error) {
	stepMin := q.W.StepMinutes()
	out := model.Preview{Timestamps: q.W.Timestamps()}
	var data []float64
	var ev rules.Evaluation
	switch in.Kind {
	case "consumer_lag":
		if in.Queue == nil || !rules.ValidLagMetric(in.Queue.Metric) {
			return out, store.Invalid("consumer_lag preview needs a queue rule with a valid metric")
		}
		lag, err := s.Store.LagInputs(r.Context(), q.W, *in.Queue)
		if err != nil {
			return out, err
		}
		data = rules.RuleSeries(in.Queue.Metric, lag, stepMin)
		ev = rules.Evaluate(data, rules.Threshold(*in.Queue), in.Queue.ForMin, stepMin)
	case "metric_threshold", "latency", "error_rate", "throughput":
		if in.Target.Service == "" {
			return out, store.Invalid("target.service is required")
		}
		// query shapes: services/alerting/README.md ("Monitor query shapes")
		var meta struct {
			Metric     string `json:"metric"`
			Percentile string `json:"percentile"`
		}
		_ = jsonUnmarshal(in.Query, &meta)
		metric := meta.Metric
		switch {
		case in.Kind == "latency":
			metric = meta.Percentile
			if metric == "" {
				metric = "p95"
			}
		case in.Kind == "error_rate":
			metric = "error_rate"
		case in.Kind == "throughput":
			metric = "rpm"
		case metric == "":
			metric = "error_rate"
		}
		id := map[string]string{"error_rate": "errorRate", "throughput": "rpm", "rpm": "rpm", "p50": "p50", "p95": "p95", "p99": "p99"}[metric]
		if id == "" {
			return out, store.Invalid("query.metric must be error_rate|throughput|p50|p95|p99")
		}
		f := store.RedFilter{Service: in.Target.Service}
		if in.Target.Endpoint != "" {
			m, rt, _ := strings.Cut(in.Target.Endpoint, " ")
			f.Ref = &store.EndpointRef{Method: m, Route: rt}
		} else if in.Target.Process != "" {
			f.Ref = &store.EndpointRef{Route: in.Target.Process, Process: true}
		}
		set, err := s.Store.RedSeries(r.Context(), q, f)
		if err != nil {
			return out, err
		}
		var rpm []float64
		for _, sr := range set.Series {
			if sr.ID == id {
				data = sr.Data
			}
			if sr.ID == "rpm" {
				rpm = sr.Data
			}
		}
		if in.Condition.MinRequests > 0 && rpm != nil { // low traffic buckets never breach
			data = append([]float64(nil), data...)
			for i := range data {
				if rpm[i]*stepMin < in.Condition.MinRequests {
					data[i] = 0
				}
			}
		}
		ev = rules.EvaluateOp(data, in.Condition.Op, in.Condition.Threshold, in.Condition.ForMin, stepMin)
	case "log_count", "log_match", "log_pattern", "log_pattern_new", "dlq", "consumer_failures", "rum_vital", "crash_rate":
		vals, volume, err := s.Store.PreviewSeries(r.Context(), q, in.Kind, in.Target, in.Query, in.Condition)
		if err != nil {
			return out, err
		}
		data = vals
		judged := data
		if in.Condition.MinRequests > 0 && volume != nil { // engine: low volume never breaches (the chart keeps the real value)
			judged = append([]float64(nil), data...)
			for i := range judged {
				if volume[i] < in.Condition.MinRequests {
					judged[i] = nonBreaching(in.Condition.Op, in.Condition.Threshold)
				}
			}
		}
		ev = rules.EvaluateOp(judged, in.Condition.Op, in.Condition.Threshold, in.Condition.ForMin, stepMin)
		ev.Now = data[len(data)-1]
	default:
		return out, store.Invalid("preview supports consumer_lag, metric_threshold, latency, error_rate, throughput, log_count, log_match, " +
			"log_pattern, log_pattern_new, dlq, consumer_failures, rum_vital and crash_rate monitors")
	}
	if data == nil {
		data = make([]float64, q.W.Points)
	}
	out.Data, out.Th, out.Episodes, out.FiringNow, out.Now = data, ev.Th, ev.Episodes, ev.FiringNow, ev.Now
	return out, nil
}
