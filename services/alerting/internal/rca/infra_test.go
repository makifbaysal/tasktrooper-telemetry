package rca

import (
	"strings"
	"testing"
)

func TestInfraCandidates(t *testing.T) {
	cases := []struct {
		name       string
		pods       []PodSignal
		hosts      []HostSignal
		wantTitles []string
		wantConf   []float64
	}{
		{"nothing", []PodSignal{{Pod: "a"}}, []HostSignal{{Host: "h", CPU: 0.5}}, nil, nil},
		{"oom", []PodSignal{{Namespace: "shop", Pod: "api-1", Reasons: []string{"OOMKilled"}, RestartIncrease: 1}}, nil,
			[]string{"Pod api-1 restarted (OOMKilled)"}, []float64{0.8}},
		{"oom wins over crashloop, many restarts", []PodSignal{{Pod: "api-1", Reasons: []string{"CrashLoopBackOff", "OOMKilled"}, RestartIncrease: 4}}, nil,
			[]string{"Pod api-1 restarted (OOMKilled)"}, []float64{0.85}},
		{"crashloop + restarts on two pods", []PodSignal{
			{Pod: "api-1", Reasons: []string{"CrashLoopBackOff"}, RestartIncrease: 1},
			{Pod: "api-2", RestartIncrease: 2}}, nil,
			[]string{"Pod api-1 restarted (CrashLoopBackOff)", "Pod api-2 restarted 2× in the last 15 min"}, []float64{0.8, 0.65}},
		{"host saturated", nil, []HostSignal{{Host: "node-a", CPU: 0.96}, {Host: "node-b", CPU: 1.2}, {Host: "node-c", CPU: 0.9}},
			[]string{"Host node-b CPU saturated (120%)", "Host node-a CPU saturated (96%)"}, []float64{0.85, 0.75}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InfraCandidates(tc.pods, tc.hosts)
			if len(got) != len(tc.wantTitles) {
				t.Fatalf("got %d candidates %+v, want %d", len(got), got, len(tc.wantTitles))
			}
			for i, c := range got {
				if c.Title != tc.wantTitles[i] || abs(c.Confidence-tc.wantConf[i]) > 1e-9 {
					t.Errorf("#%d: %q %.3f, want %q %.3f", i, c.Title, c.Confidence, tc.wantTitles[i], tc.wantConf[i])
				}
				if c.Confidence < InfraMinConfidence || c.Confidence > InfraMaxConfidence {
					t.Errorf("confidence %v out of range", c.Confidence)
				}
			}
		})
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestApplyInfra(t *testing.T) {
	oom := []InfraCandidate{{Kind: "pod", Entity: "shop/api-1", Title: "Pod api-1 restarted (OOMKilled)", Confidence: 0.8, Cluster: "prod"}}
	dbRoot := &Node{ID: "db", Kind: "db", Label: "orders · pg", Service: "orders"}
	codeRoot := &Node{ID: "x:code", Kind: "code", Label: "orders own code", Service: "orders"}
	cases := []struct {
		name        string
		res         *Result
		cands       []InfraCandidate
		wantSummary string
		wantConf    float64
		wantTree    bool
	}{
		{"no candidates keeps nil", nil, nil, "", 0, false},
		{"no tree", nil, oom, "Pod api-1 restarted (OOMKilled).", 0.8, false},
		{"own code → infra wins", &Result{Unit: UnitMs, Path: []Hop{{Node: codeRoot}}, Root: codeRoot, Summary: "own code", Confidence: 0.9}, oom,
			"Pod api-1 restarted (OOMKilled).", 0.8, true},
		{"weaker dependency → infra wins", &Result{Unit: UnitMs, Path: []Hop{{Node: dbRoot}}, Root: dbRoot, Summary: "db", Confidence: 0.5}, oom,
			"Pod api-1 restarted (OOMKilled).", 0.8, true},
		{"stronger dependency kept", &Result{Unit: UnitMs, Path: []Hop{{Node: dbRoot}}, Root: dbRoot, Summary: "db", Confidence: 0.95}, oom,
			"db", 0.95, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyInfra(tc.res, "orders", "", tc.cands)
			if tc.cands == nil {
				if got != tc.res {
					t.Fatal("result changed without candidates")
				}
				return
			}
			if got.Summary != tc.wantSummary || got.Confidence != tc.wantConf || len(got.Infra) != 1 || got.Root == nil {
				t.Fatalf("got %+v", got)
			}
			if (got.TreeSummary != "") != tc.wantTree || (tc.wantTree && got.TreeConfidence == nil) {
				t.Errorf("tree summary %q %v", got.TreeSummary, got.TreeConfidence)
			}
			ev := got.Root.Evidence
			if len(ev) == 0 || ev[len(ev)-1].Kind != "resource" || !strings.Contains(ev[len(ev)-1].Detail, "prod") {
				t.Errorf("evidence %+v", ev)
			}
		})
	}
	if len(dbRoot.Evidence) != 0 || len(codeRoot.Evidence) != 0 {
		t.Error("ApplyInfra mutated the shared root node")
	}
}
