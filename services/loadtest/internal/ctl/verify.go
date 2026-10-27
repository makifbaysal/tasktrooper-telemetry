package ctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Resolver looks up TXT records (net.Resolver satisfies it; tests fake it).
type Resolver interface {
	LookupTXT(ctx context.Context, host string) ([]string, error)
}

// Verifier proves control of a domain: DNS TXT `trooper-verify=<token>` on the host, or the body of
// https://<host>/.well-known/trooper-verify equal to the token.
type Verifier struct {
	DNS  Resolver
	HTTP *http.Client
	// WellKnownURL builds the HTTP check URL (default https://<host>/.well-known/trooper-verify).
	WellKnownURL func(host string) string
}

// NewVerifier returns a verifier with the system resolver and a short-timeout client that never leaves the host.
func NewVerifier() *Verifier {
	return &Verifier{DNS: net.DefaultResolver, HTTP: &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
				return errors.New("redirect to another host")
			}
			return nil
		}}}
}

// Check returns the method that proved control ("dns" | "http") or an error describing both attempts.
func (v *Verifier) Check(ctx context.Context, host, token string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	token = strings.TrimSpace(token)
	if host == "" || token == "" {
		return "", errors.New("host and token are required")
	}
	want := "trooper-verify=" + token
	var dnsErr error
	if v.DNS != nil {
		recs, err := v.DNS.LookupTXT(ctx, host)
		if err == nil {
			for _, r := range recs {
				if strings.TrimSpace(r) == want {
					return "dns", nil
				}
			}
			dnsErr = fmt.Errorf("no TXT record %q", want)
		} else {
			dnsErr = fmt.Errorf("TXT lookup: %v", err)
		}
	}
	u := "https://" + host + "/.well-known/trooper-verify"
	if v.WellKnownURL != nil {
		u = v.WellKnownURL(host)
	}
	httpErr := v.checkHTTP(ctx, u, token)
	if httpErr == nil {
		return "http", nil
	}
	return "", fmt.Errorf("dns: %v; http: %v", dnsErr, httpErr)
}

func (v *Verifier) checkHTTP(ctx context.Context, u, token string) error {
	hc := v.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "TaskTrooper-Verify/1")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", u, resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if strings.TrimSpace(string(b)) != token {
		return fmt.Errorf("%s does not contain the token", u)
	}
	return nil
}
