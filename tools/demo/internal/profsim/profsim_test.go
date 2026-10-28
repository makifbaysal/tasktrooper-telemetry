package profsim

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

type parsed struct {
	total  int64
	byLeaf map[string]int64 // frame → total of the stacks containing it
	routes map[string]int64
}

// parse checks the folded format (contracts §24) and sums values.
func parse(t *testing.T, body []byte) parsed {
	t.Helper()
	p := parsed{byLeaf: map[string]int64{}, routes: map[string]int64{}}
	route := ""
	for i, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			route = ""
			for _, f := range strings.Fields(line[1:]) {
				if k, v, ok := strings.Cut(f, "="); ok && k == "trooper.route" {
					route = v
				}
			}
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp <= 0 {
			t.Fatalf("line %d: no value: %q", i+1, line)
		}
		v, err := strconv.ParseInt(line[sp+1:], 10, 64)
		if err != nil || v <= 0 {
			t.Fatalf("line %d: bad value %q", i+1, line[sp+1:])
		}
		frames := strings.Split(line[:sp], ";")
		if len(frames) > 128 {
			t.Fatalf("line %d: %d frames", i+1, len(frames))
		}
		seen := map[string]bool{}
		for _, f := range frames {
			if f == "" || strings.ContainsAny(f, " \t") {
				t.Fatalf("line %d: bad frame %q", i+1, f)
			}
			if !seen[f] {
				p.byLeaf[f] += v
				seen[f] = true
			}
		}
		p.total += v
		p.routes[route] += v
	}
	return p
}

func spec(t *testing.T, name string) serviceSpec {
	for _, s := range specs() {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no spec %s", name)
	return serviceSpec{}
}

func TestFoldedProfilesAreValid(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, s := range specs() {
		for _, ty := range Types {
			for _, prev := range []bool{true, false} {
				p := parse(t, Folded(s, ty.Type, prev, 1, 2, rng))
				if p.total <= 0 {
					t.Fatalf("%s %s: empty", s.name, ty.Type)
				}
				if ty.Type != "inuse_space" && len(p.routes) < 2 {
					t.Fatalf("%s %s: no route sections", s.name, ty.Type)
				}
			}
		}
	}
}

func TestCPUMatchesCores(t *testing.T) {
	s := spec(t, "payments-api")
	p := parse(t, Folded(s, "cpu", true, 1, 2, rand.New(rand.NewPCG(3, 4))))
	want := s.cores / 2 * float64(cpuWindow.Nanoseconds())
	if got := float64(p.total); got < want*0.85 || got > want*1.15 {
		t.Fatalf("cpu total %.0f ns, want ≈ %.0f", got, want)
	}
}

// TestVersionDiff: orders-api v2.41.0 spends a much larger share in pricing.ApplyDiscounts.
func TestVersionDiff(t *testing.T) {
	s := spec(t, "orders-api")
	share := func(prev bool) float64 {
		var tot, fn int64
		for i := 0; i < 20; i++ {
			p := parse(t, Folded(s, "cpu", prev, 1, 2, rand.New(rand.NewPCG(uint64(i), 9))))
			tot += p.total
			fn += p.byLeaf["github.com/shop/orders-api/internal/pricing.ApplyDiscounts"]
		}
		return float64(fn) / float64(tot)
	}
	old, cur := share(true), share(false)
	if cur < old*2.5 {
		t.Fatalf("ApplyDiscounts share old=%.3f new=%.3f: regression not visible", old, cur)
	}
	if p := parse(t, Folded(s, "cpu", true, 1, 2, rand.New(rand.NewPCG(1, 1)))); p.byLeaf["regexp.Compile"] != 0 {
		t.Fatal("previous version must not compile regexps per request")
	}
}

func TestTargetsUsePods(t *testing.T) {
	ts := targets()
	if len(ts) != 8 {
		t.Fatalf("targets %d", len(ts))
	}
	for _, tg := range ts {
		if len(tg.hosts) < 2 || tg.version == tg.spec.prevVersion {
			t.Fatalf("%s: hosts=%v version=%s prev=%s", tg.spec.name, tg.hosts, tg.version, tg.spec.prevVersion)
		}
	}
}
