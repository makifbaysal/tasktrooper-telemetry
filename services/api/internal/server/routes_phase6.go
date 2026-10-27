package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tasktrooper/telemetry/services/api/internal/model"
	"github.com/tasktrooper/telemetry/services/api/internal/store"
)

// Phase6Store: hosts / containers / Kubernetes (ADR-019, contracts §20) and serverless (ADR-020, §21).
type Phase6Store interface {
	ListHosts(ctx context.Context, q store.Q, f store.InfraFilter) ([]model.Host, error)
	GetHost(ctx context.Context, q store.Q, host string) (*model.HostDetail, error)
	ListContainers(ctx context.Context, q store.Q, f store.InfraFilter) ([]model.Container, error)
	ListK8sClusters(ctx context.Context, q store.Q) ([]model.K8sCluster, error)
	GetK8sCluster(ctx context.Context, q store.Q, name string) (*model.K8sClusterDetail, error)
	ListWorkloads(ctx context.Context, q store.Q, f store.InfraFilter) ([]model.K8sWorkload, error)
	ListPods(ctx context.Context, q store.Q, f store.InfraFilter) ([]model.Pod, error)
	GetPod(ctx context.Context, q store.Q, uid string) (*model.PodDetail, error)
	ListK8sEvents(ctx context.Context, q store.Q, f store.InfraFilter) ([]model.K8sEvent, error)
	ServiceInfra(ctx context.Context, q store.Q, svc string) (*model.ServiceInfra, error)
	ServerlessSummary(ctx context.Context, q store.Q) (*model.ServerlessSummary, error)
	ListFunctions(ctx context.Context, q store.Q, f store.ServerlessFilter) ([]model.ServerlessFunction, error)
	GetFunction(ctx context.Context, q store.Q, id string) (*model.ServerlessFunctionDetail, error)
}

func infraFilter(r *http.Request) store.InfraFilter {
	return store.InfraFilter{Cluster: qs(r, "cluster"), Namespace: qs(r, "namespace"), Host: qs(r, "host"), Workload: qs(r, "workload"),
		Node: qs(r, "node"), Service: qs(r, "service"), Kind: qs(r, "kind"), Name: qs(r, "name"), Q: qs(r, "q"), Limit: qint(r, "limit")}
}

func (s *Server) phase6Routes(r chi.Router) {
	// ---- hosts, containers, Kubernetes (§20)
	r.Get("/infra/hosts", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		f := infraFilter(r)
		v, err := s.Store.ListHosts(r.Context(), q, store.InfraFilter{Cluster: f.Cluster, Q: f.Q})
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/hosts/{host}", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.GetHost(r.Context(), q, param(r, "host"))
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/containers", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		f := infraFilter(r)
		v, err := s.Store.ListContainers(r.Context(), q, store.InfraFilter{Host: f.Host, Service: f.Service, Cluster: f.Cluster, Namespace: f.Namespace})
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/k8s/clusters", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ListK8sClusters(r.Context(), q)
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/k8s/clusters/{cluster}", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.GetK8sCluster(r.Context(), q, param(r, "cluster"))
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/k8s/workloads", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		f := infraFilter(r)
		v, err := s.Store.ListWorkloads(r.Context(), q, store.InfraFilter{Cluster: f.Cluster, Namespace: f.Namespace})
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/k8s/pods", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		f := infraFilter(r)
		v, err := s.Store.ListPods(r.Context(), q, store.InfraFilter{Cluster: f.Cluster, Namespace: f.Namespace, Workload: f.Workload, Node: f.Node, Service: f.Service})
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/k8s/pods/{uid}", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.GetPod(r.Context(), q, param(r, "uid"))
		s.reply(w, r, v, err)
	}))
	r.Get("/infra/k8s/events", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		f := infraFilter(r)
		v, err := s.Store.ListK8sEvents(r.Context(), q, store.InfraFilter{Cluster: f.Cluster, Namespace: f.Namespace, Kind: f.Kind, Name: f.Name, Limit: f.Limit})
		s.reply(w, r, v, err)
	}))
	r.Get("/services/{service}/infra", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ServiceInfra(r.Context(), q, param(r, "service"))
		s.reply(w, r, v, err)
	}))

	r.Get("/monitor-templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.MonitorTemplates)
	})

	// ---- serverless (§21)
	r.Get("/serverless/summary", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.ServerlessSummary(r.Context(), q)
		s.reply(w, r, v, err)
	}))
	r.Get("/serverless/functions", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		p := qs(r, "provider")
		if p != "" && p != "aws" && p != "azure" {
			s.fail(w, r, store.Invalid("provider must be aws|azure"))
			return
		}
		v, err := s.Store.ListFunctions(r.Context(), q, store.ServerlessFilter{Provider: p, Q: qs(r, "q")})
		s.reply(w, r, v, err)
	}))
	r.Get("/serverless/functions/{id}", s.withQ(func(w http.ResponseWriter, r *http.Request, q store.Q) {
		v, err := s.Store.GetFunction(r.Context(), q, param(r, "id"))
		s.reply(w, r, v, err)
	}))
}
