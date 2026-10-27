package ctl

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// K8sConfig configures the Kubernetes Job executor (TT_LOADTEST_EXECUTOR=k8s).
type K8sConfig struct {
	APIServer      string // default https://kubernetes.default.svc
	Namespace      string // default: the pod's namespace
	Image          string // runner image (TT_LOADTEST_K8S_IMAGE)
	ServiceAccount string
	SecretName     string            // envFrom secret with TT_POSTGRES_URL, TT_INTERNAL_TOKEN … (TT_LOADTEST_K8S_SECRET)
	RegionLabel    string            // node label selecting a region (TT_LOADTEST_K8S_REGION_LABEL), empty = none
	Env            map[string]string // plain env for every runner (ingest URL, log level)
	CPU            string            // requests.cpu (default 500m)
	Memory         string            // requests/limits.memory (default 512Mi)
	Deadline       time.Duration     // activeDeadlineSeconds (run duration + slack)
	TokenFile      string
	CAFile         string
}

// K8sExecutor creates one batch/v1 Job per region through the Kubernetes REST API (no client-go).
type K8sExecutor struct {
	Cfg   K8sConfig
	HTTP  *http.Client
	Token func() (string, error)
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewK8sExecutor uses the in-cluster service account.
func NewK8sExecutor(cfg K8sConfig) (*K8sExecutor, error) {
	if cfg.APIServer == "" {
		cfg.APIServer = "https://kubernetes.default.svc"
	}
	if cfg.TokenFile == "" {
		cfg.TokenFile = saDir + "/token"
	}
	if cfg.CAFile == "" {
		cfg.CAFile = saDir + "/ca.crt"
	}
	if cfg.Namespace == "" {
		b, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("k8s executor: namespace: %w (set TT_LOADTEST_K8S_NAMESPACE)", err)
		}
		cfg.Namespace = strings.TrimSpace(string(b))
	}
	if cfg.Image == "" {
		return nil, errors.New("k8s executor: TT_LOADTEST_K8S_IMAGE is required")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca, err := os.ReadFile(cfg.CAFile); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		tlsCfg.RootCAs = pool
	}
	tokenFile := cfg.TokenFile
	return &K8sExecutor{Cfg: cfg, HTTP: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
		Token: func() (string, error) { // re-read: projected tokens rotate
			b, err := os.ReadFile(tokenFile)
			return strings.TrimSpace(string(b)), err
		}}, nil
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// JobName is the DNS-1123 name of a run's region Job.
func JobName(run int64, region string) string {
	r := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(region), "-"), "-")
	n := "tt-loadtest-" + strconv.FormatInt(run, 10) + "-" + r
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// RenderJob renders the Job manifest of a region runner.
func RenderJob(cfg K8sConfig, j Job) map[string]any {
	labels := map[string]any{
		"app.kubernetes.io/name":    "loadtest-runner",
		"app.kubernetes.io/part-of": "tasktrooper",
		"trooper.dev/loadtest-run":  strconv.FormatInt(j.RunID, 10),
		"trooper.dev/region":        strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(j.Region), "-"), "-"),
	}
	envMap := map[string]string{}
	for k, v := range cfg.Env {
		envMap[k] = v
	}
	for k, v := range j.Env() {
		envMap[k] = v
	}
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]any, 0, len(keys))
	for _, k := range keys {
		env = append(env, map[string]any{"name": k, "value": envMap[k]})
	}
	cpu, mem := cfg.CPU, cfg.Memory
	if cpu == "" {
		cpu = "500m"
	}
	if mem == "" {
		mem = "512Mi"
	}
	container := map[string]any{
		"name":    "runner",
		"image":   cfg.Image,
		"command": []any{"/usr/local/bin/loadtest-runner"},
		"env":     env,
		"resources": map[string]any{
			"requests": map[string]any{"cpu": cpu, "memory": mem},
			"limits":   map[string]any{"memory": mem},
		},
		"securityContext": map[string]any{
			"runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"capabilities": map[string]any{"drop": []any{"ALL"}},
		},
	}
	if cfg.SecretName != "" {
		container["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": cfg.SecretName}}}
	}
	pod := map[string]any{
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": 30,
		"containers":                    []any{container},
	}
	if cfg.ServiceAccount != "" {
		pod["serviceAccountName"] = cfg.ServiceAccount
	}
	if cfg.RegionLabel != "" {
		pod["nodeSelector"] = map[string]any{cfg.RegionLabel: j.Region}
	}
	spec := map[string]any{
		"backoffLimit":            0,
		"ttlSecondsAfterFinished": 600,
		"template":                map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod},
	}
	dl := cfg.Deadline
	if j.Deadline > 0 {
		dl = j.Deadline
	}
	if dl > 0 {
		spec["activeDeadlineSeconds"] = int(dl.Seconds())
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": JobName(j.RunID, j.Region), "namespace": cfg.Namespace, "labels": labels},
		"spec":       spec,
	}
}

func (k *K8sExecutor) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.Cfg.APIServer, "/")+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if k.Token != nil {
		tok, err := k.Token()
		if err != nil {
			return nil, 0, fmt.Errorf("k8s token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

func (k *K8sExecutor) jobPath(name string) string {
	p := "/apis/batch/v1/namespaces/" + k.Cfg.Namespace + "/jobs"
	if name != "" {
		p += "/" + name
	}
	return p
}

// Start creates the Job.
func (k *K8sExecutor) Start(ctx context.Context, j Job) (Handle, error) {
	cfg := k.Cfg
	m := RenderJob(cfg, j)
	b, code, err := k.do(ctx, http.MethodPost, k.jobPath(""), m)
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	if code == http.StatusConflict { // left over from a previous attempt: replace it
		_, _, _ = k.do(ctx, http.MethodDelete, k.jobPath(JobName(j.RunID, j.Region))+"?propagationPolicy=Background", nil)
		return nil, fmt.Errorf("create job: %s already exists (deleted; retry)", JobName(j.RunID, j.Region))
	}
	if code >= 300 {
		return nil, fmt.Errorf("create job: HTTP %d: %s", code, strings.TrimSpace(string(b)))
	}
	return &k8sHandle{k: k, name: JobName(j.RunID, j.Region)}, nil
}

type k8sHandle struct {
	k    *K8sExecutor
	name string
}

// Stop deletes the Job; the pod gets SIGTERM and writes its final shard row.
func (h *k8sHandle) Stop(ctx context.Context) error {
	_, code, err := h.k.do(ctx, http.MethodDelete, h.k.jobPath(h.name)+"?propagationPolicy=Background", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return fmt.Errorf("delete job %s: HTTP %d", h.name, code)
	}
	return nil
}

// Status reads .status of the Job.
func (h *k8sHandle) Status(ctx context.Context) (bool, error) {
	b, code, err := h.k.do(ctx, http.MethodGet, h.k.jobPath(h.name), nil)
	if err != nil {
		return false, nil // API hiccup: unknown, not exited
	}
	if code == http.StatusNotFound {
		return true, errors.New("job " + h.name + " not found")
	}
	var job struct {
		Status struct {
			Succeeded  int `json:"succeeded"`
			Failed     int `json:"failed"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(b, &job) != nil {
		return false, nil
	}
	if job.Status.Succeeded > 0 {
		return true, nil
	}
	for _, c := range job.Status.Conditions {
		if c.Type == "Failed" && c.Status == "True" {
			return true, fmt.Errorf("job %s failed: %s %s", h.name, c.Reason, c.Message)
		}
	}
	if job.Status.Failed > 0 {
		return true, fmt.Errorf("job %s failed", h.name)
	}
	return false, nil
}
