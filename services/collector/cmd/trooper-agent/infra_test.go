package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/collector/internal/otlptest"
)

func TestParseFlagsModes(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     string
		wantErr string
	}{
		{"default db needs targets", []string{"--dsn", "http://k@h/1"}, "", "no targets"},
		{"host only needs no targets", []string{"--dsn", "http://k@h/1", "--agent-modes", "host"}, "", ""},
		{"env modes", []string{"--dsn", "http://k@h/1"}, "host,cluster", ""},
		{"db,host still needs targets", []string{"--dsn", "http://k@h/1", "--agent-modes", "db,host"}, "", "no targets"},
		{"unknown mode", []string{"--dsn", "http://k@h/1", "--agent-modes", "ebpf"}, "", "unknown agent mode"},
		{"dsn required", []string{"--agent-modes", "host"}, "", "--dsn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TT_AGENT_MODES", tt.env)
			_, err := parseFlags(tt.args)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestHostClusterOnce runs `trooper-agent --agent-modes host,cluster --once` against a fake API
// server and an in-process OTLP receiver and checks the wire: resources, names, logs, auth.
func TestHostClusterOnce(t *testing.T) {
	now := time.Now().UTC()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"node-a","labels":{"topology.kubernetes.io/region":"eu-central-1"}},"spec":{"providerID":"aws:///x/i-1"},
			  "status":{"allocatable":{"cpu":"4","memory":"8Gi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`)
		case "/api/v1/pods":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"orders-api-1","namespace":"shop","uid":"u1","labels":{"app":"orders-api"},
			  "ownerReferences":[{"kind":"ReplicaSet","name":"orders-api-5d4f6c7b8","controller":true}]},
			  "spec":{"nodeName":"node-a","containers":[{"name":"app"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`)
		case "/apis/apps/v1/deployments":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"orders-api","namespace":"shop"},"spec":{"replicas":1},"status":{"readyReplicas":1,"availableReplicas":1,"updatedReplicas":1}}]}`)
		case "/api/v1/events":
			fmt.Fprintf(w, `{"items":[{"metadata":{"uid":"e1","namespace":"shop"},"involvedObject":{"kind":"Pod","name":"orders-api-1","namespace":"shop"},
			  "reason":"Unhealthy","message":"Readiness probe failed","type":"Warning","count":3,"lastTimestamp":%q}]}`, now.Format(time.RFC3339))
		default:
			fmt.Fprint(w, `{"items":[]}`)
		}
	}))
	defer api.Close()
	srv := otlptest.New()
	defer srv.Close()

	t.Setenv("TT_K8S_API_URL", api.URL)
	t.Setenv("TT_K8S_TOKEN", "t")
	t.Setenv("TT_K8S_CLUSTER", "demo-eks")
	t.Setenv("NODE_NAME", "")
	t.Setenv("NODE_IP", "")
	t.Setenv("TT_AGENT_DOCKER_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	t.Setenv("TT_AGENT_TOP_PROCESSES", "3")
	f, err := parseFlags([]string{"--agent-modes", "host,cluster", "--once", "--once-gap", "1s",
		"--dsn", "http://tt_dev_ingest_key@" + srv.Listener.Addr().String() + "/1"})
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	err = run(context.Background(), f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}

	for _, k := range srv.Keys() {
		if k != "tt_dev_ingest_key" {
			t.Fatalf("x-trooper-key = %q", k)
		}
	}
	host := srv.Find("trooper.host.cpu_cores", nil)
	if len(host) == 0 {
		t.Fatal("no trooper.host.cpu_cores")
	}
	for _, k := range []string{"host.name", "host.id", "os.type", "host.arch"} {
		if host[0].Resource[k] == "" {
			t.Errorf("host resource %s missing: %v", k, host[0].Resource)
		}
	}
	if host[0].ServiceName != "trooper-agent" || host[0].Resource["k8s.cluster.name"] != "demo-eks" {
		t.Errorf("host resource = %v", host[0].Resource)
	}
	if len(srv.Find("trooper.process.memory_bytes", nil)) == 0 {
		t.Error("no process metrics")
	}
	node := srv.Find("trooper.k8s.node.ready", map[string]string{"k8s.node.name": "node-a"})
	if len(node) == 0 || node[0].Value != 1 || node[0].Resource["k8s.cluster.name"] != "demo-eks" ||
		node[0].Resource["cloud.provider"] != "aws" || node[0].Resource["cloud.region"] != "eu-central-1" || node[0].Resource["host.name"] != "" {
		t.Fatalf("node ready = %+v", node)
	}
	if p := srv.Find("trooper.k8s.pod.ready", map[string]string{"trooper.k8s.workload.kind": "Deployment", "trooper.k8s.workload.name": "orders-api", "trooper.service": "orders-api"}); len(p) == 0 {
		t.Error("pod with resolved workload missing")
	}
	if up := srv.Find("trooper.collector.up", map[string]string{"trooper.collector.kind": "k8s", "trooper.collector.target": "demo-eks"}); len(up) == 0 || up[0].Value != 1 {
		t.Errorf("collector.up k8s = %+v", up)
	}
	if up := srv.Find("trooper.collector.up", map[string]string{"trooper.collector.kind": "host"}); len(up) == 0 || up[0].Value != 1 {
		t.Errorf("collector.up host = %+v", up)
	}
	for _, p := range srv.Points() {
		if !strings.HasPrefix(p.Metric, "trooper.host.") && !strings.HasPrefix(p.Metric, "trooper.process.") &&
			!strings.HasPrefix(p.Metric, "trooper.k8s.") && p.Metric != "trooper.collector.up" {
			t.Errorf("unexpected metric %s", p.Metric)
		}
		if p.Kind != "gauge" {
			t.Errorf("%s is a %s, want gauge", p.Metric, p.Kind)
		}
	}
	logs := srv.Logs()
	if len(logs) != 1 {
		t.Fatalf("logs = %+v", logs)
	}
	l := logs[0]
	if l.Resource["service.name"] != "k8s" || l.Resource["k8s.cluster.name"] != "demo-eks" || l.SeverityText != "warn" || l.Severity != 13 ||
		l.Body != "Readiness probe failed" || l.Attrs["k8s.event.count"] != "3" || l.Attrs["k8s.event.reason"] != "Unhealthy" ||
		l.Attrs["k8s.object.kind"] != "Pod" || l.Attrs["k8s.namespace.name"] != "shop" {
		t.Fatalf("log = %+v", l)
	}
}

// TestFailingSourcesDoNotStopAgent: an unreachable API server and kubelet only flip
// trooper.collector.up to 0; host metrics still flow.
func TestFailingSourcesDoNotStopAgent(t *testing.T) {
	srv := otlptest.New()
	defer srv.Close()
	t.Setenv("TT_K8S_API_URL", "http://127.0.0.1:1")
	t.Setenv("TT_K8S_TOKEN", "t")
	t.Setenv("TT_K8S_CLUSTER", "c1")
	t.Setenv("NODE_NAME", "node-x")
	t.Setenv("NODE_IP", "")
	t.Setenv("TT_KUBELET_URL", "https://127.0.0.1:1")
	t.Setenv("TT_AGENT_TOP_PROCESSES", "0")
	f, err := parseFlags([]string{"--agent-modes", "host,cluster", "--once", "--once-gap", "100ms",
		"--dsn", "http://k@" + srv.Listener.Addr().String() + "/1"})
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	err = run(context.Background(), f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Stdout = stdout
	if err == nil || !strings.Contains(err.Error(), "2 of 3 targets failed") {
		t.Fatalf("err = %v", err)
	}
	for _, kind := range []string{"k8s", "kubelet"} {
		up := srv.Find("trooper.collector.up", map[string]string{"trooper.collector.kind": kind})
		if len(up) == 0 || up[0].Value != 0 {
			t.Errorf("collector.up %s = %+v", kind, up)
		}
	}
	if len(srv.Find("trooper.host.memory", nil)) == 0 {
		t.Error("host metrics must still be sent")
	}
}
