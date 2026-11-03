package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/auth"
)

const testInternalToken = "tt-internal-test-token"

func TestInternalCredential(t *testing.T) {
	e := newEnv(t, 0, 0)
	body := `{"app":"web","release":"1.0","platform":"javascript","stack":"Error: x\n    at a (https://x/app.min.js:1:2)"}`
	for _, tc := range []struct {
		name, method, path string
		hdr                map[string]string
		want               int
	}{
		{"internal token + project", "POST", "/v1/symbolicate", map[string]string{auth.HeaderInternal: testInternalToken, auth.HeaderProject: "1"}, 200},
		{"wrong token", "POST", "/v1/symbolicate", map[string]string{auth.HeaderInternal: "guess", auth.HeaderProject: "1"}, 401},
		{"wrong token with valid key", "POST", "/v1/symbolicate", map[string]string{auth.HeaderInternal: "guess", auth.HeaderProject: "1", "Authorization": "Bearer tt_dev_api_key"}, 401},
		{"missing project", "POST", "/v1/symbolicate", map[string]string{auth.HeaderInternal: testInternalToken}, 401},
		{"non-numeric project", "POST", "/v1/symbolicate", map[string]string{auth.HeaderInternal: testInternalToken, auth.HeaderProject: "1x"}, 401},
		{"not accepted for artifacts", "GET", "/v1/artifacts?app=web", map[string]string{auth.HeaderInternal: testInternalToken, auth.HeaderProject: "1"}, 401},
		{"api key still works", "POST", "/v1/symbolicate", map[string]string{"Authorization": "Bearer tt_dev_api_key"}, 200},
	} {
		req, _ := http.NewRequest(tc.method, e.srv.URL+tc.path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range tc.hdr {
			req.Header.Set(k, v)
		}
		if code, _ := do(t, req); code != tc.want {
			t.Errorf("%s: status %d want %d", tc.name, code, tc.want)
		}
	}
}

func TestInternalDisabled(t *testing.T) {
	var i *auth.Internal = auth.NewInternal("  ")
	if i != nil {
		t.Fatal("empty token must disable the credential")
	}
	if _, err := i.Project("anything", "1"); err == nil {
		t.Error("nil Internal accepted a token")
	}
}
