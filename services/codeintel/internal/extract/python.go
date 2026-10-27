package extract

import (
	"path"
	"regexp"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

var (
	rePyDecorator   = regexp.MustCompile(`^\s*@([A-Za-z_][\w]*)\.(get|post|put|delete|patch|head|options|api_route|route|websocket)\s*\(\s*(?:path\s*=\s*)?[rbuRBU]?(['"])(.*?)['"](.*)$`)
	rePyMethods     = regexp.MustCompile(`methods\s*=\s*[\[(]([^\])]*)[\])]`)
	rePyDef         = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	rePyClass       = regexp.MustCompile(`^(\s*)class\s+([A-Za-z_]\w*)`)
	rePyRouterVar   = regexp.MustCompile(`^\s*([A-Za-z_]\w*)\s*=\s*(?:fastapi\.)?(APIRouter|Blueprint|Flask|FastAPI)\s*\((.*)`)
	rePyPrefixKw    = regexp.MustCompile(`(?:url_)?prefix\s*=\s*['"]([^'"]*)['"]`)
	rePyInclude     = regexp.MustCompile(`\.(include_router|register_blueprint)\s*\(\s*([A-Za-z_][\w.]*)(.*)`)
	rePyDjangoPath  = regexp.MustCompile(`^\s*(path|re_path|url)\s*\(\s*[rbuRBU]?(['"])(.*?)['"]\s*,\s*(.*)$`)
	rePyDjangoInc   = regexp.MustCompile(`include\s*\(\s*\(?\s*['"]([\w.]+)['"]`)
	rePyCeleryTask  = regexp.MustCompile(`^\s*@(?:([A-Za-z_]\w*)\.)?(task|shared_task)\b(.*)$`)
	rePyNameKw      = regexp.MustCompile(`\bname\s*=\s*['"]([^'"]+)['"]`)
	rePyQueueKw     = regexp.MustCompile(`\bqueue\s*=\s*['"]([^'"]+)['"]`)
	rePyKafkaCons   = regexp.MustCompile(`KafkaConsumer\s*\(\s*['"]([^'"]+)['"](.*)`)
	rePyGroupID     = regexp.MustCompile(`group_id\s*=\s*['"]([^'"]+)['"]`)
	rePyConfGroup   = regexp.MustCompile(`['"]group\.id['"]\s*:\s*['"]([^'"]+)['"]`)
	rePySubscribe   = regexp.MustCompile(`\.subscribe\s*\(\s*\[([^\]]*)\]`)
	rePyImport      = regexp.MustCompile(`^\s*(?:from\s+([\w.]+)\s+import|import\s+([\w.]+))`)
	rePyURL         = regexp.MustCompile(`\b(?:requests|httpx|session|client)\.(?:get|post|put|patch|delete|request)\s*\(\s*(?:['"][A-Z]+['"]\s*,\s*)?[fr]?['"](https?://[^'"]+)['"]`)
	rePyBeatEntry   = regexp.MustCompile(`['"]([^'"]+)['"]\s*:\s*\{`)
	rePyBeatTask    = regexp.MustCompile(`['"]task['"]\s*:\s*['"]([\w.]+)['"]`)
	rePyBeatSched   = regexp.MustCompile(`['"]schedule['"]\s*:\s*((?:crontab|timedelta|schedule|solar)\s*\([^)]*\)|[\d.]+(?:\s*\*\s*[\d.]+)*)`)
	rePyStrList     = regexp.MustCompile(`['"]([^'"]+)['"]`)
	rePyCrontabArgs = regexp.MustCompile(`(minute|hour|day_of_week|day_of_month|month_of_year)\s*=\s*['"]?([^,'")]+)['"]?`)
)

var pyDeps = map[string][2]string{
	"psycopg2": {"db", "postgres"}, "psycopg": {"db", "postgres"}, "asyncpg": {"db", "postgres"},
	"pymysql": {"db", "mysql"}, "MySQLdb": {"db", "mysql"}, "aiomysql": {"db", "mysql"},
	"pymongo": {"db", "mongodb"}, "motor": {"db", "mongodb"},
	"redis": {"cache", "redis"}, "aioredis": {"cache", "redis"},
	"kafka": {"queue", "kafka"}, "confluent_kafka": {"queue", "kafka"}, "aiokafka": {"queue", "kafka"},
	"pika": {"queue", "rabbitmq"}, "aio_pika": {"queue", "rabbitmq"},
	"stripe": {"http", "api.stripe.com"}, "iyzipay": {"http", "api.iyzipay.com"},
	"google.cloud.pubsub": {"queue", "pubsub"}, "google.cloud.pubsub_v1": {"queue", "pubsub"},
}

type pyFile struct {
	f     srcFile
	lines []string // comments and docstrings blanked; line numbers preserved
}

type pyRoute struct {
	item   model.Item
	router string // decorator object name (app, router, bp)
}

// pyInclude is include_router / register_blueprint / Django include().
type pyInclude struct {
	module string // python module ("orders.routes") or "" for the same file
	name   string // router variable
	prefix string
}

func extractPython(files []srcFile) []model.Item {
	var parsed []*pyFile
	for _, f := range files {
		parsed = append(parsed, &pyFile{f: f, lines: cleanPython(f.src)})
	}
	var items []model.Item
	routesByFile := map[string][]pyRoute{}
	routerPrefix := map[string]map[string]string{} // file → router var → own prefix (APIRouter(prefix=), Blueprint(url_prefix=))
	var includes []struct {
		from string
		inc  pyInclude
	}
	djangoInc := map[string]string{} // included module → prefix
	djangoRoutes := map[string][]model.Item{}
	for _, pf := range parsed {
		r, it, own, inc, dj, djInc := pf.extract()
		routesByFile[pf.f.rel] = r
		routerPrefix[pf.f.rel] = own
		items = append(items, it...)
		for _, x := range inc {
			includes = append(includes, struct {
				from string
				inc  pyInclude
			}{pf.f.rel, x})
		}
		if len(dj) > 0 {
			djangoRoutes[pf.f.rel] = dj
		}
		for m, p := range djInc {
			djangoInc[m] = route.Join(djangoInc[m], p)
		}
	}
	// FastAPI / Flask: own prefix + include prefix (include in any file naming the module of the router file).
	for file, routes := range routesByFile {
		modName := strings.TrimSuffix(path.Base(file), ".py")
		for _, r := range routes {
			prefix := routerPrefix[file][r.router]
			for _, in := range includes {
				if in.inc.name != r.router {
					continue
				}
				if (in.inc.module == "" && in.from == file) || in.inc.module == modName || strings.HasSuffix(in.inc.module, "."+modName) ||
					moduleMatches(in.inc.module, file) {
					prefix = route.Join(in.inc.prefix, prefix)
					break
				}
			}
			m, p := route.SplitKey(r.item.Key)
			r.item.Key = m + " " + route.Normalize(route.Join(prefix, p))
			items = append(items, r.item)
		}
	}
	// Django: resolve include() prefixes recursively (module path → file path).
	for file, routes := range djangoRoutes {
		prefix := djangoPrefix(file, djangoInc, 0)
		for _, it := range routes {
			m, p := route.SplitKey(it.Key)
			it.Key = m + " " + route.Normalize(route.Join(prefix, p))
			items = append(items, it)
		}
	}
	return items
}

func moduleMatches(module, file string) bool {
	if module == "" {
		return false
	}
	return strings.HasSuffix(strings.TrimSuffix(file, ".py"), strings.ReplaceAll(module, ".", "/"))
}

// djangoPrefix returns the include prefix of a urls file ("orders/urls.py" included as 'orders.urls').
func djangoPrefix(file string, inc map[string]string, depth int) string {
	if depth > 5 {
		return ""
	}
	for module, p := range inc {
		if moduleMatches(module, file) {
			return p
		}
	}
	return ""
}

// cleanPython blanks comments and triple-quoted strings, keeping line count.
func cleanPython(src []byte) []string {
	lines := strings.Split(string(src), "\n")
	inTriple := ""
	for i, l := range lines {
		var b strings.Builder
		for j := 0; j < len(l); j++ {
			if inTriple != "" {
				if strings.HasPrefix(l[j:], inTriple) {
					inTriple = ""
					j += 2
				}
				continue
			}
			c := l[j]
			if c == '#' {
				break
			}
			if (c == '"' || c == '\'') && strings.HasPrefix(l[j:], strings.Repeat(string(c), 3)) {
				inTriple = strings.Repeat(string(c), 3)
				j += 2
				continue
			}
			if c == '"' || c == '\'' {
				k := j + 1
				for k < len(l) && l[k] != c {
					if l[k] == '\\' {
						k++
					}
					k++
				}
				b.WriteString(l[j:min(k+1, len(l))])
				j = k
				continue
			}
			b.WriteByte(c)
		}
		lines[i] = b.String()
	}
	return lines
}

// joinParens joins line i with following lines until parentheses balance (max 15 lines).
func (pf *pyFile) joinParens(i int) string {
	s := pf.lines[i]
	depth := strings.Count(s, "(") + strings.Count(s, "[") - strings.Count(s, ")") - strings.Count(s, "]")
	for k := i + 1; depth > 0 && k < len(pf.lines) && k < i+15; k++ {
		s += " " + strings.TrimSpace(pf.lines[k])
		depth += strings.Count(pf.lines[k], "(") + strings.Count(pf.lines[k], "[") - strings.Count(pf.lines[k], ")") - strings.Count(pf.lines[k], "]")
	}
	return s
}

// nextDef returns the function defined after decorator line i: name, def line (1-based) and body snippet.
func (pf *pyFile) nextDef(i int) (string, int, string) {
	for k := i + 1; k < len(pf.lines) && k < i+30; k++ {
		if m := rePyDef.FindStringSubmatch(pf.lines[k]); m != nil {
			return m[2], k + 1, pf.block(k, len(m[1]))
		}
		if m := rePyClass.FindStringSubmatch(pf.lines[k]); m != nil {
			return m[2], k + 1, pf.block(k, len(m[1]))
		}
	}
	return "", i + 1, ""
}

// block returns raw source lines from k while indentation is deeper than indent (max 150 lines).
func (pf *pyFile) block(k, indent int) string {
	raw := strings.Split(string(pf.f.src), "\n")
	end := k + 1
	for ; end < len(raw) && end < k+150; end++ {
		l := raw[end]
		if strings.TrimSpace(l) == "" {
			continue
		}
		if len(l)-len(strings.TrimLeft(l, " \t")) <= indent {
			break
		}
	}
	return strings.Join(raw[k:end], "\n")
}

// findDef finds `def name` in this file and returns its snippet.
func (pf *pyFile) findDef(name string) (int, string) {
	for k, l := range pf.lines {
		if m := rePyDef.FindStringSubmatch(l); m != nil && m[2] == name {
			return k + 1, pf.block(k, len(m[1]))
		}
		if m := rePyClass.FindStringSubmatch(l); m != nil && m[2] == name {
			return k + 1, pf.block(k, len(m[1]))
		}
	}
	return 0, ""
}

func (pf *pyFile) extract() (routes []pyRoute, items []model.Item, own map[string]string, incs []pyInclude, django []model.Item, djInc map[string]string) {
	own = map[string]string{}
	djInc = map[string]string{}
	rel := pf.f.rel
	isURLConf := path.Base(rel) == "urls.py"
	var kafkaGroup string
	imports := map[string]bool{}
	for i, l := range pf.lines {
		if m := rePyImport.FindStringSubmatch(l); m != nil {
			mod := m[1] + m[2]
			imports[strings.Split(mod, ".")[0]] = true
			for prefix, d := range pyDeps {
				if mod == prefix || strings.HasPrefix(mod, prefix+".") {
					items = append(items, depItem(d[0], d[1], rel, i+1, "python"))
					break
				}
			}
		}
		if m := rePyRouterVar.FindStringSubmatch(l); m != nil {
			full := pf.joinParens(i)
			if p := rePyPrefixKw.FindStringSubmatch(full); p != nil {
				own[m[1]] = p[1]
			} else {
				own[m[1]] = ""
			}
		}
		if m := rePyInclude.FindStringSubmatch(l); m != nil {
			full := pf.joinParens(i)
			ref := m[2]
			module, name := "", ref
			if j := strings.LastIndexByte(ref, '.'); j >= 0 {
				module, name = ref[:j], ref[j+1:]
			}
			p := ""
			if pm := rePyPrefixKw.FindStringSubmatch(full); pm != nil {
				p = pm[1]
			}
			incs = append(incs, pyInclude{module: module, name: name, prefix: p})
		}
		decoLine := l
		if strings.HasPrefix(strings.TrimSpace(l), "@") && strings.Contains(l, "(") {
			decoLine = pf.joinParens(i)
		}
		if m := rePyDecorator.FindStringSubmatch(decoLine); m != nil {
			full := decoLine
			methods := []string{strings.ToUpper(m[2])}
			switch m[2] {
			case "route", "api_route":
				methods = []string{"GET"} // Flask default
				if m[2] == "api_route" {
					methods = []string{"ANY"}
				}
				if mm := rePyMethods.FindStringSubmatch(full); mm != nil {
					methods = nil
					for _, s := range rePyStrList.FindAllStringSubmatch(mm[1], -1) {
						methods = append(methods, strings.ToUpper(s[1]))
					}
				}
			case "websocket":
				methods = []string{"GET"}
			}
			name, defLine, snip := pf.nextDef(i)
			fw := "flask"
			if imports["fastapi"] {
				fw = "fastapi"
			}
			for _, meth := range methods {
				routes = append(routes, pyRoute{model.Item{Kind: model.KindEndpoint, Key: route.Key(meth, m[4]), File: rel, Line: defLine,
					Framework: fw, Handler: name, Language: "python", Snippet: snip}, m[1]})
			}
			continue
		}
		if isURLConf {
			if m := rePyDjangoPath.FindStringSubmatch(l); m != nil {
				full := pf.joinParens(i)
				p := m[3]
				if m[1] != "path" {
					p = route.FromRegex(p)
				}
				p = "/" + strings.TrimLeft(p, "/")
				if inc := rePyDjangoInc.FindStringSubmatch(full); inc != nil {
					djInc[inc[1]] = p
					continue
				}
				view := strings.TrimSpace(strings.SplitN(m[4], ",", 2)[0])
				view = strings.TrimSuffix(strings.TrimSuffix(view, ")"), ".as_view(")
				view = strings.TrimSuffix(view, ".as_view()")
				if j := strings.LastIndexByte(view, '.'); j >= 0 {
					view = view[j+1:]
				}
				django = append(django, model.Item{Kind: model.KindEndpoint, Key: route.Key("ANY", p), File: rel, Line: i + 1,
					Framework: "django", Handler: view, Language: "python", Snippet: strings.TrimSpace(full)})
			}
		}
		if m := rePyCeleryTask.FindStringSubmatch(l); m != nil && (m[2] == "shared_task" || m[1] != "") {
			full := pf.joinParens(i)
			name, defLine, snip := pf.nextDef(i)
			if name == "" {
				continue
			}
			key := name
			if n := rePyNameKw.FindStringSubmatch(full); n != nil {
				key = n[1]
			}
			src := "celery"
			if q := rePyQueueKw.FindStringSubmatch(full); q != nil {
				src = q[1]
			}
			items = append(items, model.Item{Kind: model.KindConsumer, Key: key, Source: src, File: rel, Line: defLine,
				Framework: "celery", Handler: name, Language: "python", Snippet: snip, Meta: map[string]any{"task": name}})
		}
		if m := rePyConfGroup.FindStringSubmatch(l); m != nil {
			kafkaGroup = m[1]
		}
		if m := rePyKafkaCons.FindStringSubmatch(l); m != nil {
			full := pf.joinParens(i)
			key := m[1]
			if g := rePyGroupID.FindStringSubmatch(full); g != nil {
				key = g[1]
			}
			items = append(items, model.Item{Kind: model.KindConsumer, Key: key, Source: m[1], File: rel, Line: i + 1,
				Framework: "kafka-python", Language: "python", Snippet: pf.block(i, len(pf.lines[i])-len(strings.TrimLeft(pf.lines[i], " \t")))})
		}
		if m := rePySubscribe.FindStringSubmatch(l); m != nil && (imports["confluent_kafka"] || imports["aiokafka"]) {
			for _, t := range rePyStrList.FindAllStringSubmatch(m[1], -1) {
				key := t[1]
				if kafkaGroup != "" {
					key = kafkaGroup
				}
				items = append(items, model.Item{Kind: model.KindConsumer, Key: key, Source: t[1], File: rel, Line: i + 1,
					Framework: "confluent-kafka", Language: "python", Snippet: pf.block(i, len(pf.lines[i])-len(strings.TrimLeft(pf.lines[i], " \t")))})
			}
		}
		for _, m := range rePyURL.FindAllStringSubmatch(l, -1) {
			if h := hostOf(m[1]); h != "" {
				items = append(items, depItem("http", h, rel, i+1, "python"))
			}
		}
	}
	items = append(items, pf.beatSchedule()...)
	return routes, items, own, incs, django, djInc
}

// beatSchedule parses `beat_schedule = { 'name': { 'task': '…', 'schedule': crontab(…) } }` (also CELERYBEAT_SCHEDULE).
func (pf *pyFile) beatSchedule() []model.Item {
	src := strings.Join(pf.lines, "\n")
	idx := strings.Index(src, "beat_schedule")
	if idx < 0 {
		idx = strings.Index(src, "CELERYBEAT_SCHEDULE")
	}
	if idx < 0 {
		return nil
	}
	open := strings.IndexByte(src[idx:], '{')
	if open < 0 {
		return nil
	}
	start := idx + open
	end := matchBrace(src, start)
	body := src[start+1 : end]
	var out []model.Item
	for pos := 0; pos < len(body); {
		loc := rePyBeatEntry.FindStringSubmatchIndex(body[pos:])
		if loc == nil {
			break
		}
		name := body[pos+loc[2] : pos+loc[3]]
		bOpen := pos + loc[1] - 1
		bEnd := matchBrace(body, bOpen)
		entry := body[bOpen:min(bEnd+1, len(body))]
		task := rePyBeatTask.FindStringSubmatch(entry)
		sched := rePyBeatSched.FindStringSubmatch(entry)
		if task != nil {
			s := ""
			if sched != nil {
				s = beatToCron(strings.TrimSpace(sched[1]))
			}
			line := strings.Count(src[:start+1+pos+loc[0]], "\n") + 1
			out = append(out, model.Item{Kind: model.KindCron, Key: name, Source: s, File: pf.f.rel, Line: line,
				Framework: "celery-beat", Handler: task[1], Language: "python", Snippet: strings.TrimSpace(entry),
				Meta: map[string]any{"task": task[1]}})
		}
		pos = bEnd + 1
	}
	return out
}

// beatToCron renders crontab(minute='*/5') as "*/5 * * * *"; other schedules are kept as written.
func beatToCron(s string) string {
	if !strings.HasPrefix(s, "crontab(") {
		return s
	}
	f := map[string]string{"minute": "*", "hour": "*", "day_of_month": "*", "month_of_year": "*", "day_of_week": "*"}
	for _, m := range rePyCrontabArgs.FindAllStringSubmatch(s, -1) {
		f[m[1]] = strings.TrimSpace(m[2])
	}
	return f["minute"] + " " + f["hour"] + " " + f["day_of_month"] + " " + f["month_of_year"] + " " + f["day_of_week"]
}

func matchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(s) - 1
}
