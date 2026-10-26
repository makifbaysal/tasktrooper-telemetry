package server

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestOpenAPICoversRoutes fails when a route is missing from openapi.yaml (or documented but not served).
func TestOpenAPICoversRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := map[string]bool{}
	var path string
	pathRe := regexp.MustCompile(`^  (/\S*):$`)
	opRe := regexp.MustCompile(`^    (get|post|put|patch|delete):$`)
	for _, line := range strings.Split(string(raw), "\n") {
		if m := pathRe.FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if m := opRe.FindStringSubmatch(line); m != nil {
			spec[strings.ToUpper(m[1])+" "+path] = true
		}
	}
	h := newTestServer(t, &fakeStore{}).(chi.Routes)
	served := map[string]bool{}
	err = chi.Walk(h, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(strings.ReplaceAll(route, "/*", ""), "/")
		served[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for k := range served {
		if !spec[k] {
			t.Errorf("route %s is not in openapi.yaml", k)
		}
	}
	for k := range spec {
		if !served[k] {
			t.Errorf("openapi.yaml documents %s, which is not served", k)
		}
	}
	if len(served) < 80 {
		t.Errorf("only %d routes walked", len(served))
	}
}
