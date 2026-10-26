package native

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/frame"
)

var (
	// Android tombstone / debuggerd frame (Android SDK NDK crash format):
	//   "#01 pc 00000000000003e0  /data/app/…/lib/arm64/libshop.so (compute_total+68) (BuildId: 1b7d…)"
	tombFrameRe   = regexp.MustCompile(`^\s*#(\d+)\s+pc\s+([0-9a-fA-F]+)\s+(\S+)(.*)$`)
	tombBuildIDRe = regexp.MustCompile(`\(BuildId:\s*([0-9a-fA-F]+)\)`)
	tombSymbolRe  = regexp.MustCompile(`\(([^()]+?)\+(\d+)\)`)
)

// IsTombstone reports whether stack contains Android native (tombstone-format) frames.
func IsTombstone(stack string) bool {
	for _, l := range strings.Split(stack, "\n") {
		if m := tombFrameRe.FindStringSubmatch(l); m != nil && strings.Contains(m[4], "BuildId:") {
			return true
		}
	}
	return false
}

// SymbolicateTombstone symbolicates Android NDK frames with unstripped ELF libraries matched by GNU build id.
// "pc" is the ELF-relative address; the SDK already adjusts frames ≥ 1 to the call instruction
// (−4 arm64, −2 thumb, −1 x86), so no further adjustment is applied.
func SymbolicateTombstone(stack string, provide Provider) Result {
	var res Result
	missing := map[string]bool{}
	lines := strings.Split(strings.ReplaceAll(stack, "\r\n", "\n"), "\n")
	for li, l := range lines {
		m := tombFrameRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		pc, err := strconv.ParseUint(m[2], 16, 64)
		if err != nil {
			continue
		}
		lib := m[3]
		base := frame.Frame{Addr: fmt.Sprintf("0x%x", pc), Module: libName(lib), Raw: l, InApp: isAppLib(lib)}
		if sm := tombSymbolRe.FindStringSubmatch(m[4]); sm != nil && !strings.HasPrefix(sm[1], "BuildId") && !strings.HasPrefix(sm[1], "offset") {
			base.Fn = sm[1]
		}
		id := ""
		if bm := tombBuildIDRe.FindStringSubmatch(m[4]); bm != nil {
			id = NormalizeID(bm[1])
		}
		var locs []Loc
		ok := false
		if id != "" {
			if sym, found := provide(id); found && sym != nil {
				locs, ok = sym.Lookup(pc)
			} else if base.InApp {
				missing[id] = true
			}
		}
		if !ok {
			res.Frames = append(res.Frames, base)
			continue
		}
		res.Symbolicated = true
		indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		var rendered []string
		for _, loc := range locs {
			f := base
			f.Fn, f.File, f.Line, f.Col, f.Inlined, f.Symbolicated = loc.Fn, loc.File, loc.Line, loc.Col, loc.Inlined, true
			if loc.Linkage != "" && loc.Linkage != loc.Fn {
				f.Mangled = loc.Linkage
			}
			res.Frames = append(res.Frames, f)
			inl := ""
			if f.Inlined {
				inl = " [inlined]"
			}
			rendered = append(rendered, fmt.Sprintf("%s#%s pc %s  %s (%s) (%s:%d)%s", indent, m[1], m[2], lib, f.Fn, ShortFile(f.File), f.Line, inl))
		}
		lines[li] = strings.Join(rendered, "\n")
	}
	for id := range missing {
		res.MissingIDs = append(res.MissingIDs, id)
	}
	res.Stack = strings.Join(lines, "\n")
	return res
}

func libName(p string) string {
	if i := strings.LastIndexAny(p, "/!"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func isAppLib(p string) bool {
	for _, pre := range []string{"/system/", "/apex/", "/vendor/", "/product/", "/system_ext/", "[anon", "[vdso]"} {
		if strings.HasPrefix(p, pre) {
			return false
		}
	}
	return true
}
