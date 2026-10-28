package extract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

// Go route registration methods → HTTP method ("" = taken from the pattern / ANY).
var goRouteMethods = map[string]string{
	// gin / echo
	"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE", "PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
	"Any": "ANY",
	// chi / fiber / gorilla-ish
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
	"All": "ANY", "Connect": "CONNECT", "Trace": "TRACE",
}

var reRegisterServer = regexp.MustCompile(`^Register(\w+)Server$`)

// goFile is a parsed Go file.
type goFile struct {
	f       srcFile
	ast     *ast.File
	fset    *token.FileSet
	imports map[string]string // local name → import path
}

// goFunc locates a function declaration for snippets.
type goFunc struct {
	file *goFile
	decl *ast.FuncDecl
}

// goRepo holds cross-file facts.
type goRepo struct {
	funcs      map[string][]goFunc // by name (methods by method name)
	grpc       map[string]grpcDesc // "OrderService" → desc (from *_grpc.pb.go ServiceDesc)
	funcPrefix map[string]string   // function name → route prefix (chi Mount / a group passed as argument)
}

type grpcDesc struct {
	full    string
	methods []string
}

type goRoute struct {
	item model.Item
	fn   string // enclosing top-level function (for funcPrefix)
}

func extractGo(files []srcFile) []model.Item {
	repo := &goRepo{funcs: map[string][]goFunc{}, grpc: map[string]grpcDesc{}, funcPrefix: map[string]string{}}
	var parsed []*goFile
	for _, f := range files {
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f.rel, f.src, parser.SkipObjectResolution)
		if err != nil || af == nil {
			continue
		}
		gf := &goFile{f: f, ast: af, fset: fset, imports: map[string]string{}}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			name := path.Base(p)
			if strings.HasPrefix(name, "v") && len(name) <= 3 { // github.com/redis/go-redis/v9 → redis
				name = path.Base(path.Dir(p))
			}
			name = strings.TrimPrefix(name, "go-")
			if im.Name != nil {
				name = im.Name.Name
			}
			gf.imports[name] = p
		}
		parsed = append(parsed, gf)
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				repo.funcs[fd.Name.Name] = append(repo.funcs[fd.Name.Name], goFunc{gf, fd})
			}
		}
		collectGRPCDescs(af, repo)
	}
	var routes []goRoute
	var items []model.Item
	for _, gf := range parsed {
		r, it := gf.extract(repo)
		routes = append(routes, r...)
		items = append(items, it...)
	}
	for _, r := range routes {
		if p := repo.funcPrefix[r.fn]; p != "" && !strings.HasPrefix(r.item.Key, "GRPC ") {
			m, pth := route.SplitKey(r.item.Key)
			r.item.Key = m + " " + route.Normalize(route.Join(p, pth))
		}
		items = append(items, r.item)
	}
	return items
}

// collectGRPCDescs finds `var X_ServiceDesc = grpc.ServiceDesc{ServiceName: "...", Methods: ..., Streams: ...}`.
func collectGRPCDescs(af *ast.File, repo *goRepo) {
	ast.Inspect(af, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			return true
		}
		name := vs.Names[0].Name
		if !strings.HasSuffix(name, "_ServiceDesc") {
			return true
		}
		cl, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		var d grpcDesc
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			k, _ := kv.Key.(*ast.Ident)
			if k == nil {
				continue
			}
			switch k.Name {
			case "ServiceName":
				d.full = strLit(kv.Value)
			case "Methods", "Streams":
				ast.Inspect(kv.Value, func(n ast.Node) bool {
					if kv2, ok := n.(*ast.KeyValueExpr); ok {
						if k2, ok := kv2.Key.(*ast.Ident); ok && (k2.Name == "MethodName" || k2.Name == "StreamName") {
							if s := strLit(kv2.Value); s != "" {
								d.methods = append(d.methods, s)
							}
						}
					}
					return true
				})
			}
		}
		if d.full != "" {
			repo.grpc[strings.TrimSuffix(name, "_ServiceDesc")] = d
		}
		return false
	})
}

// strLit evaluates a string literal or a concatenation of literals; "" otherwise.
func strLit(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err == nil {
				return s
			}
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			a, b := strLit(v.X), strLit(v.Y)
			if a != "" && b != "" {
				return a + b
			}
		}
	case *ast.ParenExpr:
		return strLit(v.X)
	}
	return ""
}

func isStrLit(e ast.Expr) bool {
	b, ok := e.(*ast.BasicLit)
	return ok && b.Kind == token.STRING
}

// scope maps a closure parameter (chi Route / Group callbacks) to a prefix inside the closure body.
type scope struct {
	from, to token.Pos
	name     string
	prefix   string
}

func (gf *goFile) hasImport(sub string) bool {
	for _, p := range gf.imports {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func (gf *goFile) line(p token.Pos) int { return gf.fset.Position(p).Line }

func (gf *goFile) text(from, to token.Pos) string {
	a, b := gf.fset.Position(from).Offset, gf.fset.Position(to).Offset
	if a < 0 || b > len(gf.f.src) || a > b {
		return ""
	}
	return string(gf.f.src[a:b])
}

func (gf *goFile) extract(repo *goRepo) ([]goRoute, []model.Item) {
	var routes []goRoute
	var items []model.Item
	groups := map[string]string{} // ident → prefix (r.Group / Route assignments)
	var scopes []scope
	isAsynq := gf.hasImport("hibiken/asynq")
	consumed := map[*ast.CallExpr]bool{}

	var prefixOf func(ast.Expr) string
	prefixOf = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.Ident:
			best := ""
			var bestSpan token.Pos = -1
			for _, s := range scopes {
				if s.name == v.Name && v.Pos() >= s.from && v.Pos() <= s.to && (bestSpan < 0 || s.to-s.from < bestSpan) {
					best, bestSpan = s.prefix, s.to-s.from
				}
			}
			if bestSpan >= 0 {
				return best
			}
			return groups[v.Name]
		case *ast.CallExpr: // r.Group("/x") inline, r.With(mw)
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Group", "Route", "PathPrefix", "Prefix":
					if len(v.Args) > 0 {
						return route.Join(prefixOf(sel.X), strLit(v.Args[0]))
					}
				case "With", "Use", "Subrouter", "Host", "Methods":
					return prefixOf(sel.X)
				}
			}
		case *ast.SelectorExpr: // s.router
			return groups[v.Sel.Name]
		}
		return ""
	}

	// Pass 1: groups and closures (they may be declared before use in any order inside a function).
	ast.Inspect(gf.ast, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if len(v.Lhs) == 1 && len(v.Rhs) == 1 {
				if call, ok := v.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && len(call.Args) > 0 {
						switch sel.Sel.Name {
						case "Group", "PathPrefix", "Prefix":
							if p := strLit(call.Args[0]); p != "" || isStrLit(call.Args[0]) {
								if id := lhsName(v.Lhs[0]); id != "" {
									groups[id] = route.Join(prefixOf(sel.X), p)
								}
							}
						}
					}
					// gorilla: api := r.PathPrefix("/api").Subrouter()
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Subrouter" {
						if id := lhsName(v.Lhs[0]); id != "" {
							groups[id] = prefixOf(sel.X)
						}
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if (sel.Sel.Name == "Route" || sel.Sel.Name == "Group") && len(v.Args) == 2 {
				if fl, ok := v.Args[1].(*ast.FuncLit); ok && len(fl.Type.Params.List) == 1 && len(fl.Type.Params.List[0].Names) == 1 {
					scopes = append(scopes, scope{fl.Pos(), fl.End(), fl.Type.Params.List[0].Names[0].Name, route.Join(prefixOf(sel.X), strLit(v.Args[0]))})
				}
			}
			if sel.Sel.Name == "Group" && len(v.Args) == 1 { // chi r.Group(func(r chi.Router){...}) keeps the prefix
				if fl, ok := v.Args[0].(*ast.FuncLit); ok && len(fl.Type.Params.List) == 1 && len(fl.Type.Params.List[0].Names) == 1 {
					scopes = append(scopes, scope{fl.Pos(), fl.End(), fl.Type.Params.List[0].Names[0].Name, prefixOf(sel.X)})
				}
			}
		}
		return true
	})

	// Pass 2: per top-level function, registrations.
	for _, d := range gf.ast.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fn := fd.Name.Name
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || consumed[call] {
				return true
			}
			var name string
			var recv ast.Expr
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				name, recv = f.Sel.Name, f.X
			case *ast.Ident:
				name = f.Name
			default:
				return true
			}

			// Mount / group passed to a registration function → prefix for that function's routes.
			if name == "Mount" && len(call.Args) == 2 {
				p := route.Join(prefixOf(recv), strLit(call.Args[0]))
				if target := calleeName(call.Args[1]); target != "" && p != "" {
					repo.funcPrefix[target] = p
				}
			}
			for _, a := range call.Args {
				if id, ok := a.(*ast.Ident); ok {
					if p := groups[id.Name]; p != "" {
						if target := calleeNameOnly(call.Fun); target != "" && !groupCalls[target] {
							if _, exists := repo.funcPrefix[target]; !exists {
								repo.funcPrefix[target] = p
							}
						}
					}
				}
			}

			switch {
			case reRegisterServer.MatchString(name) && len(call.Args) == 2:
				svc := reRegisterServer.FindStringSubmatch(name)[1]
				items = append(items, gf.grpcItems(repo, svc, call)...)
				return true

			case (name == "HandleFunc" || name == "Handle") && len(call.Args) >= 2 && recv != nil:
				pat := strLit(call.Args[0])
				if !strings.HasPrefix(pat, "/") && !strings.Contains(pat, " /") && !strings.Contains(pat, "/") {
					if isAsynq && pat != "" { // asynq ServeMux: task type
						items = append(items, gf.consumer(call, pat, pat, "asynq", call.Args[len(call.Args)-1], repo))
					}
					return true
				}
				method, p := "", pat
				if m, rest, ok := strings.Cut(pat, " "); ok && !strings.HasPrefix(pat, "/") {
					method, p = m, strings.TrimSpace(rest)
				}
				if i := strings.IndexByte(p, '/'); i > 0 { // host-qualified pattern "example.com/x"
					p = p[i:]
				}
				routes = append(routes, goRoute{gf.endpoint(call, method, route.Join(prefixOf(recv), p), "net/http", call.Args[len(call.Args)-1], repo), fn})
				return true

			case name == "Methods" && recv != nil: // gorilla r.HandleFunc("/x", h).Methods("GET", "POST")
				inner, ok := recv.(*ast.CallExpr)
				if !ok {
					return true
				}
				isel, ok := inner.Fun.(*ast.SelectorExpr)
				if !ok || (isel.Sel.Name != "HandleFunc" && isel.Sel.Name != "Handle") || len(inner.Args) < 2 {
					return true
				}
				consumed[inner] = true
				for _, a := range call.Args {
					if m := strLit(a); m != "" {
						routes = append(routes, goRoute{gf.endpoint(inner, m, route.Join(prefixOf(isel.X), strLit(inner.Args[0])), "gorilla/mux", inner.Args[len(inner.Args)-1], repo), fn})
					}
				}
				return true

			case (name == "Method" || name == "MethodFunc" || (name == "Handle" && len(call.Args) >= 3)) && recv != nil && len(call.Args) >= 3:
				m, p := strLit(call.Args[0]), strLit(call.Args[1])
				if m != "" && strings.HasPrefix(p, "/") {
					routes = append(routes, goRoute{gf.endpoint(call, m, route.Join(prefixOf(recv), p), "chi", call.Args[len(call.Args)-1], repo), fn})
				}
				return true

			case goRouteMethods[name] != "" && recv != nil && len(call.Args) >= 2:
				p := strLit(call.Args[0])
				pre := prefixOf(recv)
				if !isStrLit(call.Args[0]) || (!strings.HasPrefix(p, "/") && !(p == "" && pre != "") && p != "*") {
					return true
				}
				if gf.isHTTPClientCall(recv) {
					return true
				}
				routes = append(routes, goRoute{gf.endpoint(call, goRouteMethods[name], route.Join(pre, p), gf.goFramework(), call.Args[len(call.Args)-1], repo), fn})
				return true

			case name == "Consume" && len(call.Args) >= 3: // sarama ConsumerGroup.Consume(ctx, []string{"t"}, handler)
				for _, t := range strSlice(call.Args[1]) {
					group := gf.findStringArg("NewConsumerGroup", 1)
					key := t
					if group != "" {
						key = group
					}
					items = append(items, gf.consumer(call, key, t, "sarama", call.Args[2], repo))
				}
				return true

			case name == "ConsumeTopics": // franz-go kgo.ConsumeTopics("a", "b")
				group := gf.findStringArg("ConsumerGroup", 0)
				for _, a := range call.Args {
					if t := strLit(a); t != "" {
						key := t
						if group != "" {
							key = group
						}
						items = append(items, gf.consumer(call, key, t, "franz-go", nil, repo))
					}
				}
				return true

			case (name == "AddFunc" || name == "AddJob") && len(call.Args) == 2 && gf.hasImport("robfig/cron"):
				spec := strLit(call.Args[0])
				if spec == "" {
					return true
				}
				job := calleeName(call.Args[1])
				if job == "" {
					job = path.Base(strings.TrimSuffix(gf.f.rel, ".go")) + "-cron-" + strconv.Itoa(gf.line(call.Pos()))
				}
				it := gf.consumer(call, job, spec, "robfig/cron", call.Args[1], repo)
				it.Kind = model.KindCron
				items = append(items, it)
				return true
			}
			return true
		})
	}

	// segmentio/kafka-go ReaderConfig{Topic, GroupID}.
	ast.Inspect(gf.ast, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ReaderConfig" {
			return true
		}
		var topic, group string
		for _, el := range cl.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok {
					switch k.Name {
					case "Topic":
						topic = strLit(kv.Value)
					case "GroupID":
						group = strLit(kv.Value)
					}
				}
			}
		}
		if topic != "" {
			key := topic
			if group != "" {
				key = group
			}
			items = append(items, model.Item{Kind: model.KindConsumer, Key: key, Source: topic, File: gf.f.rel, Line: gf.line(cl.Pos()),
				Framework: "kafka-go", Language: "go", Snippet: linesRange(gf.f.src, gf.line(cl.Pos()), gf.line(cl.End()))})
		}
		return true
	})

	items = append(items, gf.dependencies()...)
	return routes, items
}

func lhsName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

// calleeName returns the function name a handler/registration expression refers to.
func calleeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.CallExpr:
		// h.wrap(h.create) → create; NewRouter() → NewRouter
		for i := len(v.Args) - 1; i >= 0; i-- {
			if n := calleeName(v.Args[i]); n != "" {
				if _, isCall := v.Args[i].(*ast.CallExpr); !isCall {
					return n
				}
			}
		}
		return calleeName(v.Fun)
	case *ast.UnaryExpr:
		return calleeName(v.X)
	}
	return ""
}

func strSlice(e ast.Expr) []string {
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		if s := strLit(e); s != "" {
			return []string{s}
		}
		return nil
	}
	var out []string
	for _, el := range cl.Elts {
		if s := strLit(el); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// findStringArg returns the string literal argument #idx of the first call named fn in the file.
func (gf *goFile) findStringArg(fn string, idx int) string {
	var out string
	ast.Inspect(gf.ast, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && calleeNameOnly(call.Fun) == fn && len(call.Args) > idx {
			out = strLit(call.Args[idx])
		}
		return true
	})
	return out
}

func calleeNameOnly(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

// isHTTPClientCall reports receivers that are HTTP clients, not routers (http.Get, client.Get).
func (gf *goFile) isHTTPClientCall(recv ast.Expr) bool {
	id, ok := recv.(*ast.Ident)
	if !ok {
		return false
	}
	if p, ok := gf.imports[id.Name]; ok && (p == "net/http" || strings.Contains(p, "resty")) {
		return true
	}
	switch strings.ToLower(id.Name) {
	case "client", "httpclient", "c", "cli", "resty", "req", "rdb", "redis", "cache":
		return !gf.hasImport("labstack/echo") && !gf.hasImport("gin-gonic") && !gf.hasImport("go-chi") && !gf.hasImport("gofiber")
	}
	return false
}

func (gf *goFile) goFramework() string {
	switch {
	case gf.hasImport("gin-gonic/gin"):
		return "gin"
	case gf.hasImport("labstack/echo"):
		return "echo"
	case gf.hasImport("gofiber/fiber"):
		return "fiber"
	case gf.hasImport("go-chi/chi"):
		return "chi"
	case gf.hasImport("gorilla/mux"):
		return "gorilla/mux"
	}
	return "go"
}

// handlerSource returns the handler name, its source and callee signatures.
func (gf *goFile) handlerSource(h ast.Expr, repo *goRepo) (name, snippet string, callees []string) {
	if h == nil {
		return "", "", nil
	}
	if fl, ok := h.(*ast.FuncLit); ok {
		return "", gf.text(fl.Pos(), fl.End()), calleeSigs(fl.Body, repo)
	}
	name = calleeName(h)
	if name == "" {
		return "", "", nil
	}
	cands := repo.funcs[name]
	if len(cands) == 0 {
		return name, "", nil
	}
	best := cands[0]
	for _, c := range cands { // prefer the same directory (package)
		if path.Dir(c.file.f.rel) == path.Dir(gf.f.rel) {
			best = c
			break
		}
	}
	return name, best.file.text(best.decl.Pos(), best.decl.End()), calleeSigs(best.decl.Body, repo)
}

// calleeSigs returns signatures ("func (s *Store) CreateOrder(ctx context.Context, o Order) error") of functions
// called in body that are declared in the repository (max 20).
func calleeSigs(body ast.Node, repo *goRepo) []string {
	if body == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(out) >= 20 {
			return true
		}
		name := calleeNameOnly(call.Fun)
		if name == "" || seen[name] {
			return true
		}
		seen[name] = true
		if c := repo.funcs[name]; len(c) > 0 {
			d := c[0]
			sig := d.file.text(d.decl.Pos(), d.decl.Type.End())
			out = append(out, strings.Join(strings.Fields(sig), " "))
		}
		return true
	})
	sort.Strings(out)
	return out
}

func (gf *goFile) endpoint(call *ast.CallExpr, method, pattern, framework string, handler ast.Expr, repo *goRepo) model.Item {
	name, snip, callees := gf.handlerSource(handler, repo)
	if snip == "" {
		snip = gf.text(call.Pos(), call.End())
	}
	return model.Item{Kind: model.KindEndpoint, Key: route.Key(method, pattern), File: gf.f.rel, Line: gf.line(call.Pos()),
		Framework: framework, Handler: name, Language: "go", Snippet: snip, Callees: callees}
}

func (gf *goFile) consumer(call *ast.CallExpr, key, source, framework string, handler ast.Expr, repo *goRepo) model.Item {
	name, snip, callees := gf.handlerSource(handler, repo)
	if snip == "" {
		snip = gf.text(call.Pos(), call.End())
	}
	return model.Item{Kind: model.KindConsumer, Key: key, Source: source, File: gf.f.rel, Line: gf.line(call.Pos()),
		Framework: framework, Handler: name, Language: "go", Snippet: snip, Callees: callees}
}

func (gf *goFile) grpcItems(repo *goRepo, svc string, call *ast.CallExpr) []model.Item {
	impl := ""
	if len(call.Args) == 2 {
		impl = gf.text(call.Args[1].Pos(), call.Args[1].End())
	}
	d, ok := repo.grpc[svc]
	if !ok {
		return []model.Item{{Kind: model.KindEndpoint, Key: "GRPC /" + svc, File: gf.f.rel, Line: gf.line(call.Pos()),
			Framework: "grpc", Handler: impl, Language: "go", Snippet: gf.text(call.Pos(), call.End())}}
	}
	var out []model.Item
	for _, m := range d.methods {
		it := model.Item{Kind: model.KindEndpoint, Key: "GRPC /" + d.full + "/" + m, File: gf.f.rel, Line: gf.line(call.Pos()),
			Framework: "grpc", Handler: m, Language: "go"}
		if c := repo.funcs[m]; len(c) > 0 {
			for _, fn := range c {
				if fn.decl.Recv != nil && !strings.HasSuffix(fn.file.f.rel, ".pb.go") {
					it.Snippet = fn.file.text(fn.decl.Pos(), fn.decl.End())
					it.Callees = calleeSigs(fn.decl.Body, repo)
					break
				}
			}
		}
		if it.Snippet == "" {
			it.Snippet = gf.text(call.Pos(), call.End())
		}
		out = append(out, it)
	}
	return out
}

// Go import path fragments → dependency (kind, name).
var goDeps = []struct{ frag, kind, name string }{
	{"github.com/lib/pq", "db", "postgres"},
	{"github.com/jackc/pgx", "db", "postgres"},
	{"github.com/go-sql-driver/mysql", "db", "mysql"},
	{"go.mongodb.org/mongo-driver", "db", "mongodb"},
	{"redis/go-redis", "cache", "redis"},
	{"go-redis/redis", "cache", "redis"},
	{"gomodule/redigo", "cache", "redis"},
	{"IBM/sarama", "queue", "kafka"},
	{"Shopify/sarama", "queue", "kafka"},
	{"twmb/franz-go", "queue", "kafka"},
	{"segmentio/kafka-go", "queue", "kafka"},
	{"rabbitmq/amqp091-go", "queue", "rabbitmq"},
	{"streadway/amqp", "queue", "rabbitmq"},
	{"service/sqs", "queue", "sqs"},
	{"cloud.google.com/go/pubsub", "queue", "pubsub"},
	{"stripe/stripe-go", "http", "api.stripe.com"},
	{"hibiken/asynq", "queue", "redis"},
}

func (gf *goFile) dependencies() []model.Item {
	var out []model.Item
	for _, im := range gf.ast.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		for _, d := range goDeps {
			if strings.Contains(p, d.frag) {
				out = append(out, depItem(d.kind, d.name, gf.f.rel, gf.line(im.Pos()), "go"))
				break
			}
		}
	}
	ast.Inspect(gf.ast, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		idx := -1
		switch sel.Sel.Name {
		case "Get", "Post", "Head", "PostForm":
			if id, ok := sel.X.(*ast.Ident); ok && gf.imports[id.Name] == "net/http" {
				idx = 0
			}
		case "NewRequest":
			idx = 1
		case "NewRequestWithContext":
			idx = 2
		}
		if idx >= 0 && len(call.Args) > idx {
			if h := hostOf(strLit(call.Args[idx])); h != "" {
				out = append(out, depItem("http", h, gf.f.rel, gf.line(call.Pos()), "go"))
			}
		}
		return true
	})
	return out
}

// groupCalls are router calls that take a group as argument without registering routes for it.
var groupCalls = map[string]bool{"Group": true, "Route": true, "Use": true, "Mount": true, "ListenAndServe": true, "Handler": true,
	"Serve": true, "NewServer": true, "Run": true, "Start": true}
