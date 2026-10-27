package infra

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/tasktrooper/telemetry/services/collector/internal/agent"
	"github.com/tasktrooper/telemetry/services/collector/internal/emit"
	"github.com/tasktrooper/telemetry/services/collector/internal/scrape"
)

// Agent modes (TT_AGENT_MODES).
const (
	ModeDB      = "db"
	ModeHost    = "host"
	ModeCluster = "cluster"
)

// Intervals (contracts §20).
const (
	HostInterval    = 15 * time.Second
	ClusterInterval = 30 * time.Second
)

// ParseModes parses a comma list; empty = db (today's behaviour).
func ParseModes(s string) ([]string, error) {
	var out []string
	for _, m := range strings.Split(s, ",") {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" {
			continue
		}
		if m != ModeDB && m != ModeHost && m != ModeCluster {
			return nil, fmt.Errorf("unknown agent mode %q (want db, host, cluster)", m)
		}
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		out = []string{ModeDB}
	}
	return out, nil
}

// Config is the host / cluster mode configuration.
type Config struct {
	Cluster       string // TT_K8S_CLUSTER
	DockerSocket  string // TT_AGENT_DOCKER_SOCKET
	TopProcesses  int    // TT_AGENT_TOP_PROCESSES
	CloudProvider string // TT_CLOUD_PROVIDER (else from the node's providerID)
	CloudRegion   string // TT_CLOUD_REGION (else topology.kubernetes.io/region)
	Kube          KubeEnv
	FS            HostFS
}

// ConfigFromEnv reads the environment (and points gopsutil at the host mounts).
func ConfigFromEnv() Config {
	c := Config{
		Cluster:       os.Getenv("TT_K8S_CLUSTER"),
		DockerSocket:  os.Getenv("TT_AGENT_DOCKER_SOCKET"),
		TopProcesses:  20,
		CloudProvider: os.Getenv("TT_CLOUD_PROVIDER"),
		CloudRegion:   os.Getenv("TT_CLOUD_REGION"),
		Kube:          KubeEnvFromOS(),
		FS:            DetectHostFS(),
	}
	if c.DockerSocket == "" {
		c.DockerSocket = DefaultDockerSocket
	}
	if v := os.Getenv("TT_AGENT_TOP_PROCESSES"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			c.TopProcesses = n
		}
	}
	return c
}

// HostTargets are the host-mode targets: this host (+ processes), and its containers from the
// kubelet (inside Kubernetes) or the Docker socket (when it exists).
func HostTargets(c Config, hostName string, log *slog.Logger) []agent.Target {
	out := []agent.Target{{
		ID: "host/" + hostName, Key: hostName, Kind: "host",
		Interval: HostInterval, Timeout: 12 * time.Second, Spec: "host",
		New: func() (scrape.Scraper, error) { return NewHostScraper(c.FS, c.TopProcesses), nil },
	}}
	switch {
	case c.Kube.OnNode():
		node := c.Kube.NodeName
		if node == "" {
			node = hostName
		}
		out = append(out, agent.Target{
			ID: "kubelet/" + node, Key: node, Kind: "kubelet",
			Interval: HostInterval, Timeout: 12 * time.Second, Spec: "kubelet",
			New: func() (scrape.Scraper, error) {
				kc, err := c.Kube.Kubelet()
				if err != nil {
					return nil, err
				}
				return NewKubeletScraper(kc), nil
			},
		})
	case socketExists(c.DockerSocket):
		out = append(out, agent.Target{
			ID: "docker/" + hostName, Key: hostName, Kind: "docker",
			Interval: HostInterval, Timeout: 12 * time.Second, Spec: "docker",
			New: func() (scrape.Scraper, error) { return NewDockerScraper(c.DockerSocket), nil },
		})
	default:
		log.Info("no kubelet (NODE_IP) and no docker socket: container metrics off", "socket", c.DockerSocket)
	}
	return out
}

func socketExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// ClusterTarget is the cluster-mode target.
func ClusterTarget(c Config, events LogSink) agent.Target {
	name := c.Cluster
	if name == "" {
		name = "default"
	}
	return agent.Target{
		ID: "k8s/" + name, Key: name, Kind: "k8s",
		Interval: ClusterInterval, Timeout: 25 * time.Second, Spec: "k8s",
		New: func() (scrape.Scraper, error) {
			kc, err := c.Kube.API()
			if err != nil {
				return nil, err
			}
			return NewClusterScraper(kc, events), nil
		},
	}
}

// HostResource returns the host-mode resource attributes (contracts §20).
func HostResource(ctx context.Context, c Config, hostName string) []attribute.KeyValue {
	a := []attribute.KeyValue{
		attribute.String(emit.ResHostName, hostName),
		attribute.String(emit.ResOSType, runtime.GOOS),
		attribute.String(emit.ResHostArch, runtime.GOARCH),
	}
	if id := c.FS.HostID(); id != "" {
		a = append(a, attribute.String(emit.ResHostID, id))
	}
	if c.Cluster != "" {
		a = append(a, attribute.String(emit.ResK8sCluster, c.Cluster))
	}
	if c.Kube.NodeName != "" {
		a = append(a, attribute.String(emit.ResK8sNode, c.Kube.NodeName))
	}
	return append(a, cloudAttrs(ctx, c)...)
}

// ClusterResource returns the cluster-mode resource attributes.
func ClusterResource(ctx context.Context, c Config) []attribute.KeyValue {
	var a []attribute.KeyValue
	if c.Cluster != "" {
		a = append(a, attribute.String(emit.ResK8sCluster, c.Cluster))
	}
	return append(a, cloudAttrs(ctx, c)...)
}

// cloudAttrs: TT_CLOUD_PROVIDER / TT_CLOUD_REGION, else best effort from a node object
// (spec.providerID scheme, topology.kubernetes.io/region label).
func cloudAttrs(ctx context.Context, c Config) []attribute.KeyValue {
	provider, region := c.CloudProvider, c.CloudRegion
	if (provider == "" || region == "") && c.Kube.InCluster() {
		if kc, err := c.Kube.API(); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var n *Node
			if c.Kube.NodeName != "" {
				var one Node
				if kc.Get(cctx, "/api/v1/nodes/"+c.Kube.NodeName, &one) == nil {
					n = &one
				}
			} else {
				var list struct {
					Items []Node `json:"items"`
				}
				if kc.Get(cctx, "/api/v1/nodes?limit=1", &list) == nil && len(list.Items) > 0 {
					n = &list.Items[0]
				}
			}
			if n != nil {
				if provider == "" {
					provider = ProviderFromID(n.Spec.ProviderID)
				}
				if region == "" {
					region = n.Metadata.Labels["topology.kubernetes.io/region"]
				}
			}
		}
	}
	var a []attribute.KeyValue
	if provider != "" {
		a = append(a, attribute.String(emit.ResCloudProvider, provider))
	}
	if region != "" {
		a = append(a, attribute.String(emit.ResCloudRegion, region))
	}
	return a
}

// ProviderFromID maps a node providerID ("aws:///eu-central-1a/i-…") to cloud.provider.
func ProviderFromID(id string) string {
	scheme, _, ok := strings.Cut(id, "://")
	if !ok {
		return ""
	}
	switch scheme {
	case "aws":
		return "aws"
	case "gce":
		return "gcp"
	case "azure":
		return "azure"
	case "digitalocean":
		return "digitalocean"
	case "hcloud":
		return "hetzner"
	}
	return ""
}
