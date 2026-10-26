package native

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/frame"
)

var (
	dartBuildIDRe = regexp.MustCompile(`build_id:\s*'([0-9a-fA-F]+)'`)
	dartBaseRe    = regexp.MustCompile(`isolate_dso_base:\s*([0-9a-fA-F]+)`)
	// "    #00 abs 0000000104e21623 virt 0000000000041623 _kDartIsolateSnapshotInstructions+0x31623"
	dartFrameRe = regexp.MustCompile(`^\s*#(\d+)\s+abs\s+([0-9a-fA-F]+)(?:\s+virt\s+([0-9a-fA-F]+))?(?:\s+(.*))?$`)
)

// IsDartTrace reports whether stack is a non-symbolic Dart AOT trace (--split-debug-info / --obfuscate).
func IsDartTrace(stack string) bool {
	return strings.Contains(stack, "*** *** ***") && dartBuildIDRe.MatchString(stack)
}

// DartBuildID returns the normalized build id of a Dart non-symbolic trace.
func DartBuildID(stack string) string {
	if m := dartBuildIDRe.FindStringSubmatch(stack); m != nil {
		return NormalizeID(m[1])
	}
	return ""
}

// SymbolicateDart maps the "virt" addresses of a Dart AOT trace through the --split-debug-info symbols file
// (ELF on Android, Mach-O companion on iOS/macOS) whose build id matches.
func SymbolicateDart(stack string, provide Provider) Result {
	var res Result
	id := DartBuildID(stack)
	var base uint64
	if m := dartBaseRe.FindStringSubmatch(stack); m != nil {
		base, _ = strconv.ParseUint(m[1], 16, 64)
	}
	var sym *Symbolizer
	if id != "" {
		if s, ok := provide(id); ok {
			sym = s
		} else {
			res.MissingIDs = []string{id}
		}
	}
	lines := strings.Split(strings.ReplaceAll(stack, "\r\n", "\n"), "\n")
	var out []string
	n := 0
	for _, l := range lines {
		m := dartFrameRe.FindStringSubmatch(l)
		if m == nil {
			out = append(out, l)
			continue
		}
		abs, _ := strconv.ParseUint(m[2], 16, 64)
		virt := abs - base
		if m[3] != "" {
			virt, _ = strconv.ParseUint(m[3], 16, 64)
		}
		f := frame.Frame{Fn: strings.TrimSpace(m[4]), Addr: fmt.Sprintf("0x%x", virt), Raw: l, InApp: true}
		var locs []Loc
		ok := false
		if sym != nil {
			locs, ok = sym.Lookup(virt)
		}
		if !ok {
			res.Frames = append(res.Frames, f)
			out = append(out, l)
			continue
		}
		res.Symbolicated = true
		for _, loc := range locs {
			g := f
			g.Fn, g.File, g.Line, g.Col, g.Inlined, g.Symbolicated = loc.Fn, loc.File, loc.Line, loc.Col, loc.Inlined, true
			g.InApp = !strings.HasPrefix(loc.File, "dart:") && !strings.HasPrefix(loc.File, "package:flutter/")
			res.Frames = append(res.Frames, g)
			out = append(out, fmt.Sprintf("#%-7d %s (%s:%d:%d)", n, g.Fn, g.File, g.Line, g.Col))
			n++
		}
	}
	res.Stack = strings.Join(out, "\n")
	return res
}
