package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
	"github.com/tasktrooper/telemetry/services/pkg/tenant"
)

func jsonUnmarshal(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func (s *Server) testConn(w http.ResponseWriter, r *http.Request, in model.ConnTestInput) {
	if in.Kind == "sqs" || in.Kind == "pubsub" {
		s.fail(w, r, store.Invalid(in.Kind+" has no direct connection test: connect it through a cloud integration (POST /integrations/test with provider aws|gcp)"))
		return
	}
	writeJSON(w, http.StatusOK, s.Test(r.Context(), in))
}

func (s *Server) infraRoutes(r chi.Router) {
	// ---- databases
	r.Get("/infra/databases", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ListDatabases(r.Context(), q)
		s.reply(w, r, v, err)
	}))
	r.Post("/infra/databases", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		var in model.DatabaseInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateDatabase(r.Context(), q, in)
		s.created(w, r, v, err)
	}))
	r.Post("/infra/databases/test-connection", func(w http.ResponseWriter, r *http.Request) {
		var in model.ConnTestInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		s.testConn(w, r, in)
	})
	r.Route("/infra/databases/{id}", func(r chi.Router) {
		r.Get("/", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.GetDatabase(r.Context(), q, param(r, "id"))
			s.reply(w, r, v, err)
		}))
		r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
			s.noContent(w, r, s.Store.DeleteDatabase(r.Context(), param(r, "id")))
		})
		r.Get("/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.DatabaseSeries(r.Context(), q, param(r, "id"), qs(r, "metric"), qs(r, "node"))
			s.reply(w, r, v, err)
		}))
		r.Get("/queries", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.DatabaseQueries(r.Context(), q, param(r, "id"))
			s.reply(w, r, v, err)
		}))
		r.Get("/users", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.DatabaseUsers(r.Context(), q, param(r, "id"))
			s.reply(w, r, v, err)
		}))
		// Tests the registered host/port; credentials come in the body (they are never stored, only credentials_ref).
		r.Post("/test-connection", func(w http.ResponseWriter, r *http.Request) {
			var in model.ConnTestInput
			if r.ContentLength != 0 {
				if err := decode(r, &in); err != nil {
					s.fail(w, r, err)
					return
				}
			}
			target, err := s.Store.DatabaseConn(r.Context(), param(r, "id"))
			if err != nil {
				s.fail(w, r, err)
				return
			}
			in.Kind, in.Host, in.Port = target.Kind, target.Host, target.Port
			if in.User == "" {
				in.User = target.User
			}
			s.testConn(w, r, in)
		})
	})

	// ---- brokers
	r.Get("/infra/clusters", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListClusters(r.Context(), s.Now())
		s.reply(w, r, v, err)
	})
	r.Post("/infra/clusters", func(w http.ResponseWriter, r *http.Request) {
		var in model.ClusterInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateCluster(r.Context(), s.Now(), in)
		s.created(w, r, v, err)
	})
	r.Post("/infra/clusters/test-connection", func(w http.ResponseWriter, r *http.Request) {
		var in model.ConnTestInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		s.testConn(w, r, in)
	})
	r.Get("/infra/topics", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListTopics(r.Context(), s.Now(), qs(r, "clusterId"))
		s.reply(w, r, v, err)
	})
	r.Get("/infra/groups", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListGroups(r.Context(), s.Now(), store.GroupFilter{ClusterID: qs(r, "clusterId"), ServiceID: qs(r, "serviceId")})
		s.reply(w, r, v, err)
	})
	r.Route("/infra/clusters/{id}", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			v, err := s.Store.GetCluster(r.Context(), s.Now(), param(r, "id"))
			s.reply(w, r, v, err)
		})
		r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
			s.noContent(w, r, s.Store.DeleteCluster(r.Context(), param(r, "id")))
		})
		r.Post("/test-connection", func(w http.ResponseWriter, r *http.Request) {
			var in model.ConnTestInput
			if r.ContentLength != 0 {
				if err := decode(r, &in); err != nil {
					s.fail(w, r, err)
					return
				}
			}
			kind, endpoint, vhost, err := s.Store.ClusterConn(r.Context(), param(r, "id"))
			if err != nil {
				s.fail(w, r, err)
				return
			}
			in.Kind, in.Endpoint, in.Vhost = kind, endpoint, vhost
			s.testConn(w, r, in)
		})
		r.Get("/topics", func(w http.ResponseWriter, r *http.Request) {
			v, err := s.Store.ListTopics(r.Context(), s.Now(), param(r, "id"))
			s.reply(w, r, v, err)
		})
		r.Get("/topics/{topic}/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.TopicSeries(r.Context(), q, param(r, "id"), param(r, "topic"))
			s.reply(w, r, v, err)
		}))
		r.Get("/groups", func(w http.ResponseWriter, r *http.Request) {
			v, err := s.Store.ListGroups(r.Context(), s.Now(), store.GroupFilter{ClusterID: param(r, "id")})
			s.reply(w, r, v, err)
		})
		r.Get("/groups/{group}", func(w http.ResponseWriter, r *http.Request) {
			v, err := s.Store.ListGroups(r.Context(), s.Now(), store.GroupFilter{ClusterID: param(r, "id"), Group: param(r, "group")})
			if err == nil && len(v) == 0 {
				err = store.ErrNotFound
			}
			if err != nil {
				s.fail(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, v[0])
		})
		r.Get("/groups/{group}/lag", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.GroupLagSeries(r.Context(), q, param(r, "id"), param(r, "group"), qs(r, "topic"), qs(r, "by") == "partition")
			s.reply(w, r, v, err)
		}))
		r.Put("/groups/{group}/owner", func(w http.ResponseWriter, r *http.Request) {
			var in model.GroupOwnerInput
			if err := decode(r, &in); err != nil {
				s.fail(w, r, err)
				return
			}
			s.noContent(w, r, s.Store.SetGroupOwner(r.Context(), param(r, "id"), param(r, "group"), in))
		})
		r.Get("/nodes/series", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
			v, err := s.Store.BrokerNodeSeries(r.Context(), q, param(r, "id"), qs(r, "metric"))
			s.reply(w, r, v, err)
		}))
	})

	// ---- collector
	r.Get("/internal/collector-config", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.CollectorConfig(r.Context())
		if err == nil {
			if t, ok := tenant.From(r.Context()); ok {
				v.OrgID, v.ProjectID = t.OrgID, t.ProjectID
			}
		}
		s.reply(w, r, v, err)
	})
}

func (s *Server) loadTestRoutes(r chi.Router) {
	r.Get("/loadtests/scenarios", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListScenarios(r.Context())
		s.reply(w, r, v, err)
	})
	r.Post("/loadtests/scenarios", func(w http.ResponseWriter, r *http.Request) {
		var in model.ScenarioInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.CreateScenario(r.Context(), in)
		s.created(w, r, v, err)
	})
	r.Get("/loadtests/scenarios/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetScenario(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Put("/loadtests/scenarios/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in model.ScenarioInput
		if err := decode(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		v, err := s.Store.UpdateScenario(r.Context(), param(r, "id"), in)
		s.reply(w, r, v, err)
	})
	r.Delete("/loadtests/scenarios/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.noContent(w, r, s.Store.DeleteScenario(r.Context(), param(r, "id")))
	})
	r.Post("/loadtests/scenarios/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.CreateRun(r.Context(), param(r, "id"))
		s.created(w, r, v, err)
	})
	r.Get("/loadtests/runs", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.ListRuns(r.Context(), qs(r, "scenarioId"))
		s.reply(w, r, v, err)
	})
	r.Get("/loadtests/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.GetRun(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
	r.Post("/loadtests/runs/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Store.StopRun(r.Context(), param(r, "id"))
		s.reply(w, r, v, err)
	})
}
