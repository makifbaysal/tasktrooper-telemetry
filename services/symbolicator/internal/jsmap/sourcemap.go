// Package jsmap decodes Source Map v3 files and symbolicates minified JavaScript stacks.
package jsmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Map is a decoded source map (plain or indexed/sectioned, flattened).
type Map struct {
	File           string
	Sources        []string
	SourcesContent []string // may be shorter than Sources; "" = absent
	Names          []string
	lines          [][]segment // index = generated line (0-based), sorted by genCol
}

type segment struct {
	genCol  int32
	src     int32 // -1 = no source
	srcLine int32
	srcCol  int32
	name    int32 // -1 = no name
}

// Mapping is the original position of a generated position.
type Mapping struct {
	Source  string
	Line    int // 1-based
	Col     int // 1-based
	Name    string
	Content string // full original source text when sourcesContent was embedded
}

type rawMap struct {
	Version        int       `json:"version"`
	File           string    `json:"file"`
	SourceRoot     string    `json:"sourceRoot"`
	Sources        []*string `json:"sources"`
	SourcesContent []*string `json:"sourcesContent"`
	Names          []string  `json:"names"`
	Mappings       string    `json:"mappings"`
	Sections       []struct {
		Offset struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		} `json:"offset"`
		Map json.RawMessage `json:"map"`
	} `json:"sections"`
}

// Parse decodes a v3 source map. It never panics on malformed input.
func Parse(data []byte) (m *Map, err error) {
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, fmt.Errorf("sourcemap: malformed: %v", r)
		}
	}()
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if bytes.HasPrefix(data, []byte(")]}'")) { // XSSI guard
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	var raw rawMap
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("sourcemap: %w", err)
	}
	return fromRaw(&raw, 0)
}

func fromRaw(raw *rawMap, depth int) (*Map, error) {
	if depth > 4 {
		return nil, errors.New("sourcemap: sections nested too deep")
	}
	if raw.Version != 3 {
		return nil, fmt.Errorf("sourcemap: unsupported version %d", raw.Version)
	}
	if len(raw.Sections) > 0 {
		out := &Map{File: raw.File}
		for _, s := range raw.Sections {
			var sub rawMap
			if err := json.Unmarshal(s.Map, &sub); err != nil {
				return nil, fmt.Errorf("sourcemap: section: %w", err)
			}
			sm, err := fromRaw(&sub, depth+1)
			if err != nil {
				return nil, err
			}
			out.merge(sm, s.Offset.Line, s.Offset.Column)
		}
		return out, nil
	}
	m := &Map{File: raw.File, Names: raw.Names}
	root := raw.SourceRoot
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}
	for _, s := range raw.Sources {
		v := ""
		if s != nil {
			v = *s
		}
		if root != "" && !strings.Contains(v, "://") && !strings.HasPrefix(v, "/") {
			v = root + v
		}
		m.Sources = append(m.Sources, v)
	}
	for _, c := range raw.SourcesContent {
		if c == nil {
			m.SourcesContent = append(m.SourcesContent, "")
		} else {
			m.SourcesContent = append(m.SourcesContent, *c)
		}
	}
	if err := m.decode(raw.Mappings); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Map) merge(sub *Map, lineOff, colOff int) {
	srcOff, nameOff := int32(len(m.Sources)), int32(len(m.Names))
	m.Sources = append(m.Sources, sub.Sources...)
	for len(m.SourcesContent) < int(srcOff) {
		m.SourcesContent = append(m.SourcesContent, "")
	}
	m.SourcesContent = append(m.SourcesContent, sub.SourcesContent...)
	m.Names = append(m.Names, sub.Names...)
	for i, segs := range sub.lines {
		gl := i + lineOff
		for len(m.lines) <= gl {
			m.lines = append(m.lines, nil)
		}
		for _, s := range segs {
			if i == 0 {
				s.genCol += int32(colOff)
			}
			if s.src >= 0 {
				s.src += srcOff
			}
			if s.name >= 0 {
				s.name += nameOff
			}
			m.lines[gl] = append(m.lines[gl], s)
		}
		sort.SliceStable(m.lines[gl], func(a, b int) bool { return m.lines[gl][a].genCol < m.lines[gl][b].genCol })
	}
}

const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var b64val = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(b64); i++ {
		t[b64[i]] = int8(i)
	}
	return
}()

func (m *Map) decode(mappings string) error {
	var src, srcLine, srcCol, name int32
	line := []segment{}
	var fields [5]int32
	i := 0
	for i <= len(mappings) {
		// one segment or a separator
		if i == len(mappings) || mappings[i] == ';' {
			m.lines = append(m.lines, line)
			line = []segment{}
			i++
			continue
		}
		if mappings[i] == ',' {
			i++
			continue
		}
		n := 0
		for i < len(mappings) && mappings[i] != ',' && mappings[i] != ';' {
			if n >= 5 {
				return errors.New("sourcemap: segment with more than 5 fields")
			}
			v, next, err := vlq(mappings, i)
			if err != nil {
				return err
			}
			fields[n] = v
			n++
			i = next
		}
		genCol := fields[0]
		if len(line) > 0 {
			genCol += line[len(line)-1].genCol
		}
		seg := segment{genCol: genCol, src: -1, name: -1}
		switch n {
		case 1:
		case 4, 5:
			src += fields[1]
			srcLine += fields[2]
			srcCol += fields[3]
			if src < 0 || int(src) >= len(m.Sources) {
				return fmt.Errorf("sourcemap: source index %d out of range", src)
			}
			seg.src, seg.srcLine, seg.srcCol = src, srcLine, srcCol
			if n == 5 {
				name += fields[4]
				if name >= 0 && int(name) < len(m.Names) {
					seg.name = name
				}
			}
		default:
			return fmt.Errorf("sourcemap: segment with %d fields", n)
		}
		line = append(line, seg)
	}
	for _, l := range m.lines {
		if !sort.SliceIsSorted(l, func(a, b int) bool { return l[a].genCol < l[b].genCol }) {
			sort.SliceStable(l, func(a, b int) bool { return l[a].genCol < l[b].genCol })
		}
	}
	return nil
}

func vlq(s string, i int) (int32, int, error) {
	var result int64
	shift := uint(0)
	for {
		if i >= len(s) {
			return 0, i, errors.New("sourcemap: truncated VLQ")
		}
		d := b64val[s[i]]
		if d < 0 {
			return 0, i, fmt.Errorf("sourcemap: invalid base64 char %q", s[i])
		}
		i++
		result |= int64(d&31) << shift
		if d&32 == 0 {
			break
		}
		shift += 5
		if shift > 31 {
			return 0, i, errors.New("sourcemap: VLQ overflow")
		}
	}
	v := int32(result >> 1)
	if result&1 == 1 {
		v = -v
	}
	return v, i, nil
}

// Lookup maps a 1-based generated line/column (as printed in JS stacks) to the original position.
// It returns the closest segment at or before the column on that line.
func (m *Map) Lookup(line, col int) (Mapping, bool) {
	s, ok := m.segmentAt(line-1, col-1)
	if !ok || s.src < 0 {
		return Mapping{}, false
	}
	return m.mapping(s), true
}

func (m *Map) segmentAt(line, col int) (segment, bool) {
	if line < 0 || line >= len(m.lines) || col < 0 {
		return segment{}, false
	}
	segs := m.lines[line]
	i := sort.Search(len(segs), func(i int) bool { return int(segs[i].genCol) > col })
	if i == 0 {
		return segment{}, false
	}
	return segs[i-1], true
}

func (m *Map) mapping(s segment) Mapping {
	out := Mapping{Source: m.Sources[s.src], Line: int(s.srcLine) + 1, Col: int(s.srcCol) + 1}
	if s.name >= 0 {
		out.Name = m.Names[s.name]
	}
	if int(s.src) < len(m.SourcesContent) {
		out.Content = m.SourcesContent[s.src]
	}
	return out
}

// exactName returns the name recorded for the token starting exactly at (line, col) (0-based), or the
// identifier found at the mapped position in the embedded original source.
func (m *Map) exactName(line, col int) string {
	s, ok := m.segmentAt(line, col)
	if !ok || s.src < 0 || int(s.genCol) != col {
		return ""
	}
	if s.name >= 0 {
		return m.Names[s.name]
	}
	mp := m.mapping(s)
	return identAt(mp.Content, mp.Line, mp.Col)
}

// identAt reads a JS identifier at 1-based line/col of src.
func identAt(src string, line, col int) string {
	if src == "" {
		return ""
	}
	for l := 1; l < line; l++ {
		i := strings.IndexByte(src, '\n')
		if i < 0 {
			return ""
		}
		src = src[i+1:]
	}
	if i := strings.IndexByte(src, '\n'); i >= 0 {
		src = src[:i]
	}
	if col-1 < 0 || col-1 >= len(src) {
		return ""
	}
	src = src[col-1:]
	n := 0
	for n < len(src) && isIdent(src[n]) {
		n++
	}
	if n == 0 || (src[0] >= '0' && src[0] <= '9') {
		return ""
	}
	return src[:n]
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}
