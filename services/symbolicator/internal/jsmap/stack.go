package jsmap

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/frame"
)

// Bundle is a source map plus (optionally) the minified file it belongs to.
type Bundle struct {
	Map      *Map
	Minified string // minified JS text, "" when not uploaded
}

// Resolver finds the bundle for a frame URL (e.g. https://cdn/x/app.min.js).
type Resolver func(url string) (*Bundle, bool)

// RawFrame is a parsed (still minified) JS stack line.
type RawFrame struct {
	Fn     string
	URL    string
	Line   int
	Col    int
	Prefix string // "async " / "new "
}

var (
	// V8: "    at fn (url:1:2)", "    at url:1:2", "    at async fn (url:1:2)", "    at new Foo (url:1:2)"
	chromeRe = regexp.MustCompile(`^\s*at\s+(?:(async\s+|new\s+)?(.+?)\s+\()?((?:[a-zA-Z][\w+.-]*:)?[^()]*?):(\d+):(\d+)\)?\s*$`)
	// SpiderMonkey / JavaScriptCore: "fn@url:1:2", "@url:1:2", "fn/<@url:1:2"
	geckoRe = regexp.MustCompile(`^\s*([^@\s]*)@(.+?):(\d+):(\d+)\s*$`)
)

// ParseLine parses one stack line; ok=false for message lines and unknown formats.
func ParseLine(s string) (RawFrame, bool) {
	if m := chromeRe.FindStringSubmatch(s); m != nil {
		line, _ := strconv.Atoi(m[4])
		col, _ := strconv.Atoi(m[5])
		return RawFrame{Prefix: m[1], Fn: m[2], URL: m[3], Line: line, Col: col}, true
	}
	if m := geckoRe.FindStringSubmatch(s); m != nil {
		line, _ := strconv.Atoi(m[3])
		col, _ := strconv.Atoi(m[4])
		return RawFrame{Fn: m[1], URL: m[2], Line: line, Col: col}, true
	}
	return RawFrame{}, false
}

// Result is the outcome of symbolicating a JS stack.
type Result struct {
	Frames       []frame.Frame
	Stack        string
	Symbolicated bool
}

// Symbolicate maps every recognisable frame of stack through the bundles returned by resolve.
// Lines that are not frames (the "Error: msg" header) are kept verbatim.
func Symbolicate(stack string, resolve Resolver) Result {
	lines := strings.Split(strings.ReplaceAll(stack, "\r\n", "\n"), "\n")
	type item struct {
		raw    string
		rf     RawFrame
		isFr   bool
		bundle *Bundle
	}
	items := make([]item, len(lines))
	for i, l := range lines {
		items[i].raw = l
		if rf, ok := ParseLine(l); ok {
			items[i].rf, items[i].isFr = rf, true
			if b, ok := resolve(rf.URL); ok && b != nil && b.Map != nil {
				items[i].bundle = b
			}
		}
	}
	var res Result
	var out []string
	for i, it := range items {
		if !it.isFr {
			out = append(out, it.raw)
			continue
		}
		f := frame.Frame{Fn: it.rf.Fn, File: it.rf.URL, Line: it.rf.Line, Col: it.rf.Col, Module: it.rf.URL, Raw: it.raw,
			InApp: isAppURL(it.rf.URL)}
		if it.bundle != nil {
			if mp, ok := it.bundle.Map.Lookup(it.rf.Line, it.rf.Col); ok {
				f.File, f.Line, f.Col, f.Symbolicated = mp.Source, mp.Line, mp.Col, true
				f.InApp = isAppSource(mp.Source)
				f.Fn = functionName(it.bundle, it.rf, nextCaller(items[i+1:], func(x item) (RawFrame, *Bundle, bool) {
					return x.rf, x.bundle, x.isFr
				}))
				res.Symbolicated = true
			}
		}
		res.Frames = append(res.Frames, f)
		out = append(out, formatFrame(it.rf, f))
	}
	res.Stack = strings.Join(out, "\n")
	return res
}

func nextCaller[T any](rest []T, get func(T) (RawFrame, *Bundle, bool)) *callSite {
	for _, x := range rest {
		rf, b, ok := get(x)
		if !ok {
			continue
		}
		return &callSite{rf: rf, bundle: b}
	}
	return nil
}

type callSite struct {
	rf     RawFrame
	bundle *Bundle
}

// functionName recovers the original name of the function a frame is in:
//  1. with the minified source: find the declaration of the minified name before the position and read the
//     name the map records for that token;
//  2. otherwise the caller's call-site token (callers call us by our original name);
//  3. otherwise the minified name.
func functionName(b *Bundle, rf RawFrame, caller *callSite) string {
	minName := rf.Fn
	short := minName
	if i := strings.LastIndexByte(short, '.'); i >= 0 {
		short = short[i+1:]
	}
	short = strings.TrimSuffix(short, " [as "+short+"]")
	if b.Minified != "" && short != "" && short != "<anonymous>" {
		if line, col, ok := findDeclaration(b.Minified, short, rf.Line-1, rf.Col-1); ok {
			if n := b.Map.exactName(line, col); n != "" {
				return replaceShort(minName, short, n)
			}
		}
	}
	if caller != nil && caller.bundle != nil && caller.bundle.Map == b.Map {
		if s, ok := b.Map.segmentAt(caller.rf.Line-1, caller.rf.Col-1); ok && s.name >= 0 {
			return replaceShort(minName, short, b.Map.Names[s.name])
		}
	}
	if minName == "" {
		return "<anonymous>"
	}
	return minName
}

func replaceShort(full, short, orig string) string {
	if full == "" || short == "" {
		return orig
	}
	if strings.HasSuffix(full, short) {
		return full[:len(full)-len(short)] + orig
	}
	return orig
}

// findDeclaration searches backwards from (line, col) (0-based) for a function/method declaration named name.
func findDeclaration(src, name string, line, col int) (int, int, bool) {
	lines := strings.Split(src, "\n")
	if line < 0 || line >= len(lines) {
		return 0, 0, false
	}
	for l := line; l >= 0 && line-l < 2000; l-- {
		text := lines[l]
		end := len(text)
		if l == line && col >= 0 && col < end {
			end = col
		}
		pos := end
		for pos > 0 {
			i := strings.LastIndex(text[:pos], name)
			if i < 0 {
				break
			}
			pos = i
			if isDeclaration(text, i, len(name)) {
				return l, i, true
			}
		}
	}
	return 0, 0, false
}

var (
	afterAssignRe = regexp.MustCompile(`^\s*[=:]\s*(?:async\s*)?(?:function\b|\(|[A-Za-z_$][\w$]*\s*=>)`)
	beforeFuncRe  = regexp.MustCompile(`function\s*\*?\s*$`)
)

func isDeclaration(text string, i, n int) bool {
	if i > 0 && isIdent(text[i-1]) || i+n < len(text) && isIdent(text[i+n]) {
		return false // part of a longer identifier
	}
	if i > 0 && text[i-1] == '.' && !strings.HasSuffix(text[:i], "prototype.") {
		return false
	}
	if beforeFuncRe.MatchString(text[:i]) {
		return true
	}
	rest := text[i+n:]
	if afterAssignRe.MatchString(rest) {
		return true
	}
	// method shorthand: name(args){ preceded by { } ; , static async get set or start of line
	if strings.HasPrefix(rest, "(") {
		depth := 0
		for j := 0; j < len(rest); j++ {
			switch rest[j] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					tail := strings.TrimLeft(rest[j+1:], " \t")
					if !strings.HasPrefix(tail, "{") {
						return false
					}
					head := strings.TrimRight(text[:i], " \t")
					for _, kw := range []string{"static", "async", "get", "set", "*"} {
						head = strings.TrimRight(strings.TrimSuffix(head, kw), " \t")
					}
					return head == "" || strings.HasSuffix(head, "{") || strings.HasSuffix(head, "}") ||
						strings.HasSuffix(head, ";") || strings.HasSuffix(head, ",")
				}
			}
		}
	}
	return false
}

func formatFrame(rf RawFrame, f frame.Frame) string {
	fn := f.Fn
	if fn == "" {
		fn = "<anonymous>"
	}
	if f.Symbolicated {
		return fmt.Sprintf("    at %s%s (%s:%d:%d)", rf.Prefix, fn, f.File, f.Line, f.Col)
	}
	return f.Raw
}

func isAppSource(src string) bool {
	return !strings.Contains(src, "node_modules/") && !strings.Contains(src, "webpack/bootstrap") &&
		!strings.HasPrefix(src, "webpack/runtime") && !strings.Contains(src, "(webpack)")
}

func isAppURL(u string) bool {
	if u == "" || u == "<anonymous>" || u == "native" || strings.HasPrefix(u, "node:") ||
		strings.HasPrefix(u, "chrome-extension:") || strings.HasPrefix(u, "moz-extension:") ||
		strings.HasPrefix(u, "safari-extension:") || strings.HasPrefix(u, "[native code]") {
		return false
	}
	return isAppSource(u)
}
