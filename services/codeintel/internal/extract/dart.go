package extract

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

// Dart (Flutter) extractor. Sources are masked by cleanDart and scanned like Swift (see swift.go).
//
// Views, keyed like the Flutter SDK's TrooperNavigatorObserver reports them where possible (RouteSettings.name):
//   - go_router `GoRoute(path:, name:, builder:)` and `@TypedGoRoute` — nested routes join their parent's path
//     (absolute child paths stay as-is; ShellRoute / StatefulShellBranch are transparent). Key = full normalized path,
//     meta.rumName = `name ?? relative path` (what go_router puts in RouteSettings.name).
//   - `MaterialApp(routes: {'/cart': (c) => CartPage()})` and `onGenerateRoute` `case '/x':` → Key = route name
//     (framework flutter-routes). `static const routeName = '/x'` constants used as keys are resolved.
//   - auto_route `AutoRoute(page: XRoute.page, path: '/x', children: […])` → Key = path (route name without a path);
//     meta.rumName = XRoute. `@RoutePage()` classes no AutoRoute references → Key = class name.
//   - `class XScreen/XPage extends …Widget` → Key = class name (framework flutter), unless a route item anywhere in
//     the repo builds that widget (then the route is the item).
//   - `Navigator.pushNamed(context, '/x')`, `context.go('/x')`, … only count as meta.navigations on matching items.
//
// Dependencies: absolute URL literals in files using package:http / dio (or in Uri.parse / baseUrl), `Uri.https(host)`,
// and cloud_firestore / sqflite / drift / hive / isar / realm imports.

var (
	reDartImport   = regexp.MustCompile(`(?m)^[ \t]*(?:import|export)\s+`)
	reDartClass    = regexp.MustCompile(`(?m)^[ \t]*(?:(?:abstract|final|base|sealed|interface|mixin)\s+)*class\s+(\w+)(?:\s*<[^{]*?>)?(?:\s+extends\s+(\w+))?[^{;]*\{`)
	reDartGoRoute  = regexp.MustCompile(`\b(GoRoute|TypedGoRoute\s*<\s*(\w+)\s*>)\s*\(`)
	reDartAuto     = regexp.MustCompile(`\b(AutoRoute|CustomRoute|AdaptiveRoute|MaterialRoute|CupertinoRoute)\s*(?:<[^>]*>)?\s*\(`)
	reDartRoutePg  = regexp.MustCompile(`@RoutePage\s*(?:<[^>]*>)?\s*\(`)
	reDartRoutes   = regexp.MustCompile(`\broutes\s*:\s*(?:const\s+)?(?:<[^>]*>\s*)?\{`)
	reDartCase     = regexp.MustCompile(`\bcase\s+`)
	reDartCaseEnd  = regexp.MustCompile(`\b(?:case|default)\b`)
	reDartConst    = regexp.MustCompile(`\b(?:static\s+)?(?:const|final)\s+(?:String\s+)?(\w+)\s*=\s*`)
	reDartNav      = regexp.MustCompile(`\b(?:pushNamed|pushReplacementNamed|popAndPushNamed|pushNamedAndRemoveUntil|restorablePushNamed|go|push|goNamed|pushReplacement|replace|replaceNamed)\s*(?:<[^>]*>)?\s*\(\s*(?:context\s*,\s*)?`)
	reDartUriParse = regexp.MustCompile(`(?:\bUri\s*\.\s*parse\s*\(|\bbaseUrl\s*[:=])\s*$`)
	reDartUriHTTPS = regexp.MustCompile(`\bUri\s*\.\s*https?\s*\(\s*`)
	reDartScreen   = regexp.MustCompile(`(?:Screen|Page)$`)
)

var dartDepPkgs = map[string][2]string{
	"cloud_firestore": {"db", "firestore"}, "firebase_database": {"db", "firebase-database"},
	"sqflite": {"db", "sqlite"}, "sqflite_common_ffi": {"db", "sqlite"}, "drift": {"db", "sqlite"}, "sqlite3": {"db", "sqlite"},
	"hive": {"db", "hive"}, "hive_flutter": {"db", "hive"}, "hive_ce": {"db", "hive"}, "hive_ce_flutter": {"db", "hive"},
	"isar": {"db", "isar"}, "realm": {"db", "realm"}, "objectbox": {"db", "objectbox"},
	"postgres": {"db", "postgres"}, "mongo_dart": {"db", "mongodb"}, "redis": {"cache", "redis"},
}

// dartRouteWrappers are framework widgets/pages that wrap the screen in a builder.
var dartRouteWrappers = map[string]bool{"MaterialPage": true, "CupertinoPage": true, "NoTransitionPage": true,
	"CustomTransitionPage": true, "MaterialPageRoute": true, "CupertinoPageRoute": true, "PageRouteBuilder": true,
	"RouteSettings": true, "ValueKey": true, "Key": true, "BlocProvider": true, "MultiBlocProvider": true,
	"ProviderScope": true, "ChangeNotifierProvider": true, "MultiProvider": true, "Provider": true, "Builder": true,
	"Scaffold": true, "Text": true, "FadeTransition": true, "SlideTransition": true}

type dartClass struct {
	name, super string
	file        *mobSrc
	off, open   int
	close       int
	routePage   bool   // @RoutePage()
	routeName   string // auto_route generated route class
}

// dartRoute is one route-call span (GoRoute / AutoRoute) for parent resolution.
type dartRoute struct {
	open, close int
	path        string // full path ("" = transparent / unknown)
}

func extractDart(files []srcFile) []model.Item {
	var srcs []*mobSrc
	classes := map[string]*dartClass{}
	var classList []*dartClass
	for _, f := range files {
		m := cleanDart(f)
		srcs = append(srcs, m)
		for _, c := range m.dartClasses() {
			if _, ok := classes[c.name]; !ok {
				classes[c.name] = c
			}
			classList = append(classList, c)
		}
	}
	byRouteName := map[string]*dartClass{}
	for _, c := range classList {
		if c.routePage {
			byRouteName[c.routeName] = c
		}
	}
	consts := map[string]string{} // "Class.field" and "field" → string value
	for _, m := range srcs {
		m.dartConsts(classList, consts)
	}
	navs := map[string]int{}
	var items []model.Item
	built := map[string]bool{}           // widget classes a route builds
	referencedRoute := map[string]bool{} // auto_route route classes referenced by an AutoRoute
	for _, m := range srcs {
		its := m.goRoutes(classes)
		its = append(its, m.routesMap(classes, consts)...)
		its = append(its, m.generateRoutes(classes, consts)...)
		its = append(its, m.autoRoutes(byRouteName, referencedRoute)...)
		for _, it := range its {
			if it.Handler != "" {
				built[it.Handler] = true
			}
		}
		items = append(items, its...)
		m.dartNavigations(navs)
	}
	for _, c := range classList {
		if built[c.name] {
			continue
		}
		fw := ""
		switch {
		case c.routePage && !referencedRoute[c.routeName]:
			fw = "auto_route"
		case !c.routePage && reDartScreen.MatchString(c.name) && dartWidgetSuper(c.super):
			fw = "flutter"
		}
		if fw == "" {
			continue
		}
		line := lineAt(c.file.code, c.off)
		it := model.Item{Kind: model.KindView, Key: c.name, File: c.file.f.rel, Line: line, Framework: fw, Handler: c.name,
			Language: "dart", Snippet: braceBlock(c.file.f.src, line)}
		if c.routePage {
			it.Meta = map[string]any{"rumName": c.routeName}
		}
		items = append(items, it)
	}
	for i := range items {
		n := navs[items[i].Key]
		if r, _ := items[i].Meta["rumName"].(string); r != "" && r != items[i].Key {
			n += navs[r]
		}
		if n > 0 {
			if items[i].Meta == nil {
				items[i].Meta = map[string]any{}
			}
			items[i].Meta["navigations"] = n
		}
	}
	for _, m := range srcs {
		items = append(items, m.dartDeps()...)
	}
	return items
}

func dartWidgetSuper(s string) bool {
	return strings.HasSuffix(s, "Widget") || s == "GetView" || s == "GetWidget"
}

// cleanDart masks a Dart source: `//`, `///` and nested `/* */` comments are blanked; contents of '…', "…", ”'…”',
// """…""" and raw r'…' literals (and ${…} interpolations) are blanked in the code view only. Offsets are preserved.
func cleanDart(f srcFile) *mobSrc {
	f.lang = "dart"
	code, full := mobMask(f.src, true)
	return &mobSrc{f: f, code: code, full: full}
}

func (m *mobSrc) dartClasses() []*dartClass {
	var out []*dartClass
	pages := reDartRoutePg.FindAllIndex(m.code, -1)
	for _, x := range reDartClass.FindAllSubmatchIndex(m.code, -1) {
		c := &dartClass{name: string(m.code[x[2]:x[3]]), file: m, off: x[0], open: x[1] - 1}
		if x[4] >= 0 {
			c.super = string(m.code[x[4]:x[5]])
		}
		c.close = mobClose(m.code, c.open)
		for _, p := range pages { // @RoutePage() directly before the class (only whitespace / annotations between)
			if p[1] <= x[0] && x[0]-p[1] < 200 && !bytes.Contains(m.code[p[1]:x[0]], []byte("class ")) {
				c.routePage = true
				pc := mobClose(m.code, p[1]-1)
				name := ""
				if pc > 0 {
					for _, a := range mobArgs(m.code, p[1]-1, pc) {
						if l, vs := mobLabel(m.code, a[0], a[1]); l == "name" {
							name, _, _ = m.str(mobSkipSpace(m.code, vs))
						}
					}
				}
				if name == "" {
					name = strings.TrimSuffix(strings.TrimSuffix(c.name, "Page"), "Screen") + "Route"
				}
				c.routeName = name
			}
		}
		out = append(out, c)
	}
	return out
}

// dartConsts records `static const routeName = '/x'` (as Class.field and field) and top-level string consts.
func (m *mobSrc) dartConsts(classes []*dartClass, consts map[string]string) {
	for _, x := range reDartConst.FindAllSubmatchIndex(m.code, -1) {
		v, _, ok := m.str(x[1])
		if !ok {
			continue
		}
		field := string(m.code[x[2]:x[3]])
		for _, c := range classes {
			if c.file == m && c.open < x[0] && x[0] < c.close {
				consts[c.name+"."+field] = v
			}
		}
		if _, dup := consts[field]; !dup {
			consts[field] = v
		}
	}
}

// key resolves a route-name expression: a string literal or a known const (`HomePage.routeName`, `kHome`).
func (m *mobSrc) dartKey(a, b int, consts map[string]string) (string, bool) {
	i := mobSkipSpace(m.code, a)
	if v, _, ok := m.str(i); ok {
		return v, true
	}
	ref := strings.TrimSpace(string(m.code[i:b]))
	if v, ok := consts[ref]; ok {
		return v, true
	}
	return "", false
}

// widgetIn returns the screen widget built in code[a:b]: the first in-repo widget class, else the first
// construction that is not a known wrapper.
func (m *mobSrc) widgetIn(a, b int, classes map[string]*dartClass) string {
	first := ""
	for _, x := range reMobCall.FindAllSubmatch(m.code[a:b], -1) {
		n := string(x[1])
		if c, ok := classes[n]; ok && (dartWidgetSuper(c.super) || c.routePage || reDartScreen.MatchString(n)) {
			return n
		}
		if first == "" && !dartRouteWrappers[n] {
			first = n
		}
	}
	return first
}

// routeView builds a view item; the snippet is the widget class body when it is in the repo.
func (m *mobSrc) routeView(key, fw, widget string, off, close int, classes map[string]*dartClass) model.Item {
	line := lineAt(m.code, off)
	it := model.Item{Kind: model.KindView, Key: key, File: m.f.rel, Line: line, Framework: fw, Handler: widget, Language: "dart"}
	if c, ok := classes[widget]; ok && widget != "" {
		it.Snippet = braceBlock(c.file.f.src, lineAt(c.file.code, c.off))
	} else {
		end := lineAt(m.code, max(off, close))
		it.Snippet = linesRange(m.f.src, line, min(end, line+40))
	}
	return it
}

// parentPath returns the full path of the innermost route span enclosing off.
func parentPath(spans []dartRoute, off int) string {
	best := -1
	for i, s := range spans {
		if s.open < off && off < s.close && s.path != "" && (best < 0 || s.open > spans[best].open) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return spans[best].path
}

func joinChild(parent, p string) string {
	if strings.HasPrefix(p, "/") || parent == "" {
		return p
	}
	return route.Join(parent, p)
}

// goRoutes extracts go_router GoRoute / TypedGoRoute views.
func (m *mobSrc) goRoutes(classes map[string]*dartClass) []model.Item {
	if !bytes.Contains(m.code, []byte("GoRoute")) {
		return nil
	}
	var spans []dartRoute
	var items []model.Item
	for _, x := range reDartGoRoute.FindAllSubmatchIndex(m.code, -1) { // in source order: parents come first
		open := x[1] - 1
		c := mobClose(m.code, open)
		if c < 0 {
			continue
		}
		path, name, widget := "", "", ""
		hasPath := false
		for _, a := range mobArgs(m.code, open, c) {
			label, vs := mobLabel(m.code, a[0], a[1])
			switch label {
			case "path":
				path, _, hasPath = m.str(mobSkipSpace(m.code, vs))
			case "name":
				name, _, _ = m.str(mobSkipSpace(m.code, vs))
			case "builder", "pageBuilder":
				widget = m.widgetIn(vs, a[1], classes)
			}
		}
		if x[4] >= 0 { // TypedGoRoute<HomeRoute>: the route data class
			widget = string(m.code[x[4]:x[5]])
		}
		if !hasPath {
			continue
		}
		full := joinChild(parentPath(spans, open), path)
		spans = append(spans, dartRoute{open: open, close: c, path: full})
		it := m.routeView(route.Normalize(full), "go_router", widget, x[0], c, classes)
		rum := name
		if rum == "" {
			rum = path
		}
		it.Meta = map[string]any{"rumName": rum}
		items = append(items, it)
	}
	return items
}

// routesMap extracts `routes: {'/x': (context) => XPage()}` maps (MaterialApp / CupertinoApp / WidgetsApp).
func (m *mobSrc) routesMap(classes map[string]*dartClass, consts map[string]string) []model.Item {
	var items []model.Item
	for _, x := range reDartRoutes.FindAllIndex(m.code, -1) {
		open := x[1] - 1
		c := mobClose(m.code, open)
		if c < 0 {
			continue
		}
		for _, e := range mobArgs(m.code, open, c) {
			colon := -1
			depth := 0
			for i := e[0]; i < e[1]; i++ {
				switch m.code[i] {
				case '(', '[', '{':
					depth++
				case ')', ']', '}':
					depth--
				case ':':
					if depth == 0 && colon < 0 {
						colon = i
					}
				}
			}
			if colon < 0 {
				continue
			}
			key, ok := m.dartKey(e[0], colon, consts)
			if !ok || key == "" {
				continue
			}
			if strings.HasPrefix(key, "/") {
				key = route.Normalize(key)
			}
			w := m.widgetIn(colon+1, e[1], classes)
			items = append(items, m.routeView(key, "flutter-routes", w, mobSkipSpace(m.code, e[0]), e[1], classes))
		}
	}
	return items
}

// generateRoutes extracts `case '/x':` branches of onGenerateRoute functions.
func (m *mobSrc) generateRoutes(classes map[string]*dartClass, consts map[string]string) []model.Item {
	if !bytes.Contains(m.code, []byte("onGenerateRoute")) && !bytes.Contains(m.code, []byte("RouteSettings")) {
		return nil
	}
	var items []model.Item
	for _, x := range reDartCase.FindAllIndex(m.code, -1) {
		colon := bytes.IndexByte(m.code[x[1]:min(len(m.code), x[1]+200)], ':')
		if colon < 0 {
			continue
		}
		colon += x[1]
		key, ok := m.dartKey(x[1], colon, consts)
		if !ok || !strings.HasPrefix(key, "/") {
			continue
		}
		end := min(len(m.code), colon+800)
		if y := reDartCaseEnd.FindIndex(m.code[colon:end]); y != nil {
			end = colon + y[0]
		}
		w := m.widgetIn(colon+1, end, classes)
		items = append(items, m.routeView(route.Normalize(key), "flutter-routes", w, x[0], end, classes))
	}
	return items
}

// autoRoutes extracts auto_route AutoRoute(page: XRoute.page, path: …, children: […]) views.
func (m *mobSrc) autoRoutes(byRouteName map[string]*dartClass, referenced map[string]bool) []model.Item {
	if !bytes.Contains(m.code, []byte("Route")) || !bytes.Contains(m.code, []byte(".page")) {
		return nil
	}
	var spans []dartRoute
	var items []model.Item
	for _, x := range reDartAuto.FindAllSubmatchIndex(m.code, -1) {
		open := x[1] - 1
		c := mobClose(m.code, open)
		if c < 0 {
			continue
		}
		path, page := "", ""
		hasPath := false
		for _, a := range mobArgs(m.code, open, c) {
			label, vs := mobLabel(m.code, a[0], a[1])
			switch label {
			case "path":
				path, _, hasPath = m.str(mobSkipSpace(m.code, vs))
			case "page":
				page = strings.TrimSuffix(strings.TrimSpace(string(m.code[vs:a[1]])), ".page")
			}
		}
		if page == "" {
			continue
		}
		referenced[page] = true
		key := page
		if hasPath {
			full := joinChild(parentPath(spans, open), path)
			spans = append(spans, dartRoute{open: open, close: c, path: full})
			key = route.Normalize(full)
		}
		widget := ""
		classes := map[string]*dartClass{}
		if pc := byRouteName[page]; pc != nil {
			widget = pc.name
			classes[pc.name] = pc
		}
		it := m.routeView(key, "auto_route", widget, x[0], c, classes)
		it.Meta = map[string]any{"rumName": page}
		items = append(items, it)
	}
	return items
}

// dartNavigations counts route names pushed with Navigator / go_router calls.
func (m *mobSrc) dartNavigations(navs map[string]int) {
	for _, x := range reDartNav.FindAllIndex(m.code, -1) {
		if v, _, ok := m.str(x[1]); ok && v != "" {
			navs[v]++
			if strings.HasPrefix(v, "/") {
				if n := route.Normalize(v); n != v {
					navs[n]++
				}
			}
		}
	}
}

// dartDeps returns package imports of local stores and absolute URL literals.
func (m *mobSrc) dartDeps() []model.Item {
	var out []model.Item
	network := false
	for _, x := range reDartImport.FindAllIndex(m.code, -1) {
		v, _, ok := m.str(x[1])
		if !ok {
			continue
		}
		pkg, ok := strings.CutPrefix(v, "package:")
		if !ok {
			continue
		}
		if i := strings.IndexByte(pkg, '/'); i >= 0 {
			pkg = pkg[:i]
		}
		switch pkg {
		case "http", "dio", "chopper", "retrofit", "graphql_flutter", "graphql":
			network = true
		}
		if d, ok := dartDepPkgs[pkg]; ok {
			out = append(out, depItem(d[0], d[1], m.f.rel, lineAt(m.code, x[0]), "dart"))
		}
	}
	if bytes.Contains(m.code, []byte("HttpClient(")) {
		network = true
	}
	for _, l := range m.literals() {
		h := mobHost(l.val)
		if h == "" {
			continue
		}
		if network || reDartUriParse.Match(m.code[max(0, l.off-40):l.off]) {
			out = append(out, depItem("http", h, m.f.rel, lineAt(m.code, l.off), "dart"))
		}
	}
	for _, x := range reDartUriHTTPS.FindAllIndex(m.code, -1) { // Uri.https('api.x.com', '/v1/…')
		if v, _, ok := m.str(x[1]); ok {
			if h := mobHost("https://" + v); h != "" {
				out = append(out, depItem("http", h, m.f.rel, lineAt(m.code, x[0]), "dart"))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}
