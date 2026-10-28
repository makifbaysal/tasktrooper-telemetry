// Command ai-playground exposes only the offline mock on loopback for local UI testing.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
)

type inventoryRequest struct {
	Key, Kind, Service, File, Criticality string
}

func readInventory(w http.ResponseWriter, r *http.Request) (inventoryRequest, bool) {
	var in inventoryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil ||
		len(in.Key) == 0 || len(in.Key) > 200 || len(in.Service) == 0 || len(in.Service) > 200 ||
		len(in.Kind) == 0 || len(in.Kind) > 50 || len(in.File) > 500 {
		http.Error(w, `{"error":"invalid inventory item"}`, http.StatusBadRequest)
		return inventoryRequest{}, false
	}
	return in, true
}

func main() {
	provider := ai.NewMock() // Deliberately ignores paid-provider environment variables.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","provider":"mock"}`))
	})
	mux.HandleFunc("POST /incident-summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in ai.Input
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		in.Title = strings.TrimSpace(in.Title)
		if len(in.Title) == 0 || len(in.Title) > 200 || len(in.Evidence) == 0 || len(in.Evidence) > 10 {
			http.Error(w, `{"error":"title or evidence length is invalid"}`, http.StatusBadRequest)
			return
		}
		for _, item := range in.Evidence {
			if len(strings.TrimSpace(item)) == 0 || len(item) > 500 {
				http.Error(w, `{"error":"evidence item length is invalid"}`, http.StatusBadRequest)
				return
			}
		}
		result, err := provider.Summarize(r.Context(), in)
		if err != nil {
			http.Error(w, `{"error":"mock provider failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("POST /inventory-enrichment", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		in, ok := readInventory(w, r)
		if !ok {
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Summary      string   `json:"summary"`
			Criticality  string   `json:"criticality"`
			Provider     string   `json:"provider"`
			FailureModes []string `json:"failureModes"`
			Usage        ai.Usage `json:"usage"`
		}{
			Summary:      "[Mock AI draft] " + in.Kind + " " + in.Key + " belongs to " + in.Service + ". Review the handler and observed traffic before assigning business impact.",
			Criticality:  in.Criticality,
			Provider:     "mock",
			FailureModes: []string{"Review errors, latency, and downstream dependencies for this code path."},
		})
	})
	mux.HandleFunc("POST /monitor-suggestions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		in, ok := readInventory(w, r)
		if !ok {
			return
		}
		type suggestion struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			Condition string `json:"condition"`
			Rationale string `json:"rationale"`
		}
		var drafts []suggestion
		switch in.Kind {
		case "endpoint":
			drafts = []suggestion{{"Error rate for " + in.Key, "error_rate", "> 1% for 5m", "Illustrative threshold; calibrate against 14 days of traffic."}}
			if in.Criticality == "critical" {
				drafts = append(drafts, suggestion{"p95 latency for " + in.Key, "latency", "> 800ms for 10m", "Critical paths should also track user-visible slowness."})
			}
		case "consumer":
			drafts = []suggestion{{"Consumer lag for " + in.Key, "consumer_lag", "> 10,000 messages for 5m", "Illustrative backlog threshold; calibrate to arrival and drain rates."}}
		case "cron":
			drafts = []suggestion{{"Missed run for " + in.Key, "job_missed", "no run within expected schedule", "Review the configured schedule before activating."}}
		default:
			drafts = []suggestion{{"Failure rate for " + in.Key, "metric_threshold", "> 1% for 5m", "Illustrative template; verify that this item emits the required telemetry."}}
		}
		_ = json.NewEncoder(w).Encode(struct {
			Suggestions []suggestion `json:"suggestions"`
			Provider    string       `json:"provider"`
			Usage       ai.Usage     `json:"usage"`
		}{drafts, "mock", ai.Usage{}})
	})
	mux.HandleFunc("POST /loadtest-draft", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		in, ok := readInventory(w, r)
		if !ok {
			return
		}
		method, path, routeOK := strings.Cut(in.Key, " ")
		if in.Kind != "endpoint" || !routeOK || !strings.HasPrefix(path, "/") {
			http.Error(w, `{"error":"an HTTP endpoint is required"}`, http.StatusBadRequest)
			return
		}
		yaml := fmt.Sprintf("name: Draft for %s\ntarget: <verified-staging-host>\nstages:\n  - { duration: 1m, target: 5 }\n  - { duration: 3m, target: 20 }\n  - { duration: 1m, target: 0 }\nsteps:\n  - request: { method: %s, url: %s }\nthresholds:\n  http_req_failed: rate < 0.01\n  http_req_duration: p(95) < 800\n", in.Key, method, path)
		_ = json.NewEncoder(w).Encode(struct {
			YAML     string   `json:"yaml"`
			Note     string   `json:"note"`
			Provider string   `json:"provider"`
			Usage    ai.Usage `json:"usage"`
		}{yaml, "Draft only. Verify target ownership and request payload before running.", "mock", ai.Usage{}})
	})
	mux.HandleFunc("POST /incident-next-steps", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in ai.Input
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || len(strings.TrimSpace(in.Title)) == 0 || len(in.Title) > 200 {
			http.Error(w, `{"error":"invalid incident"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Steps    []string `json:"steps"`
			Provider string   `json:"provider"`
			Usage    ai.Usage `json:"usage"`
		}{[]string{
			"Compare the incident start time with recent deploys and configuration changes.",
			"Inspect related traces and error logs; confirm the affected service and dependency.",
			"Verify error rate and latency recover before resolving the incident.",
		}, "mock", ai.Usage{}})
	})
	mux.HandleFunc("POST /incident-report", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in ai.Input
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || len(strings.TrimSpace(in.Title)) == 0 || len(in.Title) > 200 {
			http.Error(w, `{"error":"invalid incident"}`, http.StatusBadRequest)
			return
		}
		var evidence []string
		for _, e := range in.Evidence {
			if len(e) > 0 && len(e) <= 500 && len(evidence) < 10 {
				evidence = append(evidence, "- "+e)
			}
		}
		report := "# Draft incident report: " + in.Title + "\n\n## Evidence\n" + strings.Join(evidence, "\n") +
			"\n\n## Impact\nConfirm affected users and duration.\n\n## Timeline\nAdd verified incident events and deploy times.\n\n## Cause\nUnknown until traces and logs are reviewed.\n\n## Follow-up\nRecord the fix and preventive checks.\n"
		_ = json.NewEncoder(w).Encode(struct {
			Report   string   `json:"report"`
			Provider string   `json:"provider"`
			Usage    ai.Usage `json:"usage"`
		}{report, "mock", ai.Usage{}})
	})
	mux.HandleFunc("POST /triage-logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in struct {
			Title string   `json:"title"`
			Logs  []string `json:"logs"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || len(in.Title) > 200 || len(in.Logs) > 10 {
			http.Error(w, `{"error":"invalid log set"}`, http.StatusBadRequest)
			return
		}
		counts := map[string]int{}
		for _, line := range in.Logs {
			if len(line) > 500 {
				http.Error(w, `{"error":"log line too long"}`, http.StatusBadRequest)
				return
			}
			lower := strings.ToLower(line)
			label := "Other warnings/errors"
			switch {
			case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
				label = "Timeouts"
			case strings.Contains(lower, "connect") || strings.Contains(lower, "network"):
				label = "Connectivity"
			case strings.Contains(lower, "database") || strings.Contains(lower, "sql"):
				label = "Database"
			}
			counts[label]++
		}
		type group struct {
			Label string `json:"label"`
			Count int    `json:"count"`
		}
		groups := []group{}
		for _, label := range []string{"Timeouts", "Connectivity", "Database", "Other warnings/errors"} {
			if counts[label] > 0 {
				groups = append(groups, group{label, counts[label]})
			}
		}
		_ = json.NewEncoder(w).Encode(struct {
			Groups   []group  `json:"groups"`
			Provider string   `json:"provider"`
			Usage    ai.Usage `json:"usage"`
		}{groups, "mock", ai.Usage{}})
	})
	mux.HandleFunc("POST /ask-telemetry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in struct {
			Question      string  `json:"question"`
			RPM           float64 `json:"rpm"`
			ErrorRate     float64 `json:"errorRate"`
			P95           float64 `json:"p95"`
			OpenIncidents int     `json:"openIncidents"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			http.Error(w, `{"error":"invalid telemetry question"}`, http.StatusBadRequest)
			return
		}
		var answer string
		switch in.Question {
		case "health":
			answer = fmt.Sprintf("Current snapshot: %.0f requests/min, %.2f%% error rate, p95 %.0f ms, and %d open incidents. Inspect the incident and service detail views for causes.", in.RPM, in.ErrorRate*100, in.P95, in.OpenIncidents)
		case "errors":
			answer = fmt.Sprintf("The current project-wide error rate is %.2f%% with %d open incidents. Open an incident for its monitor, timeline, and related logs.", in.ErrorRate*100, in.OpenIncidents)
		case "latency":
			answer = fmt.Sprintf("Current project-wide p95 latency is %.0f ms. Compare service and endpoint charts to identify which path contributes most.", in.P95)
		default:
			http.Error(w, `{"error":"question is not allowed"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Answer   string   `json:"answer"`
			Provider string   `json:"provider"`
			Usage    ai.Usage `json:"usage"`
		}{answer, "mock", ai.Usage{}})
	})
	server := &http.Server{Addr: "127.0.0.1:8099", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Println("offline AI playground listening on 127.0.0.1:8099")
	log.Fatal(server.ListenAndServe())
}
