package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"regexp"
	"strconv"
	"strings"
)

// Template functions (contracts §14 + what the UI's AI request generator writes):
//
//	{{int a-b}}                 random integer in [a, b]
//	{{uuid}}                    random UUID v4
//	{{pool.<name>}}             random element of spec.pools[name] (same row for every use in one request);
//	{{pool.<name>.<field>}}     a field of an object row; {{random.<name>}} is an alias
//	{{weighted a:70 b:30}}      weighted choice
//	{{prev "KEY" .field}}       field of the previous JSON response of step KEY in this VU
//	{{session.<name>}}          per-VU value (spec.session templates, rendered once per VU)
//	{{optional 12% <expr>}}     expr with 12% probability, else omitted (JSON key dropped / empty string)
//	{{faker.words N}}, {{faker.sentence}}, {{faker.name}}, {{faker.email}}, {{faker.number}}
//
// A JSON string value that is exactly one template keeps the template's type (numbers stay numbers,
// prev values keep their JSON type).

var tmplRe = regexp.MustCompile(`\{\{\s*(.*?)\s*\}\}`)

// absent marks an {{optional}} that was not taken.
type absent struct{}

// vuState is what a virtual user carries between requests.
type vuState struct {
	rng     *mrand.Rand
	session map[string]any
	prev    map[string]any // step key → decoded JSON response
}

// renderer renders the templates of one request.
type renderer struct {
	vu    *vuState
	pools map[string][]any
	rows  map[string]any // pool name → row picked for this request
	warn  func(string)
}

func (r *renderer) rng() *mrand.Rand { return r.vu.rng }

// String renders a text template; absent/nil render as "".
func (r *renderer) String(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		return toText(r.eval(tmplRe.FindStringSubmatch(m)[1]))
	})
}

// Value renders s keeping the type when s is exactly one template.
func (r *renderer) Value(s string) any {
	t := strings.TrimSpace(s)
	if m := tmplRe.FindStringSubmatchIndex(t); m != nil && m[0] == 0 && m[1] == len(t) {
		return r.eval(t[m[2]:m[3]])
	}
	return r.String(s)
}

// JSON renders every string inside a decoded JSON template.
func (r *renderer) JSON(v any) any {
	switch x := v.(type) {
	case string:
		return r.Value(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			rv := r.JSON(val)
			if _, skip := rv.(absent); skip {
				continue
			}
			out[k] = rv
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			rv := r.JSON(val)
			if _, skip := rv.(absent); skip {
				continue
			}
			out = append(out, rv)
		}
		return out
	}
	return v
}

func toText(v any) string {
	switch x := v.(type) {
	case nil, absent:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case json.Number:
		return x.String()
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return fmt.Sprint(v)
}

// fields splits an expression on spaces, keeping "double quoted" parts together.
func fields(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, c := range s {
		switch {
		case c == '"':
			inQ = !inQ
			if !inQ {
				out = append(out, cur.String())
				cur.Reset()
			}
		case c == ' ' && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func (r *renderer) eval(expr string) any {
	f := fields(expr)
	if len(f) == 0 {
		return ""
	}
	head := f[0]
	switch {
	case head == "uuid":
		return newUUID()
	case head == "int":
		if len(f) < 2 {
			r.warn("int: want {{int a-b}}")
			return 0
		}
		a, b, ok := strings.Cut(f[1], "-")
		lo, e1 := strconv.Atoi(a)
		hi, e2 := strconv.Atoi(b)
		if !ok || e1 != nil || e2 != nil || hi < lo {
			r.warn("int: bad range " + f[1])
			return 0
		}
		return float64(lo + r.rng().IntN(hi-lo+1))
	case head == "weighted":
		return typed(weighted(r.rng(), f[1:]))
	case head == "prev":
		if len(f) < 2 {
			r.warn(`prev: want {{prev "KEY" .field}}`)
			return nil
		}
		path := ""
		if len(f) > 2 {
			path = f[2]
		}
		v, ok := r.vu.prev[f[1]]
		if !ok {
			return nil
		}
		return walk(v, path)
	case head == "optional":
		if len(f) < 3 {
			r.warn("optional: want {{optional N% expr}}")
			return absent{}
		}
		p, err := strconv.ParseFloat(strings.TrimSuffix(f[1], "%"), 64)
		if err != nil {
			r.warn("optional: bad probability " + f[1])
			return absent{}
		}
		if r.rng().Float64()*100 >= p {
			return absent{}
		}
		i := strings.Index(expr, f[1]) + len(f[1])
		return r.eval(strings.TrimSpace(expr[i:]))
	case strings.HasPrefix(head, "pool.") || strings.HasPrefix(head, "random."):
		_, rest, _ := strings.Cut(head, ".")
		return r.pool(rest)
	case strings.HasPrefix(head, "session."):
		name := strings.TrimPrefix(head, "session.")
		if v, ok := r.vu.session[name]; ok {
			return v
		}
		r.warn("session." + name + " is not defined (spec.session); using a random value per VU")
		v := newUUID()
		r.vu.session[name] = v
		return v
	case strings.HasPrefix(head, "faker."):
		return r.faker(strings.TrimPrefix(head, "faker."), f[1:])
	}
	r.warn("unknown template {{" + expr + "}}")
	return ""
}

// pool resolves "name" or "name.field"; the longest defined pool name wins ("test_users.email" → pool test_users).
func (r *renderer) pool(ref string) any {
	name, field := ref, ""
	if _, ok := r.pools[name]; !ok {
		for i := strings.LastIndex(ref, "."); i > 0; i = strings.LastIndex(ref[:i], ".") {
			if _, ok := r.pools[ref[:i]]; ok {
				name, field = ref[:i], ref[i+1:]
				break
			}
		}
	}
	vals := r.pools[name]
	if len(vals) == 0 {
		r.warn("pool " + name + " has no values (spec.pools); using synthetic values")
		return fmt.Sprintf("%s-%d", name, 1+r.rng().IntN(1000))
	}
	row, ok := r.rows[name]
	if !ok {
		row = vals[r.rng().IntN(len(vals))]
		r.rows[name] = row
	}
	if field == "" {
		return row
	}
	return walk(row, "."+field)
}

func (r *renderer) faker(kind string, args []string) any {
	words := []string{"alpha", "bravo", "cargo", "delta", "ember", "fable", "giant", "harbor", "indigo", "jolly", "kayak", "lumen", "maple", "nova", "orbit", "pixel", "quartz", "river", "solar", "tango"}
	pick := func(n int) string {
		w := make([]string, n)
		for i := range w {
			w[i] = words[r.rng().IntN(len(words))]
		}
		return strings.Join(w, " ")
	}
	switch kind {
	case "words":
		n := 2
		if len(args) > 0 {
			if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
				n = v
			}
		}
		return pick(n)
	case "sentence":
		s := pick(6 + r.rng().IntN(5))
		return strings.ToUpper(s[:1]) + s[1:] + "."
	case "name":
		s := pick(2)
		return strings.ToUpper(s[:1]) + s[1:]
	case "email":
		return fmt.Sprintf("%s.%d@example.test", words[r.rng().IntN(len(words))], r.rng().IntN(100000))
	case "number":
		return float64(r.rng().IntN(10000))
	}
	r.warn("unknown faker." + kind)
	return ""
}

func weighted(rng *mrand.Rand, opts []string) string {
	type opt struct {
		v string
		w float64
	}
	var list []opt
	total := 0.0
	for _, o := range opts {
		i := strings.LastIndex(o, ":")
		if i < 0 {
			list = append(list, opt{o, 1})
			total++
			continue
		}
		w, err := strconv.ParseFloat(o[i+1:], 64)
		if err != nil || w < 0 {
			w = 1
		}
		list = append(list, opt{o[:i], w})
		total += w
	}
	if len(list) == 0 {
		return ""
	}
	x := rng.Float64() * total
	for _, o := range list {
		if x < o.w {
			return o.v
		}
		x -= o.w
	}
	return list[len(list)-1].v
}

var numRe = regexp.MustCompile(`^-?(0|[1-9]\d*)(\.\d+)?$`)

func typed(s string) any {
	if numRe.MatchString(s) {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	return s
}

// walk follows ".a.b.0.c" through decoded JSON.
func walk(v any, path string) any {
	path = strings.TrimPrefix(strings.TrimSpace(path), ".")
	if path == "" {
		return v
	}
	for _, p := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// prevRefs returns the step keys referenced by {{prev "KEY" …}} in the given template texts.
func prevRefs(texts ...string) map[string]bool {
	out := map[string]bool{}
	for _, t := range texts {
		for _, m := range tmplRe.FindAllStringSubmatch(t, -1) {
			f := fields(m[1])
			if len(f) >= 2 && f[0] == "prev" {
				out[f[1]] = true
			}
			if len(f) >= 4 && f[0] == "optional" && f[2] == "prev" {
				out[f[3]] = true
			}
		}
	}
	return out
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
