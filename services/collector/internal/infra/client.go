package infra

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// In-cluster service account files.
const (
	SATokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	SACAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// KubeClient is a read-only HTTP client for the Kubernetes API server or the kubelet.
type KubeClient struct {
	BaseURL string
	// Token is a static bearer token; TokenFile is re-read on every request (bound SA tokens
	// rotate). Token wins.
	Token     string
	TokenFile string
	HC        *http.Client
	// Stream is HC without the overall request timeout (watch streams); nil = derived from HC.
	Stream *http.Client
}

// KubeEnv is the environment the clients are built from (injectable for tests).
type KubeEnv struct {
	APIURL   string // TT_K8S_API_URL (else https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT)
	Token    string // TT_K8S_TOKEN (else the service account token file)
	Insecure bool   // TT_K8S_INSECURE: skip API server TLS verification (tests / dev clusters)

	KubeletURL      string // TT_KUBELET_URL (else https://$NODE_IP:10250)
	KubeletInsecure bool   // TT_KUBELET_INSECURE: skip kubelet serving-cert verification
	NodeIP          string // NODE_IP
	NodeName        string // NODE_NAME

	TokenFile, CAFile string // defaults: service account files
}

// KubeEnvFromOS reads the environment.
func KubeEnvFromOS() KubeEnv {
	e := KubeEnv{
		APIURL:          os.Getenv("TT_K8S_API_URL"),
		Token:           os.Getenv("TT_K8S_TOKEN"),
		Insecure:        envBool("TT_K8S_INSECURE"),
		KubeletURL:      os.Getenv("TT_KUBELET_URL"),
		KubeletInsecure: envBool("TT_KUBELET_INSECURE"),
		NodeIP:          os.Getenv("NODE_IP"),
		NodeName:        os.Getenv("NODE_NAME"),
		TokenFile:       SATokenFile,
		CAFile:          SACAFile,
	}
	if e.APIURL == "" {
		if h, p := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"); h != "" {
			if p == "" {
				p = "443"
			}
			e.APIURL = "https://" + net.JoinHostPort(h, p)
		}
	}
	return e
}

func envBool(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// InCluster reports whether a Kubernetes API is configured.
func (e KubeEnv) InCluster() bool { return e.APIURL != "" }

// OnNode reports whether the kubelet is reachable in principle (host mode inside Kubernetes).
func (e KubeEnv) OnNode() bool { return e.KubeletURL != "" || e.NodeIP != "" }

func (e KubeEnv) tlsConfig(insecure bool) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		c.InsecureSkipVerify = true //nolint:gosec // explicit opt-in (TT_K8S_INSECURE / TT_KUBELET_INSECURE)
		return c
	}
	if e.CAFile != "" {
		if pem, err := os.ReadFile(e.CAFile); err == nil {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			pool.AppendCertsFromPEM(pem)
			c.RootCAs = pool
		}
	}
	return c
}

func (e KubeEnv) client(base string, insecure bool) *KubeClient {
	tr := &http.Transport{
		TLSClientConfig:     e.tlsConfig(insecure),
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
	kc := &KubeClient{BaseURL: strings.TrimSuffix(base, "/"), Token: e.Token,
		HC: &http.Client{Transport: tr, Timeout: 60 * time.Second}, Stream: &http.Client{Transport: tr}}
	if kc.Token == "" {
		kc.TokenFile = e.TokenFile
	}
	return kc
}

// API returns the API server client.
func (e KubeEnv) API() (*KubeClient, error) {
	if e.APIURL == "" {
		return nil, errors.New("no Kubernetes API: set TT_K8S_API_URL or run in a pod (KUBERNETES_SERVICE_HOST)")
	}
	return e.client(e.APIURL, e.Insecure), nil
}

// Kubelet returns the kubelet client.
func (e KubeEnv) Kubelet() (*KubeClient, error) {
	base := e.KubeletURL
	if base == "" {
		if e.NodeIP == "" {
			return nil, errors.New("no kubelet: set NODE_IP (downward API status.hostIP) or TT_KUBELET_URL")
		}
		base = "https://" + net.JoinHostPort(e.NodeIP, "10250")
	}
	return e.client(base, e.KubeletInsecure), nil
}

// HTTPError is a non-2xx answer.
type HTTPError struct {
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("GET %s: %d %s", e.URL, e.Status, e.Body)
}

// Open performs a GET and returns the body (caller closes).
func (c *KubeClient) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	return c.open(ctx, c.HC, path)
}

// OpenStream is Open for long-running watch streams: no overall client timeout (ctx bounds it).
func (c *KubeClient) OpenStream(ctx context.Context, path string) (io.ReadCloser, error) {
	hc := c.Stream
	if hc == nil {
		cp := *c.HC
		cp.Timeout = 0
		hc = &cp
	}
	return c.open(ctx, hc, path)
}

func (c *KubeClient) open(ctx context.Context, hc *http.Client, path string) (io.ReadCloser, error) {
	u := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	tok := c.Token
	if tok == "" && c.TokenFile != "" {
		if b, err := os.ReadFile(c.TokenFile); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &HTTPError{URL: redactQuery(u), Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return resp.Body, nil
}

// Get decodes a JSON GET.
func (c *KubeClient) Get(ctx context.Context, path string, out any) error {
	body, err := c.Open(ctx, path)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}

// ListAll pages through a list endpoint (limit/continue) and decodes every item.
func ListAll[T any](ctx context.Context, c *KubeClient, path string, extra url.Values) ([]T, error) {
	var out []T
	cont := ""
	for {
		q := url.Values{}
		for k, v := range extra {
			q[k] = v
		}
		q.Set("limit", "500")
		if cont != "" {
			q.Set("continue", cont)
		}
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []T `json:"items"`
		}
		if err := c.Get(ctx, path+"?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Metadata.Continue == "" {
			return out, nil
		}
		cont = page.Metadata.Continue
	}
}

func redactQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
