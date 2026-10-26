package native

import (
	"debug/dwarf"
	"fmt"
	"sort"
	"strings"
)

// Loc is one (possibly inlined) source location for an address; Lookup returns innermost first.
type Loc struct {
	Fn      string
	Linkage string // mangled/linkage name when present
	File    string
	Line    int
	Col     int
	Inlined bool // inlined into the next Loc
}

// Lookup resolves a file address (vmaddr space of the image). ok=false when nothing matched.
func (s *Symbolizer) Lookup(addr uint64) (locs []Loc, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			locs, ok = nil, false
		}
	}()
	if s.d != nil {
		if !s.built {
			s.buildCUIndex()
		}
		if locs := s.dwarfLookup(addr); len(locs) > 0 {
			return locs, true
		}
	}
	if i := sort.Search(len(s.syms), func(i int) bool { return s.syms[i].addr > addr }); i > 0 {
		return []Loc{{Fn: s.syms[i-1].name, Linkage: s.syms[i-1].name}}, true
	}
	return nil, false
}

func (s *Symbolizer) buildCUIndex() {
	s.built = true
	r := s.d.Reader()
	for {
		e, err := r.Next()
		if err != nil || e == nil {
			break
		}
		if e.Tag == dwarf.TagCompileUnit || e.Tag == dwarf.TagPartialUnit {
			if rs, err := s.d.Ranges(e); err == nil {
				for _, rg := range rs {
					if rg[1] > rg[0] {
						s.cus = append(s.cus, cuRange{lo: rg[0], hi: rg[1], off: e.Offset})
					}
				}
			}
		}
		r.SkipChildren()
	}
	sort.Slice(s.cus, func(i, j int) bool { return s.cus[i].lo < s.cus[j].lo })
}

func (s *Symbolizer) dwarfLookup(addr uint64) []Loc {
	// candidate CUs: ranges may overlap in odd producers, so scan backwards from the last lo <= addr
	i := sort.Search(len(s.cus), func(i int) bool { return s.cus[i].lo > addr })
	for j := i - 1; j >= 0 && i-j <= 64; j-- {
		c := s.cus[j]
		if addr >= c.lo && addr < c.hi {
			if locs := s.lookupInCU(c.off, addr); len(locs) > 0 {
				return locs
			}
		}
	}
	return nil
}

func (s *Symbolizer) contains(e *dwarf.Entry, addr uint64) bool {
	rs, err := s.d.Ranges(e)
	if err != nil {
		return false
	}
	for _, r := range rs {
		if addr >= r[0] && addr < r[1] {
			return true
		}
	}
	return false
}

func (s *Symbolizer) lookupInCU(off dwarf.Offset, addr uint64) []Loc {
	r := s.d.Reader()
	r.Seek(off)
	cu, err := r.Next()
	if err != nil || cu == nil {
		return nil
	}
	lr, _ := s.d.LineReader(cu)
	var files []*dwarf.LineFile
	if lr != nil {
		files = lr.Files()
	}
	var chain []*dwarf.Entry
	if cu.Children {
		chain = s.findChain(r, addr)
	}
	// innermost location from the line table
	var le dwarf.LineEntry
	haveLine := false
	if lr != nil && lr.SeekPC(addr, &le) == nil {
		haveLine = true
	}
	if len(chain) == 0 {
		if !haveLine {
			return nil
		}
		return []Loc{{File: fileName(le.File), Line: le.Line, Col: le.Column}}
	}
	locs := make([]Loc, len(chain))
	for k := len(chain) - 1; k >= 0; k-- {
		name, linkage := s.names(chain[k])
		loc := Loc{Fn: name, Linkage: linkage, Inlined: k > 0}
		if k == len(chain)-1 {
			if haveLine {
				loc.File, loc.Line, loc.Col = fileName(le.File), le.Line, le.Column
			}
		} else {
			// our position is the call site of the inlined child
			inl := chain[k+1]
			if fi, ok := inl.Val(dwarf.AttrCallFile).(int64); ok && fi >= 0 && int(fi) < len(files) {
				loc.File = fileName(files[fi])
			}
			if ln, ok := inl.Val(dwarf.AttrCallLine).(int64); ok {
				loc.Line = int(ln)
			}
			if cl, ok := inl.Val(dwarf.AttrCallColumn).(int64); ok {
				loc.Col = int(cl)
			}
		}
		locs[len(chain)-1-k] = loc
	}
	return locs
}

// findChain walks the children of the current CU and returns [subprogram, inlined..., innermost inlined].
func (s *Symbolizer) findChain(r *dwarf.Reader, addr uint64) []*dwarf.Entry {
	var chain []*dwarf.Entry
	depth := 1  // inside the CU
	skipTo := 0 // > 0: ignore entries until depth returns to skipTo (manual skip: Reader.SkipChildren
	// trusts DW_AT_sibling, which some Swift/dsymutil output gets wrong)
	for depth > 0 {
		e, err := r.Next()
		if err != nil || e == nil {
			break
		}
		if e.Tag == 0 { // end of siblings
			depth--
			if skipTo > 0 && depth <= skipTo {
				skipTo = 0
			}
			continue
		}
		if skipTo > 0 {
			if e.Children {
				depth++
			}
			continue
		}
		relevant := e.Tag == dwarf.TagSubprogram || e.Tag == dwarf.TagInlinedSubroutine
		if relevant && s.contains(e, addr) {
			chain = append(chain, e)
			if e.Children {
				depth++
			}
			continue
		}
		if e.Children {
			// lexical blocks / namespaces / classes may contain subprograms: descend; skip everything else
			switch e.Tag {
			// (Swift closures / C++ lambdas are subprograms nested in their enclosing subprogram)
			case dwarf.TagSubprogram, dwarf.TagLexDwarfBlock, dwarf.TagNamespace, dwarf.TagClassType, dwarf.TagStructType, dwarf.TagModule:
				depth++
			default:
				skipTo = depth
				depth++
			}
		}
	}
	// keep a proper nesting: the innermost subprogram containing addr and the inlined subroutines after it
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Tag == dwarf.TagSubprogram {
			out := []*dwarf.Entry{chain[i]}
			for _, x := range chain[i+1:] {
				if x.Tag == dwarf.TagInlinedSubroutine {
					out = append(out, x)
				}
			}
			return out
		}
	}
	return nil
}

// names resolves name and linkage name following abstract_origin / specification.
func (s *Symbolizer) names(e *dwarf.Entry) (name, linkage string) {
	for depth := 0; e != nil && depth < 6; depth++ {
		if name == "" {
			name, _ = e.Val(dwarf.AttrName).(string)
		}
		if linkage == "" {
			if l, ok := e.Val(dwarf.AttrLinkageName).(string); ok {
				linkage = l
			} else if l, ok := e.Val(dwarf.Attr(0x2007) /* DW_AT_MIPS_linkage_name */).(string); ok {
				linkage = l
			}
		}
		if name != "" && linkage != "" {
			break
		}
		ref, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
		if !ok {
			ref, ok = e.Val(dwarf.AttrSpecification).(dwarf.Offset)
		}
		if !ok {
			break
		}
		r := s.d.Reader()
		r.Seek(ref)
		next, err := r.Next()
		if err != nil {
			break
		}
		e = next
	}
	if name == "" {
		name = linkage
	}
	return name, linkage
}

func fileName(f *dwarf.LineFile) string {
	if f == nil {
		return ""
	}
	return f.Name
}

// String renders a Loc for debugging.
func (l Loc) String() string {
	return fmt.Sprintf("%s (%s:%d:%d)%s", l.Fn, l.File, l.Line, l.Col, map[bool]string{true: " [inlined]"}[l.Inlined])
}

// ShortFile returns the last path element of a DWARF file name ("file:///a/b/main.dart" → "main.dart").
func ShortFile(f string) string {
	if i := strings.LastIndexByte(f, '/'); i >= 0 {
		return f[i+1:]
	}
	return f
}
