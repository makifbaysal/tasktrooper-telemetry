// Package retrace parses R8/ProGuard mapping.txt files and deobfuscates JVM stack traces.
package retrace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Mapping is a parsed mapping.txt.
type Mapping struct {
	classes    map[string]*Class // obfuscated name → class
	byOriginal map[string]*Class // original name → class
	ID         string            // "# pg_map_id:" when present
}

// Class is one "original -> obfuscated:" block.
type Class struct {
	Original   string
	Obfuscated string
	SourceFile string // from R8 {"id":"sourceFile"} metadata
	methods    map[string][]*member
}

type member struct {
	obfStart, obfEnd int
	hasRange         bool
	class            string // original declaring class when the method is qualified (inlined from another class)
	method           string
	args             string
	origStart        int // -1 = absent
	origEnd          int // -1 = absent
	obfName          string
	removeInner      int    // rewriteFrame: removeInnerFrames(n)
	removeIfThrows   string // rewriteFrame condition: throws(Lcom/Foo;) → com.Foo
}

var (
	classRe  = regexp.MustCompile(`^(\S+)\s+->\s+(\S+):\s*$`)
	methodRe = regexp.MustCompile(`^\s+(?:(\d+):(\d+):)?(\S+)\s+([^\s(]+)\(([^)]*)\)(?::(\d+)(?::(\d+))?)?\s+->\s+(\S+)\s*$`)
	throwsRe = regexp.MustCompile(`throws\(L([^;]+);\)`)
	removeRe = regexp.MustCompile(`removeInnerFrames\((\d+)\)`)
)

// maxLine guards against absurd sizes in hostile files.
const maxLineLen = 1 << 20

// Parse reads a mapping file. Unknown lines are skipped; it never panics.
func Parse(r io.Reader) (m *Mapping, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			m, err = nil, fmt.Errorf("mapping: malformed: %v", rec)
		}
	}()
	m = &Mapping{classes: map[string]*Class{}, byOriginal: map[string]*Class{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLineLen)
	var cur *Class
	var last *member
	n := 0
	for sc.Scan() {
		line := sc.Text()
		n++
		if strings.TrimSpace(line) == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
			if strings.HasPrefix(body, "pg_map_id:") {
				m.ID = strings.TrimSpace(strings.TrimPrefix(body, "pg_map_id:"))
			}
			if strings.HasPrefix(body, "{") {
				applyMeta(body, cur, last, line[0] == '#')
			}
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			if mm := classRe.FindStringSubmatch(line); mm != nil {
				cur = &Class{Original: mm[1], Obfuscated: mm[2], methods: map[string][]*member{}}
				m.classes[cur.Obfuscated] = cur
				m.byOriginal[cur.Original] = cur
				last = nil
			}
			continue
		}
		if cur == nil {
			continue
		}
		mm := methodRe.FindStringSubmatch(line)
		if mm == nil {
			last = nil // field or unsupported member
			continue
		}
		e := &member{origStart: -1, origEnd: -1, args: mm[5], obfName: mm[8]}
		if mm[1] != "" {
			e.obfStart, _ = strconv.Atoi(mm[1])
			e.obfEnd, _ = strconv.Atoi(mm[2])
			e.hasRange = true
		}
		name := mm[4]
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			e.class, e.method = name[:i], name[i+1:]
		} else {
			e.method = name
		}
		if mm[6] != "" {
			e.origStart, _ = strconv.Atoi(mm[6])
		}
		if mm[7] != "" {
			e.origEnd, _ = strconv.Atoi(mm[7])
		}
		cur.methods[e.obfName] = append(cur.methods[e.obfName], e)
		last = e
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mapping: line %d: %w", n+1, err)
	}
	if len(m.classes) == 0 {
		return nil, fmt.Errorf("mapping: no class mappings found")
	}
	return m, nil
}

type meta struct {
	ID         string   `json:"id"`
	FileName   string   `json:"fileName"`
	Conditions []string `json:"conditions"`
	Actions    []string `json:"actions"`
}

func applyMeta(body string, cur *Class, last *member, topLevel bool) {
	var md meta
	if json.Unmarshal([]byte(body), &md) != nil {
		return
	}
	switch md.ID {
	case "sourceFile":
		if cur != nil && last == nil {
			cur.SourceFile = md.FileName
		}
	case "com.android.tools.r8.rewriteFrame":
		if last == nil || topLevel {
			return
		}
		for _, a := range md.Actions {
			if mm := removeRe.FindStringSubmatch(a); mm != nil {
				last.removeInner, _ = strconv.Atoi(mm[1])
			}
		}
		for _, c := range md.Conditions {
			if mm := throwsRe.FindStringSubmatch(c); mm != nil {
				last.removeIfThrows = strings.ReplaceAll(mm[1], "/", ".")
			}
		}
	}
}

// Classes returns the number of mapped classes.
func (m *Mapping) Classes() int { return len(m.classes) }

// OriginalClass returns the original name for an obfuscated class name (or the input).
func (m *Mapping) OriginalClass(obf string) (string, bool) {
	if c, ok := m.classes[obf]; ok {
		return c.Original, true
	}
	// inner classes of a mapped outer class that kept their own name are listed separately; nothing else to do
	return obf, false
}

// Retraced is one original frame.
type Retraced struct {
	Class   string
	Method  string
	File    string
	Line    int // 0 = unknown
	Inlined bool
	// Alternatives lists other candidate methods when the mapping is ambiguous (no line info).
	Alternatives []string
}

// Frame retraces a single obfuscated frame. line <= 0 means unknown. thrown is the exception class of the
// trace (already deobfuscated) and top whether this is the first frame; both drive R8 rewriteFrame rules.
func (m *Mapping) Frame(obfClass, obfMethod string, line int, thrown string, top bool) ([]Retraced, bool) {
	c, ok := m.classes[obfClass]
	if !ok {
		return nil, false
	}
	members := c.methods[obfMethod]
	if len(members) == 0 {
		return []Retraced{{Class: c.Original, Method: obfMethod, File: m.sourceFile(c.Original), Line: line}}, true
	}
	var chain []*member
	if line > 0 {
		for _, e := range members {
			if e.hasRange && line >= e.obfStart && line <= e.obfEnd {
				chain = append(chain, e)
			}
		}
	}
	if len(chain) == 0 {
		// no range matched: prefer range-less entries, then everything; collapse to distinct methods
		var pool []*member
		for _, e := range members {
			if !e.hasRange {
				pool = append(pool, e)
			}
		}
		if len(pool) == 0 {
			pool = members
		}
		seen := map[string]bool{}
		var uniq []*member
		for _, e := range pool {
			k := e.class + "." + e.method
			if !seen[k] {
				seen[k] = true
				uniq = append(uniq, e)
			}
		}
		first := uniq[0]
		r := m.toRetraced(c, first, line, false)
		if !first.hasRange && first.origStart < 0 {
			r.Line = line // ProGuard without line info keeps the reported line
		}
		if len(uniq) > 1 {
			r.Line = 0
			for _, e := range uniq[1:] {
				r.Alternatives = append(r.Alternatives, m.declaring(c, e)+"."+e.method)
			}
		}
		return []Retraced{r}, true
	}
	if top {
		if e := chain[0]; e.removeInner > 0 && (e.removeIfThrows == "" || e.removeIfThrows == thrown) && e.removeInner < len(chain) {
			chain = chain[e.removeInner:]
		}
	}
	out := make([]Retraced, 0, len(chain))
	for i, e := range chain {
		out = append(out, m.toRetraced(c, e, line, i < len(chain)-1))
	}
	return out, true
}

func (m *Mapping) declaring(c *Class, e *member) string {
	if e.class != "" {
		return e.class
	}
	return c.Original
}

func (m *Mapping) toRetraced(c *Class, e *member, line int, inlined bool) Retraced {
	cls := m.declaring(c, e)
	r := Retraced{Class: cls, Method: e.method, File: m.sourceFile(cls), Inlined: inlined}
	switch {
	case e.origStart >= 0 && e.origEnd >= 0 && e.hasRange && e.origEnd-e.origStart == e.obfEnd-e.obfStart:
		r.Line = e.origStart + (line - e.obfStart)
	case e.origStart >= 0:
		r.Line = e.origStart
	case e.hasRange:
		r.Line = line
	default:
		r.Line = 0
	}
	if r.Line < 0 {
		r.Line = 0
	}
	return r
}

// sourceFile returns the file recorded for an original class (or one derived from the outer class name).
func (m *Mapping) sourceFile(original string) string {
	if c, ok := m.byOriginal[original]; ok && c.SourceFile != "" {
		return c.SourceFile
	}
	outer := original
	if i := strings.IndexByte(outer, '$'); i >= 0 {
		outer = outer[:i]
	}
	if c, ok := m.byOriginal[outer]; ok && c.SourceFile != "" {
		return c.SourceFile
	}
	if i := strings.LastIndexByte(outer, '.'); i >= 0 {
		outer = outer[i+1:]
	}
	return outer + ".java"
}
