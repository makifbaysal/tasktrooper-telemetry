package retrace

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/frame"
)

var (
	// "\tat a.b.c(SourceFile:12)", "at a.b.c(Unknown Source:3)", "at a.b.c(Native Method)", "at a.b$c.d(Foo.kt)"
	frameRe = regexp.MustCompile(`^(\s*)at\s+(?:[\w.$-]+/)?([\w$.<>-]+)\.([\w$<>-]+)\(([^)]*)\)\s*$`)
	// "java.lang.IllegalStateException: msg", "Caused by: a.b: msg", "Suppressed: a.b"
	headerRe = regexp.MustCompile(`^(\s*(?:Caused by: |Suppressed: )?)([A-Za-z_$][\w$]*(?:\.[\w$]+)+)(:.*)?$`)
)

// Result is the outcome of retracing a stack.
type Result struct {
	Frames       []frame.Frame
	Stack        string
	Symbolicated bool
}

// Retrace deobfuscates a Java/Kotlin stack trace with m (m may be nil: frames are parsed only).
func Retrace(stack string, m *Mapping) Result {
	var res Result
	var out []string
	thrown := ""
	top := false
	for _, raw := range strings.Split(strings.ReplaceAll(stack, "\r\n", "\n"), "\n") {
		if fm := frameRe.FindStringSubmatch(raw); fm != nil {
			indent, cls, method, loc := fm[1], fm[2], fm[3], fm[4]
			file, line := splitLoc(loc)
			var rs []Retraced
			ok := false
			if m != nil {
				rs, ok = m.Frame(cls, method, line, thrown, top)
			}
			top = false
			if !ok {
				res.Frames = append(res.Frames, frame.Frame{Fn: cls + "." + method, File: file, Line: line, Module: cls,
					InApp: inApp(cls), Raw: raw})
				out = append(out, raw)
				continue
			}
			res.Symbolicated = true
			for _, r := range rs {
				f := frame.Frame{Fn: r.Class + "." + r.Method, File: r.File, Line: r.Line, Module: r.Class,
					InApp: inApp(r.Class), Symbolicated: true, Inlined: r.Inlined, Raw: raw}
				res.Frames = append(res.Frames, f)
				loc := r.File
				if r.Line > 0 {
					loc += ":" + strconv.Itoa(r.Line)
				}
				if loc == "" {
					loc = "Unknown Source"
				}
				out = append(out, fmt.Sprintf("%sat %s.%s(%s)", indentOr(indent), r.Class, r.Method, loc))
				for _, alt := range r.Alternatives {
					out = append(out, fmt.Sprintf("%s<OR> at %s(%s)", indentOr(indent), alt, r.File))
				}
			}
			continue
		}
		if hm := headerRe.FindStringSubmatch(raw); hm != nil {
			cls := hm[2]
			if m != nil {
				if orig, ok := m.OriginalClass(cls); ok {
					cls = orig
					res.Symbolicated = true
				}
			}
			thrown, top = cls, true
			out = append(out, hm[1]+cls+hm[3])
			continue
		}
		out = append(out, raw)
	}
	res.Stack = strings.Join(out, "\n")
	return res
}

func indentOr(s string) string {
	if s == "" {
		return "\t"
	}
	return s
}

func splitLoc(loc string) (string, int) {
	if i := strings.LastIndexByte(loc, ':'); i >= 0 {
		if n, err := strconv.Atoi(loc[i+1:]); err == nil {
			return loc[:i], n
		}
	}
	return loc, 0
}

var frameworkPrefixes = []string{
	"java.", "javax.", "jdk.", "sun.", "com.sun.", "android.", "androidx.", "com.android.", "com.google.android.",
	"dalvik.", "libcore.", "kotlin.", "kotlinx.", "org.jetbrains.", "okhttp3.", "okio.", "retrofit2.",
	"com.squareup.", "io.reactivex.", "dev.tasktrooper.", "io.flutter.", "com.facebook.react.",
}

func inApp(cls string) bool {
	for _, p := range frameworkPrefixes {
		if strings.HasPrefix(cls, p) {
			return false
		}
	}
	return true
}
