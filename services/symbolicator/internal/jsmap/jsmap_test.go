package jsmap

import (
	"os"
	"strings"
	"testing"
)

func loadBundle(t *testing.T, withMinified bool) *Bundle {
	t.Helper()
	data, err := os.ReadFile("../../testdata/js/dist/assets/app.min.js.map")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bundle{Map: m}
	if withMinified {
		js, err := os.ReadFile("../../testdata/js/dist/assets/app.min.js")
		if err != nil {
			t.Fatal(err)
		}
		b.Minified = string(js)
	}
	return b
}

func TestSymbolicateFixture(t *testing.T) {
	type want struct {
		fn, file string
		line     int
	}
	expect := []want{
		{"validatePrice", "../../src/cart.ts", 11},
		{"computeTotal", "../../src/cart.ts", 4},
		{"checkout", "../../src/app.ts", 4},
	}
	for _, tc := range []struct {
		name, stackFile string
		minified        bool
		lastFn          string // fn of frame 3 (resolved only via declaration lookup)
	}{
		{"chrome+minified", "stack-chrome.txt", true, "checkout"},
		{"firefox+minified", "stack-firefox.txt", true, "checkout"},
		{"chrome map only", "stack-chrome.txt", false, "u"},
		{"firefox map only", "stack-firefox.txt", false, "u"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, _ := os.ReadFile("../../testdata/js/" + tc.stackFile)
			b := loadBundle(t, tc.minified)
			res := Symbolicate(string(stack), func(url string) (*Bundle, bool) {
				if strings.HasSuffix(url, "/assets/app.min.js") {
					return b, true
				}
				return nil, false
			})
			if !res.Symbolicated {
				t.Fatal("not symbolicated")
			}
			if len(res.Frames) != 4 {
				t.Fatalf("frames = %d", len(res.Frames))
			}
			for i, w := range expect {
				f := res.Frames[i]
				wantFn := w.fn
				if i == 2 {
					wantFn = tc.lastFn
				}
				if f.Fn != wantFn || f.File != w.file || f.Line != w.line || !f.InApp || !f.Symbolicated {
					t.Errorf("frame %d = %+v, want %s %s:%d", i, f, wantFn, w.file, w.line)
				}
			}
			if res.Frames[3].Symbolicated {
				t.Errorf("unmapped frame marked symbolicated: %+v", res.Frames[3])
			}
			if !strings.Contains(res.Stack, "at validatePrice (../../src/cart.ts:11:") {
				t.Errorf("stack text:\n%s", res.Stack)
			}
		})
	}
}

func TestParseLine(t *testing.T) {
	for _, tc := range []struct {
		in       string
		ok       bool
		fn, url  string
		line, cl int
	}{
		{"    at n (https://a.com/x.js:1:88)", true, "n", "https://a.com/x.js", 1, 88},
		{"    at https://a.com/x.js:3:4", true, "", "https://a.com/x.js", 3, 4},
		{"    at async Promise.all (index 0)", false, "", "", 0, 0},
		{"    at async t.load (https://a.com/x.js:1:5)", true, "t.load", "https://a.com/x.js", 1, 5},
		{"    at new Foo (http://localhost:5173/src/a.js:10:2)", true, "Foo", "http://localhost:5173/src/a.js", 10, 2},
		{"    at Object.<anonymous> (webpack-internal:///./src/a.js:1:1)", true, "Object.<anonymous>", "webpack-internal:///./src/a.js", 1, 1},
		{"n@https://a.com/x.js:1:88", true, "n", "https://a.com/x.js", 1, 88},
		{"@https://a.com/x.js:1:88", true, "", "https://a.com/x.js", 1, 88},
		{"Error: boom", false, "", "", 0, 0},
		{"", false, "", "", 0, 0},
	} {
		rf, ok := ParseLine(tc.in)
		if ok != tc.ok || ok && (rf.Fn != tc.fn || rf.URL != tc.url || rf.Line != tc.line || rf.Col != tc.cl) {
			t.Errorf("ParseLine(%q) = %+v %v", tc.in, rf, ok)
		}
	}
}

func TestParseMalformed(t *testing.T) {
	for _, in := range []string{
		``, `{`, `{"version":2}`, `{"version":3,"sources":["a"],"mappings":"!!!"}`,
		`{"version":3,"sources":["a"],"mappings":"AAAAAAAAA"}`, `{"version":3,"sources":[],"mappings":"AAAA"}`,
		`{"version":3,"sources":["a"],"mappings":"gggggggggggggg"}`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) = nil error", in)
		}
	}
}

func TestIndexedMap(t *testing.T) {
	in := `{"version":3,"file":"b.js","sections":[
	 {"offset":{"line":0,"column":0},"map":{"version":3,"sources":["a.ts"],"names":["foo"],"mappings":"AAAAA"}},
	 {"offset":{"line":2,"column":10},"map":{"version":3,"sources":["b.ts"],"names":["bar"],"mappings":"AAEAA"}}]}`
	m, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if mp, ok := m.Lookup(1, 1); !ok || mp.Source != "a.ts" || mp.Name != "foo" {
		t.Errorf("section 1: %+v %v", mp, ok)
	}
	if mp, ok := m.Lookup(3, 12); !ok || mp.Source != "b.ts" || mp.Line != 3 || mp.Name != "bar" {
		t.Errorf("section 2: %+v %v", mp, ok)
	}
	if _, ok := m.Lookup(3, 5); ok {
		t.Error("before section offset should not map")
	}
}
