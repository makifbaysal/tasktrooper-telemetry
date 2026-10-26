package rca

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/golden.json is produced by running apps/web/src/lib/rca.ts on the trees in testdata/golden.ts:
//
//	node --experimental-strip-types golden.ts > golden.json   (with rca.ts copied next to it, type imports removed)
type golden struct {
	Trees []struct {
		Name string `json:"name"`
		Unit Unit   `json:"unit"`
		Tree *Node  `json:"tree"`
	} `json:"trees"`
	Out []struct {
		Name       string  `json:"name"`
		Summary    string  `json:"summary"`
		Confidence float64 `json:"confidence"`
		Root       string  `json:"root"`
		Path       []struct {
			ID           string   `json:"id"`
			Explains     float64  `json:"explains"`
			Contribution float64  `json:"contribution"`
			Alts         []string `json:"alts"`
		} `json:"path"`
	} `json:"out"`
}

func TestMatchesTypeScript(t *testing.T) {
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	for i, tc := range g.Trees {
		want := g.Out[i]
		t.Run(tc.Name, func(t *testing.T) {
			r := FindRootCause(tc.Tree, tc.Unit)
			if r.Summary != want.Summary {
				t.Errorf("summary %q\n want %q", r.Summary, want.Summary)
			}
			if math.Abs(r.Confidence-want.Confidence) > 1e-9 || r.Root.ID != want.Root {
				t.Errorf("confidence/root %v %s want %v %s", r.Confidence, r.Root.ID, want.Confidence, want.Root)
			}
			if len(r.Path) != len(want.Path) {
				t.Fatalf("path len %d want %d", len(r.Path), len(want.Path))
			}
			for j, h := range r.Path {
				w := want.Path[j]
				if h.Node.ID != w.ID || math.Abs(h.Explains-w.Explains) > 1e-9 || math.Abs(h.Contribution-w.Contribution) > 1e-9 {
					t.Errorf("hop %d: %s %v %v want %s %v %v", j, h.Node.ID, h.Explains, h.Contribution, w.ID, w.Explains, w.Contribution)
				}
				var alts []string
				for _, a := range h.Alternatives {
					alts = append(alts, a.Node.ID)
				}
				if strings.Join(alts, ",") != strings.Join(w.Alts, ",") {
					t.Errorf("hop %d alts %v want %v", j, alts, w.Alts)
				}
			}
		})
	}
}

// fakeSource is an in-memory call graph:
// checkout-api POST /checkout → postgres · orders-db, HTTP · payments-api → payments-api POST /charge → HTTP · iyzico
type fakeSource struct{ queries int }

func st(calls, errs, dur float64) Stat { return Stat{Calls: calls, Errors: errs, DurSum: dur} }

func (f *fakeSource) Endpoint(_ context.Context, svc, route, method string) (Pair, error) {
	f.queries++
	switch svc + " " + method + " " + route {
	case "checkout-api POST /checkout":
		// 1000 req: 250 ms → 1150 ms; errors 1% → 11%
		return Pair{Cur: st(1000, 110, 1_150_000), Base: st(1000, 10, 250_000)}, nil
	case "payments-api POST /charge":
		return Pair{Cur: st(1000, 100, 1_000_000), Base: st(1000, 0, 100_000)}, nil
	}
	return Pair{}, nil
}

func (f *fakeSource) Edges(_ context.Context, svc, route, method string) ([]Edge, error) {
	f.queries++
	switch svc {
	case "checkout-api":
		return []Edge{
			{Dep: "postgres · orders-db", DepKind: "db", Pair: Pair{Cur: st(3000, 0, 60_000), Base: st(3000, 0, 60_000)}},
			{Dep: "HTTP · payments-api", DepKind: "http", ChildService: "payments-api", Pair: Pair{Cur: st(1000, 100, 1_010_000), Base: st(1000, 0, 110_000)}},
		}, nil
	case "payments-api":
		return []Edge{
			{Dep: "HTTP · iyzico", DepKind: "http", Pair: Pair{Cur: st(1000, 100, 950_000), Base: st(1000, 0, 60_000)}},
		}, nil
	}
	return nil, nil
}

func (f *fakeSource) CalleeRoutes(_ context.Context, callee, caller string) ([]RouteCalls, error) {
	f.queries++
	if callee == "payments-api" && caller == "checkout-api" {
		return []RouteCalls{{Route: "/charge", Method: "POST", Pair: Pair{Cur: st(1000, 0, 0), Base: st(1000, 0, 0)}}}, nil
	}
	return nil, nil
}

func (f *fakeSource) Deploys(_ context.Context, svc string) ([]Deploy, error) {
	if svc == "payments-api" {
		return []Deploy{{Version: "v42", At: time.Unix(1_700_000_000, 0)}}, nil
	}
	return nil, nil
}

func TestBuildAndAttribute(t *testing.T) {
	tests := []struct {
		unit    Unit
		rootID  string
		kind    string
		summary string
		pathLen int
		minConf float64
	}{
		{UnitMs, "payments-api>HTTP · iyzico", "external", "Caused by payments-api's external dependency iyzico — 16× slower than usual.", 3, 0.9},
		{UnitRatio, "payments-api>HTTP · iyzico", "external", "Failures come from payments-api's external dependency iyzico (0.0% → 10.0% of calls failing).", 3, 0.9},
	}
	for _, tc := range tests {
		t.Run(string(tc.unit), func(t *testing.T) {
			src := &fakeSource{}
			r, tree, err := Analyze(context.Background(), src, "checkout-api", "/checkout", "POST", tc.unit)
			if err != nil {
				t.Fatal(err)
			}
			if tree.Label != "POST /checkout" || len(tree.Children) != 2 {
				t.Fatalf("tree root %+v", tree)
			}
			if r.Root.ID != tc.rootID || r.Root.Kind != tc.kind || len(r.Path) != tc.pathLen || r.Confidence < tc.minConf {
				t.Fatalf("result root=%s kind=%s path=%d conf=%v", r.Root.ID, r.Root.Kind, len(r.Path), r.Confidence)
			}
			if r.Summary != tc.summary {
				t.Fatalf("summary %q", r.Summary)
			}
			// payments-api endpoint carries its deploy as own-code evidence
			ep := r.Path[1].Node
			if ep.Kind != "endpoint" || len(ep.OwnEvidence) != 1 {
				t.Fatalf("hop 1 %+v", ep)
			}
			c := Compact(r)
			b, _ := json.Marshal(c)
			if strings.Contains(string(b), `"children"`) {
				t.Fatal("compact result still has children")
			}
		})
	}
}

func TestNodeBudget(t *testing.T) {
	src := &fakeSource{}
	root, err := Build(context.Background(), src, "checkout-api", "/checkout", "POST", UnitMs, Limits{MaxDepth: 4, MaxNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 1 {
		t.Fatalf("budget not applied: %d children", len(root.Children))
	}
	root, _ = Build(context.Background(), &fakeSource{}, "checkout-api", "/checkout", "POST", UnitMs, Limits{MaxDepth: 1, MaxNodes: 50})
	if len(root.Children[1].Node.Children) != 0 {
		t.Fatal("depth limit not applied")
	}
}
