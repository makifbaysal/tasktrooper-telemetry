package extract

import (
	"bytes"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

// Swift (iOS / server-side Swift) extractor. The source is masked by cleanSwift (comments blanked, string contents
// hidden from the code view) and scanned with regexes; string values are read back from the unmasked view at the
// same offsets, so nothing inside a comment or a string literal is ever matched as code.
//
// Views:
//   - UIKit: classes whose superclass is UIViewController / UITableViewController / UICollectionViewController (or an
//     in-repo subclass of those, transitively up to 3 levels). Key = class name, which is what the iOS SDK reports.
//     Container controllers (UINavigationController, UITabBarController, UIPageViewController, UISplitViewController)
//     are not views: the SDK ignores them.
//   - SwiftUI: a top-level (brace depth 0) `struct X: View` is a view when its name ends with Screen or Page, or when it
//     is used as a navigation target anywhere in the repo: the destination of NavigationLink / .navigationDestination /
//     .sheet / .fullScreenCover / .popover, a direct child of TabView / Tab / WindowGroup / NavigationStack /
//     NavigationView / NavigationSplitView, or the receiver of `.trooperView("Name")`. Other `*View` structs are
//     treated as subviews and skipped. Key = type name; `.trooperView` / `trooperViewName` names go to meta.rumName.
//
// Endpoints: Vapor `app.get("orders", ":id")`, `on(.POST, …)`, `grouped(…)` variables, `group(…) { g in }`
// closures and RouteCollection `boot(routes:)` mounted with `register(collection:)` (best effort, per file order).
//
// Dependencies: absolute URL literals in `URL(string:)` (or anywhere in files using URLSession / Alamofire), and
// Firestore, Realm, SQLite/GRDB, Fluent drivers and Redis imports.

var (
	reSwImport       = regexp.MustCompile(`(?m)^[ \t]*(?:@\w+[ \t]+)*import[ \t]+(?:(?:class|struct|enum|protocol|func|var|let|typealias)[ \t]+)?(\w+)`)
	reSwType         = regexp.MustCompile(`(?m)^[ \t]*(?:(?:@\w+(?:\([^)\n]*\))?|public|private|fileprivate|internal|open|final|package|nonisolated)\s+)*(struct|class|actor|enum)\s+(\w+)\s*(?:<[^>{]*>)?\s*(?::\s*([^{]*?))?\s*(?:where\s[^{]*)?\{`)
	reSwExt          = regexp.MustCompile(`(?m)^[ \t]*(?:(?:public|private|fileprivate|internal|package)\s+)?extension\s+([\w.]+)\s*(?::\s*([^{]*?))?\s*(?:where\s[^{]*)?\{`)
	reSwNavKw        = regexp.MustCompile(`\b(NavigationLink|navigationDestination|sheet|fullScreenCover|popover|TabView|Tab|WindowGroup|NavigationStack|NavigationView|NavigationSplitView)\b`)
	reSwTrooperV     = regexp.MustCompile(`\b([A-Z]\w*)\s*\([^()]*\)\s*\.trooperView\s*\(\s*`)
	reSwTrooperN     = regexp.MustCompile(`\btrooperViewName\b[^{]*\{\s*(?:return\s+)?`)
	reSwURLInit      = regexp.MustCompile(`\bURL\s*\(\s*string:\s*$`)
	reSwRoute        = regexp.MustCompile(`\b(\w+)\s*\.\s*(get|post|put|patch|delete|on|webSocket)\s*\(`)
	reSwGrouped      = regexp.MustCompile(`\b(?:let|var)\s+(\w+)\s*(?::\s*[\w& ]+)?=\s*(?:try\s+)?(\w+)\s*\.\s*grouped\s*\(`)
	reSwGroup        = regexp.MustCompile(`\b(\w+)\s*\.\s*group\s*\(`)
	reSwRegister     = regexp.MustCompile(`\b(\w+)\s*\.\s*register\s*\(\s*collection:\s*(\w+)`)
	reSwBuilder      = regexp.MustCompile(`\b(\w+)\s*:\s*(?:any\s+)?(?:RoutesBuilder|Application)\b`)
	reSwBoot         = regexp.MustCompile(`\bfunc\s+boot\s*\(\s*(\w+)(?:\s+(\w+))?\s*:`)
	reSwClosureParam = regexp.MustCompile(`^\{\s*(\w+)\s+in\b`)
	reSwParamCall    = regexp.MustCompile(`^\.parameter\s*\(\s*$`)
)

var swiftDepImports = map[string][2]string{
	"FirebaseFirestore": {"db", "firestore"}, "FirebaseFirestoreSwift": {"db", "firestore"},
	"FirebaseDatabase": {"db", "firebase-database"},
	"RealmSwift":       {"db", "realm"}, "Realm": {"db", "realm"},
	"SQLite": {"db", "sqlite"}, "SQLite3": {"db", "sqlite"}, "GRDB": {"db", "sqlite"}, "FluentSQLiteDriver": {"db", "sqlite"},
	"FluentPostgresDriver": {"db", "postgres"}, "PostgresNIO": {"db", "postgres"}, "PostgresKit": {"db", "postgres"},
	"FluentMySQLDriver": {"db", "mysql"}, "MySQLNIO": {"db", "mysql"},
	"FluentMongoDriver": {"db", "mongodb"}, "MongoKitten": {"db", "mongodb"},
	"Redis": {"cache", "redis"}, "RediStack": {"cache", "redis"},
}

// uikitBases are the UIKit controllers the iOS SDK tracks as views.
var uikitBases = map[string]bool{"UIViewController": true, "UITableViewController": true, "UICollectionViewController": true}

// swType is a type or extension declaration with its body range in the masked source.
type swType struct {
	kind, name string
	inherits   []string
	off, open  int // declaration start, '{'
	close      int // matching '}'
	depth      int // brace depth of the declaration
	ext        bool
	file       *mobSrc
}

func extractSwift(files []srcFile) []model.Item {
	var srcs []*mobSrc
	var types []*swType
	evidence := map[string]bool{}    // SwiftUI types used as navigation targets
	rumNames := map[string]string{}  // type → .trooperView / trooperViewName
	collMount := map[string]string{} // RouteCollection type → mount prefix (resolved per register call)
	var items []model.Item
	for _, f := range files {
		m := cleanSwift(f)
		srcs = append(srcs, m)
		ts := m.swiftTypes()
		types = append(types, ts...)
		m.swiftNavEvidence(evidence, rumNames)
		m.swiftViewNames(ts, rumNames)
	}
	// UIKit: superclass is a UIKit controller, or an in-repo controller (transitively, max 3 levels).
	vc := map[string]bool{}
	for range 3 {
		for _, t := range types {
			if !t.ext && t.kind == "class" && len(t.inherits) > 0 && (uikitBases[t.inherits[0]] || vc[t.inherits[0]]) {
				vc[t.name] = true
			}
		}
	}
	for _, t := range types {
		if t.ext {
			continue
		}
		fw := ""
		switch {
		case vc[t.name] && t.kind == "class":
			fw = "uikit"
		case t.depth == 0 && t.kind == "struct" && slices.Contains(t.inherits, "View") &&
			(strings.HasSuffix(t.name, "Screen") || strings.HasSuffix(t.name, "Page") || evidence[t.name]):
			fw = "swiftui"
		}
		if fw == "" {
			continue
		}
		line := lineAt(t.file.code, t.off)
		it := model.Item{Kind: model.KindView, Key: t.name, File: t.file.f.rel, Line: line, Framework: fw, Handler: t.name,
			Language: "swift", Snippet: braceBlock(t.file.f.src, line)}
		if n := rumNames[t.name]; n != "" {
			it.Meta = map[string]any{"rumName": n}
		}
		items = append(items, it)
	}
	// Vapor: collect register(collection:) mounts first (they may live in another file), then routes.
	var pending []swVaporRoute
	for _, m := range srcs {
		if !m.imports("Vapor") {
			continue
		}
		pending = append(pending, m.vaporRoutes(types, collMount)...)
	}
	for _, r := range pending {
		prefix := ""
		if r.coll != "" {
			prefix = collMount[r.coll]
		}
		r.item.Key = route.Key(r.method, route.Join(prefix, r.path))
		items = append(items, r.item)
	}
	for _, m := range srcs {
		items = append(items, m.swiftDeps()...)
	}
	return items
}

// cleanSwift masks a Swift source: `//` and nested `/* */` comments are blanked; string literal contents (including
// `"""` blocks, `#"raw"#` strings and `\( )` interpolations) are blanked in the code view only. Line numbers and byte
// offsets are preserved.
func cleanSwift(f srcFile) *mobSrc {
	code, full := mobMask(f.src, false)
	return &mobSrc{f: f, code: code, full: full}
}

func (m *mobSrc) imports(mod string) bool {
	for _, x := range reSwImport.FindAllSubmatch(m.code, -1) {
		if string(x[1]) == mod {
			return true
		}
	}
	return false
}

// swiftTypes returns type and extension declarations with their body ranges and brace depth.
func (m *mobSrc) swiftTypes() []*swType {
	var out []*swType
	add := func(kind, name, inh string, off, open int, ext bool) {
		t := &swType{kind: kind, name: name, off: off, open: open, close: mobClose(m.code, open), ext: ext, file: m}
		for _, s := range strings.Split(inh, ",") {
			s = strings.TrimSpace(s)
			if i := strings.IndexAny(s, "<( "); i >= 0 {
				s = s[:i]
			}
			if i := strings.LastIndexByte(s, '.'); i >= 0 { // SwiftUI.View, UIKit.UIViewController
				s = s[i+1:]
			}
			if s != "" {
				t.inherits = append(t.inherits, s)
			}
		}
		out = append(out, t)
	}
	for _, x := range reSwType.FindAllSubmatchIndex(m.code, -1) {
		inh := ""
		if x[6] >= 0 {
			inh = string(m.code[x[6]:x[7]])
		}
		add(string(m.code[x[2]:x[3]]), string(m.code[x[4]:x[5]]), inh, x[0], x[1]-1, false)
	}
	for _, x := range reSwExt.FindAllSubmatchIndex(m.code, -1) {
		inh := ""
		if x[4] >= 0 {
			inh = string(m.code[x[4]:x[5]])
		}
		name := string(m.code[x[2]:x[3]])
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		add("extension", name, inh, x[0], x[1]-1, true)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].off < out[j].off })
	offs := make([]int, len(out))
	for i, t := range out {
		offs[i] = t.open
	}
	for i, d := range mobDepths(m.code, offs) {
		out[i].depth = d
	}
	return out
}

// swiftNavEvidence records types used as SwiftUI navigation targets and `.trooperView("Name")` names.
func (m *mobSrc) swiftNavEvidence(evidence map[string]bool, rumNames map[string]string) {
	code := m.code
	for _, x := range reSwNavKw.FindAllSubmatchIndex(code, -1) {
		kw := string(code[x[2]:x[3]])
		if x[0] > 0 && code[x[0]-1] == '.' && (kw == "Tab" || kw == "TabView" || kw == "WindowGroup" || strings.HasPrefix(kw, "Navigation")) {
			continue // a member (`.Tab`), not the container
		}
		j := mobSkipSpace(code, x[1])
		closureIsTarget := true
		if j < len(code) && code[j] == '(' {
			c := mobClose(code, j)
			if c < 0 {
				continue
			}
			for _, a := range mobArgs(code, j, c) {
				label, vs := mobLabel(code, a[0], a[1])
				switch label {
				case "destination":
					if n := mobFirstCall(code, vs, a[1]); n != "" {
						evidence[n] = true
					}
					closureIsTarget = false
				case "value":
					closureIsTarget = false
				}
			}
			j = mobSkipSpace(code, c+1)
		}
		if closureIsTarget && j < len(code) && code[j] == '{' {
			if c := mobClose(code, j); c > 0 {
				for _, n := range mobTopCalls(code, j+1, c) {
					evidence[n] = true
				}
			}
		}
	}
	for _, x := range reSwTrooperV.FindAllSubmatchIndex(code, -1) {
		name := string(code[x[2]:x[3]])
		evidence[name] = true
		if v, _, ok := m.str(x[1]); ok && v != "" {
			rumNames[name] = v
		}
	}
}

// swiftViewNames maps `trooperViewName` (TrooperViewNaming) values to their enclosing type or extension.
func (m *mobSrc) swiftViewNames(ts []*swType, rumNames map[string]string) {
	for _, x := range reSwTrooperN.FindAllIndex(m.code, -1) {
		v, _, ok := m.str(x[1])
		if !ok || v == "" {
			continue
		}
		var owner *swType
		for _, t := range ts {
			if t.open < x[0] && x[0] < t.close && (owner == nil || t.open > owner.open) {
				owner = t
			}
		}
		if owner != nil {
			rumNames[owner.name] = v
		}
	}
}

type swVaporRoute struct {
	item         model.Item
	method, path string
	coll         string // RouteCollection the route is relative to ("" = absolute)
}

type swGroup struct{ coll, prefix string }

// vaporRoutes scans one Vapor file in source order, tracking `grouped` variables and `group` closure parameters.
// register(collection:) calls record the mount prefix of a collection in mounts.
func (m *mobSrc) vaporRoutes(types []*swType, mounts map[string]string) []swVaporRoute {
	code := m.code
	type ev struct {
		off  int
		kind string
		x    []int
	}
	var evs []ev
	for _, x := range reSwRoute.FindAllSubmatchIndex(code, -1) {
		evs = append(evs, ev{x[0], "route", x})
	}
	for _, x := range reSwGrouped.FindAllSubmatchIndex(code, -1) {
		evs = append(evs, ev{x[0], "grouped", x})
	}
	for _, x := range reSwGroup.FindAllSubmatchIndex(code, -1) {
		evs = append(evs, ev{x[0], "group", x})
	}
	for _, x := range reSwRegister.FindAllSubmatchIndex(code, -1) {
		evs = append(evs, ev{x[0], "register", x})
	}
	for _, x := range reSwBoot.FindAllSubmatchIndex(code, -1) {
		evs = append(evs, ev{x[0], "boot", x})
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].off < evs[j].off })

	var colls []*swType
	for _, t := range types {
		if t.file == m && !t.ext && slices.Contains(t.inherits, "RouteCollection") {
			colls = append(colls, t)
		}
	}
	for _, t := range types { // extension X: RouteCollection
		if t.file == m && t.ext && slices.Contains(t.inherits, "RouteCollection") {
			colls = append(colls, t)
		}
	}
	collAt := func(off int) string {
		for _, t := range colls {
			if t.open < off && off < t.close {
				return t.name
			}
		}
		return ""
	}
	builders := map[string]bool{"app": true}
	for _, x := range reSwBuilder.FindAllSubmatch(code, -1) {
		builders[string(x[1])] = true
	}
	groups := map[string]swGroup{}
	resolve := func(recv string, off int) (swGroup, bool) {
		if g, ok := groups[recv]; ok {
			return g, true
		}
		if builders[recv] {
			return swGroup{coll: collAt(off)}, true
		}
		return swGroup{}, false
	}
	// groupedPrefix reads `.grouped("a", ":b")` (and chained `.grouped(…)`) starting at the '(' at open.
	groupedPrefix := func(open int) (string, int) {
		p := ""
		for {
			c := mobClose(code, open)
			if c < 0 {
				return p, open
			}
			segs, _, _ := m.vaporArgs(open, c)
			p = route.Join(p, strings.Join(segs, "/"))
			k := mobSkipSpace(code, c+1)
			if k < len(code) && code[k] == '.' {
				k2 := mobSkipSpace(code, k+1)
				if bytes.HasPrefix(code[k2:], []byte("grouped")) {
					if o := mobSkipSpace(code, k2+len("grouped")); o < len(code) && code[o] == '(' {
						open = o
						continue
					}
				}
			}
			return p, c
		}
	}
	var out []swVaporRoute
	for _, e := range evs {
		x := e.x
		switch e.kind {
		case "boot":
			name := string(code[x[2]:x[3]])
			if x[4] >= 0 {
				name = string(code[x[4]:x[5]])
			}
			groups[name] = swGroup{coll: collAt(e.off)}
		case "grouped":
			g, ok := resolve(string(code[x[4]:x[5]]), e.off)
			if !ok {
				continue
			}
			p, _ := groupedPrefix(x[1] - 1)
			groups[string(code[x[2]:x[3]])] = swGroup{coll: g.coll, prefix: route.Join(g.prefix, p)}
		case "group":
			g, ok := resolve(string(code[x[2]:x[3]]), e.off)
			if !ok {
				continue
			}
			p, c := groupedPrefix(x[1] - 1)
			// `{ orders in` names the closure parameter
			k := mobSkipSpace(code, c+1)
			if k < len(code) && code[k] == '{' {
				if pm := reSwClosureParam.FindSubmatch(code[k:min(len(code), k+80)]); pm != nil {
					groups[string(pm[1])] = swGroup{coll: g.coll, prefix: route.Join(g.prefix, p)}
				}
			}
		case "register":
			g, ok := resolve(string(code[x[2]:x[3]]), e.off)
			if !ok {
				continue
			}
			coll := string(code[x[4]:x[5]])
			prefix := g.prefix
			if g.coll != "" && g.coll != coll {
				prefix = route.Join(mounts[g.coll], prefix)
			}
			if _, seen := mounts[coll]; !seen {
				mounts[coll] = prefix
			}
		case "route":
			recv, verb := string(code[x[2]:x[3]]), string(code[x[4]:x[5]])
			g, ok := resolve(recv, e.off)
			if !ok {
				continue
			}
			open := x[1] - 1
			c := mobClose(code, open)
			if c < 0 {
				continue
			}
			segs, method, handler := m.vaporArgs(open, c)
			switch verb {
			case "on":
				if method == "" {
					continue
				}
			case "webSocket":
				method = "GET"
			default:
				method = strings.ToUpper(verb)
			}
			line := lineAt(code, e.off)
			snippet := ""
			if handler != "" {
				if fx := regexp.MustCompile(`\bfunc\s+` + regexp.QuoteMeta(handler) + `\s*[<(]`).FindIndex(code); fx != nil {
					snippet = braceBlock(m.f.src, lineAt(code, fx[0]))
				}
			}
			if snippet == "" {
				snippet = braceBlock(m.f.src, line)
			}
			out = append(out, swVaporRoute{
				item: model.Item{Kind: model.KindEndpoint, File: m.f.rel, Line: line, Framework: "vapor", Handler: handler,
					Language: "swift", Snippet: snippet},
				method: method, path: route.Join(g.prefix, "/"+strings.Join(segs, "/")), coll: g.coll,
			})
		}
	}
	return out
}

// vaporArgs reads Vapor path components, an `on(.METHOD, …)` method and the `use:` handler from call args.
func (m *mobSrc) vaporArgs(open, close int) (segs []string, method, handler string) {
	for _, a := range mobArgs(m.code, open, close) {
		label, vs := mobLabel(m.code, a[0], a[1])
		val := strings.TrimSpace(string(m.code[vs:a[1]]))
		switch {
		case label == "use":
			handler = strings.TrimPrefix(strings.TrimSuffix(val, ")"), "self.")
			if i := strings.IndexAny(handler, "( "); i >= 0 {
				handler = handler[:i]
			}
		case label != "":
		case strings.HasPrefix(val, `"`):
			if v, _, ok := m.str(mobSkipSpace(m.code, vs)); ok {
				if v == "**" {
					v = "*"
				}
				segs = append(segs, strings.Trim(v, "/"))
			}
		case val == ".catchall" || val == ".anything" || val == "**" || val == "*":
			segs = append(segs, "*")
		case strings.HasPrefix(val, ".parameter"):
			if i := bytes.IndexByte(m.code[vs:a[1]], '"'); i >= 0 && reSwParamCall.Match(bytes.TrimSpace(m.code[vs:vs+i])) {
				if v, _, ok := m.str(vs + i); ok {
					segs = append(segs, ":"+v)
				}
			}
		case strings.HasPrefix(val, ".constant"):
			if i := bytes.IndexByte(m.code[vs:a[1]], '"'); i >= 0 {
				if v, _, ok := m.str(vs + i); ok {
					segs = append(segs, v)
				}
			}
		case len(segs) == 0 && method == "" && strings.HasPrefix(val, ".") && val == strings.ToUpper(val):
			method = strings.TrimPrefix(val, ".")
		case strings.HasPrefix(val, "HTTPMethod.") || strings.HasPrefix(val, ".init("):
		}
	}
	return segs, method, handler
}

// swiftDeps returns driver imports and absolute URL literals.
func (m *mobSrc) swiftDeps() []model.Item {
	var out []model.Item
	for _, x := range reSwImport.FindAllSubmatchIndex(m.code, -1) {
		if d, ok := swiftDepImports[string(m.code[x[2]:x[3]])]; ok {
			out = append(out, depItem(d[0], d[1], m.f.rel, lineAt(m.code, x[0]), "swift"))
		}
	}
	network := m.imports("Alamofire") || m.imports("Moya") || bytes.Contains(m.code, []byte("URLSession")) ||
		bytes.Contains(m.code, []byte("URLRequest"))
	for _, l := range m.literals() {
		h := mobHost(l.val)
		if h == "" {
			continue
		}
		before := m.code[max(0, l.off-40):l.off]
		if network || reSwURLInit.Match(before) {
			out = append(out, depItem("http", h, m.f.rel, lineAt(m.code, l.off), "swift"))
		}
	}
	return out
}

// ---- shared helpers of the Swift and Dart scanners ----

// mobSrc is a source file masked for scanning. code and full have the same length as the source.
type mobSrc struct {
	f    srcFile
	code []byte // comments blanked, string literal contents blanked (delimiters kept)
	full []byte // comments blanked, strings kept
}

type mobDelim struct {
	open   int // delimiter length: prefix (r / #…) + quotes
	hashes int
	q      byte
	triple bool
	raw    bool
}

func mobIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// mobDelimAt reports whether a string literal starts at src[i].
func mobDelimAt(src []byte, i int, dart bool) (d mobDelim, ok bool) {
	n := len(src)
	j := i
	switch {
	case dart && (src[i] == 'r' || src[i] == 'R') && (i == 0 || !mobIdentByte(src[i-1])) && i+1 < n && (src[i+1] == '\'' || src[i+1] == '"'):
		d.raw = true
		j++
	case !dart && src[i] == '#':
		for j < n && src[j] == '#' {
			j++
		}
		if j >= n || src[j] != '"' {
			return d, false
		}
		d.hashes, d.raw = j-i, true
	}
	if j >= n {
		return d, false
	}
	q := src[j]
	if q != '"' && !(dart && q == '\'') {
		return d, false
	}
	d.q = q
	if j+2 < n && src[j+1] == q && src[j+2] == q {
		d.triple = true
		j += 3
	} else {
		j++
	}
	d.open = j - i
	return d, true
}

// mobStrEnd returns the offset of the closing delimiter of a literal whose content starts at j, and its length.
func mobStrEnd(src []byte, j int, d mobDelim, dart bool, mask func(int)) (int, int) {
	n := len(src)
	closeLen := 1
	if d.triple {
		closeLen = 3
	}
	for j < n {
		c := src[j]
		if !d.triple && c == '\n' {
			return j, 0 // unterminated single-line literal
		}
		if c == d.q && (!d.triple || (j+2 < n && src[j+1] == d.q && src[j+2] == d.q)) {
			k, h := j+closeLen, 0
			for h < d.hashes && k < n && src[k] == '#' {
				h++
				k++
			}
			if h == d.hashes {
				return j, closeLen + h
			}
		}
		if !d.raw && c == '\\' && j+1 < n {
			if !dart && src[j+1] == '(' {
				mask(j)
				j = mobSkipInterp(src, j+1, '(', ')', dart, mask)
				continue
			}
			mask(j)
			if src[j+1] != '\n' {
				mask(j + 1)
			}
			j += 2
			continue
		}
		if dart && !d.raw && c == '$' && j+1 < n && src[j+1] == '{' {
			mask(j)
			j = mobSkipInterp(src, j+1, '{', '}', dart, mask)
			continue
		}
		mask(j)
		j++
	}
	return n, 0
}

func mobSkipInterp(src []byte, j int, open, close byte, dart bool, mask func(int)) int {
	depth := 0
	for ; j < len(src); j++ {
		if d, ok := mobDelimAt(src, j, dart); ok && depth > 0 { // a string nested in the interpolation
			end, cl := mobStrEnd(src, j+d.open, d, dart, mask)
			for k := j; k < min(len(src), end+cl); k++ {
				mask(k)
			}
			j = end + cl - 1
			continue
		}
		c := src[j]
		mask(j)
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return j
}

// mobMask blanks comments (`//`, nested `/* */`) in both views and string contents in the code view.
func mobMask(src []byte, dart bool) (code, full []byte) {
	code = bytes.Clone(src)
	full = bytes.Clone(src)
	blank := func(i int) {
		if src[i] != '\n' {
			code[i], full[i] = ' ', ' '
		}
	}
	maskCode := func(i int) {
		if src[i] != '\n' {
			code[i] = ' '
		}
	}
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				blank(i)
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			depth := 0
			for i < n {
				if src[i] == '/' && i+1 < n && src[i+1] == '*' {
					depth++
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					depth--
					blank(i)
					blank(i + 1)
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				blank(i)
				i++
			}
		default:
			d, ok := mobDelimAt(src, i, dart)
			if !ok {
				i++
				continue
			}
			end, cl := mobStrEnd(src, i+d.open, d, dart, maskCode)
			i = end + cl
			if cl == 0 && i < n && src[i] != '\n' {
				i++
			}
		}
	}
	return code, full
}

// str reads the string literal whose delimiter starts at code[i]: value (raw source text), offset after it.
func (m *mobSrc) str(i int) (string, int, bool) {
	if i >= len(m.code) {
		return "", i, false
	}
	d, ok := mobDelimAt(m.code, i, m.f.lang == "dart")
	if !ok {
		return "", i, false
	}
	start := i + d.open
	end, cl := mobStrEnd(m.code, start, d, m.f.lang == "dart", func(int) {})
	return string(m.full[start:end]), end + cl, true
}

type mobLit struct {
	off int
	val string
}

// literals returns every string literal of the file in order.
func (m *mobSrc) literals() []mobLit {
	var out []mobLit
	dart := m.f.lang == "dart"
	for i := 0; i < len(m.code); {
		if _, ok := mobDelimAt(m.code, i, dart); ok {
			v, next, _ := m.str(i)
			out = append(out, mobLit{off: i, val: v})
			if next <= i {
				next = i + 1
			}
			i = next
			continue
		}
		i++
	}
	return out
}

// mobHost is hostOf restricted to literal hosts (no interpolation).
func mobHost(u string) string {
	h := hostOf(u)
	if h == "" || strings.ContainsAny(h, `\()$'"`) || !strings.Contains(h, ".") {
		return ""
	}
	return h
}

// mobClose returns the index of the bracket closing code[open] ('(' '[' '{'), or -1.
func mobClose(code []byte, open int) int {
	if open < 0 || open >= len(code) {
		return -1
	}
	o := code[open]
	c := map[byte]byte{'(': ')', '[': ']', '{': '}'}[o]
	if c == 0 {
		return -1
	}
	depth := 0
	for i := open; i < len(code); i++ {
		switch code[i] {
		case o:
			depth++
		case c:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// mobArgs splits code(open, close) at top-level commas into [start, end) ranges.
func mobArgs(code []byte, open, close int) [][2]int {
	var out [][2]int
	depth, start := 0, open+1
	for i := open + 1; i < close; i++ {
		switch code[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, [2]int{start, i})
				start = i + 1
			}
		}
	}
	if len(bytes.TrimSpace(code[start:close])) > 0 {
		out = append(out, [2]int{start, close})
	}
	return out
}

var reMobLabel = regexp.MustCompile(`^\s*(\w+)\s*:([^:]|$)`)

// mobLabel returns the label of a `label: value` argument and the value offset (label "" = positional).
func mobLabel(code []byte, a, b int) (string, int) {
	if x := reMobLabel.FindSubmatchIndex(code[a:b]); x != nil {
		return string(code[a+x[2] : a+x[3]]), a + x[4]
	}
	return "", a
}

func mobSkipSpace(code []byte, i int) int {
	for i < len(code) && (code[i] == ' ' || code[i] == '\t' || code[i] == '\n' || code[i] == '\r') {
		i++
	}
	return i
}

var reMobCall = regexp.MustCompile(`(?:^|[^\w.])([A-Z]\w*)\s*(?:<[^<>(){}]*>)?\s*\(`)

// mobFirstCall returns the first `Type(` construction in code[a:b].
func mobFirstCall(code []byte, a, b int) string {
	if x := reMobCall.FindSubmatch(code[a:b]); x != nil {
		return string(x[1])
	}
	return ""
}

// mobTopCalls returns `Type(` constructions at bracket depth 0 of code[a:b] (direct children of a closure).
func mobTopCalls(code []byte, a, b int) []string {
	var out []string
	depth := 0
	for i := a; i < b; i++ {
		c := code[i]
		switch {
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case depth == 0 && c >= 'A' && c <= 'Z' && (i == a || (!mobIdentByte(code[i-1]) && code[i-1] != '.')):
			j := i
			for j < b && mobIdentByte(code[j]) {
				j++
			}
			k := mobSkipSpace(code, j)
			if k < b && code[k] == '(' {
				out = append(out, string(code[i:j]))
			}
			i = j - 1
		}
	}
	return out
}

// mobDepths returns the brace depth of the code at each (ascending) offset.
func mobDepths(code []byte, offs []int) []int {
	out := make([]int, len(offs))
	depth, k := 0, 0
	for i := 0; i < len(code) && k < len(offs); i++ {
		for k < len(offs) && offs[k] == i {
			out[k] = depth
			k++
		}
		switch code[i] {
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return out
}
