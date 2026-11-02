package extract

import (
	"regexp"
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

// Java and Kotlin are read by one scanner: cleanJVM blanks comments (and, in a second copy, string contents) while
// keeping every byte offset, so regexes run on the code copy never match inside comments or strings and string
// values are read from the raw copy at the same offsets.

var (
	reJvmAnno      = regexp.MustCompile(`@(?:[a-z]+:)?([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)`)
	reJvmGap       = regexp.MustCompile(`^(?:\s+|\b(?:public|private|protected|static|final|abstract|open|override|suspend|internal|inline|synchronized|default|native|transient|volatile|data|sealed|enum|inner|lateinit|operator|infix|tailrec|external|const|annotation|value|strictfp)\b)*$`)
	reJvmClassKw   = regexp.MustCompile(`\b(class|interface|object|record)\s+([A-Za-z_]\w*)`)
	reJvmClassDecl = regexp.MustCompile(`(?:^|[^\w.:@$])(class|interface|object|record)\s+([A-Za-z_]\w*)`)
	reJvmLastIdent = regexp.MustCompile(`([A-Za-z_]\w*)\s*$`)
	reJvmExtends   = regexp.MustCompile(`\bextends\s+([\w.]+)`)
	reJvmLeadIdent = regexp.MustCompile(`^\s*([\w.]+)`)
	reJvmNamedArg  = regexp.MustCompile(`^([A-Za-z_]\w*)\s*=\s*([\s\S]*)$`)
	reJvmQualIdent = regexp.MustCompile(`^[A-Za-z_][\w]*(?:\.[A-Za-z_]\w*)*$`)
	reJvmArrayCall = regexp.MustCompile(`^(?:arrayOf|listOf|setOf|mutableListOf|new\s+String\s*\[\s*\])\s*[({]([\s\S]*)[)}]$`)
	reJvmTemplate  = regexp.MustCompile(`\$\{([A-Za-z_]\w*)\}|\$([A-Za-z_]\w*)`)
	reJvmConst     = regexp.MustCompile(`(?:\bconst\s+val|\bstatic\s+final\s+String|\bfinal\s+static\s+String|\bval|\bString)\s+([A-Za-z_]\w*)\s*(?::\s*String\s*)?=\s*([^;\n})]+)`)
	reJvmImport    = regexp.MustCompile(`(?m)^\s*import\s+(?:static\s+)?([\w.]+)`)
	reJvmStrLit    = regexp.MustCompile(`"[^"\n]*"`)
	reJvmHTTPMark  = regexp.MustCompile(`RestTemplate|restTemplate|WebClient|webClient|RestClient|restClient|\.uri\s*\(|\.url\s*\(|baseUrl\s*\(|URI\.create\s*\(|\bURI\s*\(|\bURL\s*\(|HttpRequest|For(?:Object|Entity)\s*\(|\.exchange\s*\(|Request\.Builder|Http(?:Get|Post|Put|Delete)\s*\(|@FeignClient|@RegisterRestClient|@Client\s*\(|\b(?:[cC]lient|http)\.(?:get|post|put|patch|delete|request)\s*\(`)
	reJvmUpperId   = regexp.MustCompile(`\b([A-Z][A-Z0-9_]+)\b`)
	reKtorCall     = regexp.MustCompile(`(?:^|[^\w.])(route|get|post|put|delete|patch|head|options)\s*(?:\(\s*("[^"\n]*")\s*(?:,[^(){}\n]*)?\))?\s*\{`)
	reKtorExtFun   = regexp.MustCompile(`\bfun\s+Route\.([A-Za-z_]\w*)\s*\([^)]*\)\s*(?::\s*\w+\s*)?\{`)
	reKtorCallFn   = regexp.MustCompile(`(?:^|[^\w.])([a-z]\w*)\s*\(\s*\)`)
	reJvmFnRoute   = regexp.MustCompile(`(?:^|[^\w@])(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s*\(\s*("[^"\n]*")(?:\s*\)?\s*,\s*([\w.]+::\w+))?`)
	reJvmFnNest    = regexp.MustCompile(`(?:\bpath\s*\(\s*("[^"\n]*")\s*\)|("[^"\n]*"))\s*\.\s*nest\s*\{`)
	reJvmCompose   = regexp.MustCompile(`(?:^|[^\w.])composable\s*(?:<\s*([\w.]+)\s*>)?\s*(?:\(\s*(?:route\s*=\s*)?(?:("[^"\n]*")|([A-Za-z_][\w.]*))?)?`)
)

var jvmMappings = map[string]string{"GetMapping": "GET", "PostMapping": "POST", "PutMapping": "PUT", "DeleteMapping": "DELETE",
	"PatchMapping": "PATCH", "RequestMapping": ""}

var jaxrsMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true, "HEAD": true, "OPTIONS": true}

var micronautMethods = map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH",
	"Head": "HEAD", "Options": "OPTIONS"}

var androidBases = map[string]bool{"Activity": true, "AppCompatActivity": true, "ComponentActivity": true, "FragmentActivity": true,
	"Fragment": true, "DialogFragment": true, "BottomSheetDialogFragment": true, "ListActivity": true, "PreferenceFragmentCompat": true}

var jvmDeps = []struct{ prefix, kind, name string }{
	{"org.postgresql", "db", "postgres"}, {"io.r2dbc.postgresql", "db", "postgres"}, {"io.vertx.pgclient", "db", "postgres"},
	{"io.vertx.mutiny.pgclient", "db", "postgres"},
	{"com.mysql", "db", "mysql"}, {"org.mariadb", "db", "mysql"}, {"io.vertx.mysqlclient", "db", "mysql"},
	{"com.mongodb", "db", "mongodb"}, {"org.springframework.data.mongodb", "db", "mongodb"}, {"io.quarkus.mongodb", "db", "mongodb"},
	{"org.litote.kmongo", "db", "mongodb"},
	{"redis.clients.jedis", "cache", "redis"}, {"io.lettuce", "cache", "redis"}, {"org.springframework.data.redis", "cache", "redis"},
	{"org.redisson", "cache", "redis"}, {"io.quarkus.redis", "cache", "redis"}, {"io.vertx.redis", "cache", "redis"},
	{"org.apache.kafka", "queue", "kafka"}, {"org.springframework.kafka", "queue", "kafka"}, {"io.micronaut.configuration.kafka", "queue", "kafka"},
	{"io.smallrye.reactive.messaging.kafka", "queue", "kafka"}, {"reactor.kafka", "queue", "kafka"},
	{"com.rabbitmq", "queue", "rabbitmq"}, {"org.springframework.amqp", "queue", "rabbitmq"}, {"io.micronaut.rabbitmq", "queue", "rabbitmq"},
	{"software.amazon.awssdk.services.sqs", "queue", "sqs"}, {"com.amazonaws.services.sqs", "queue", "sqs"}, {"io.awspring.cloud.sqs", "queue", "sqs"},
	{"software.amazon.awssdk.services.dynamodb", "db", "dynamodb"}, {"com.amazonaws.services.dynamodbv2", "db", "dynamodb"},
	{"co.elastic.clients", "db", "elasticsearch"}, {"org.elasticsearch", "db", "elasticsearch"},
	{"org.springframework.data.elasticsearch", "db", "elasticsearch"}, {"com.datastax", "db", "cassandra"},
	{"com.stripe", "http", "api.stripe.com"},
	{"javax.persistence", "db", "sql"}, {"jakarta.persistence", "db", "sql"}, {"org.springframework.data.jpa", "db", "sql"},
	{"org.springframework.jdbc", "db", "sql"}, {"org.springframework.data.jdbc", "db", "sql"}, {"org.jooq", "db", "sql"},
	{"org.hibernate", "db", "sql"}, {"io.quarkus.hibernate", "db", "sql"}, {"org.jetbrains.exposed", "db", "sql"},
	{"org.jdbi", "db", "sql"}, {"com.zaxxer.hikari", "db", "sql"},
}

type jvmAnno struct {
	name    string // simple name ("GetMapping" for "org.springframework...GetMapping")
	hasArgs bool
	args    map[string]string // named args; positional args under "value"
	off     int
	end     int
}

type jvmClass struct {
	name                string
	nameOff             int
	bodyOpen, bodyClose int // -1 without a body
	annos               []jvmAnno
	bases               []string
	abstract, iface     bool
}

type jvmMethod struct {
	name  string
	annos []jvmAnno
	decl  int // offset of the declaration after its annotations
	class *jvmClass
}

type jvmFile struct {
	f       srcFile
	raw     string // comments blanked
	code    string // comments and string contents blanked
	starts  []int
	consts  map[string]string
	imports []string
	classes []*jvmClass
	methods []*jvmMethod
}

// extractJVM extracts Java and Kotlin items: Spring MVC/WebFlux (annotations + functional routes), JAX-RS/Quarkus,
// Micronaut, Ktor, spring-kafka/amqp/jms/sqs + SmallRye/Micronaut listeners, @Scheduled jobs, Android
// Activities/Fragments + Compose Navigation, and driver/HTTP dependencies.
func extractJVM(files []srcFile) []model.Item {
	var parsed []*jvmFile
	for _, f := range files {
		parsed = append(parsed, parseJVM(f))
	}
	var items []model.Item
	for _, jf := range parsed {
		items = append(items, jf.annotated()...)
		items = append(items, jf.functionalRoutes()...)
		items = append(items, jf.composeViews()...)
		items = append(items, jf.dependencies()...)
	}
	items = append(items, jvmKtorRoutes(parsed)...)
	items = append(items, jvmAndroidViews(parsed)...)
	return items
}

func parseJVM(f srcFile) *jvmFile {
	raw, code := cleanJVM(f.src)
	jf := &jvmFile{f: f, raw: raw, code: code, starts: []int{0}, consts: map[string]string{}}
	for i := 0; i < len(code); i++ {
		if code[i] == '\n' {
			jf.starts = append(jf.starts, i+1)
		}
	}
	for _, m := range reJvmImport.FindAllStringSubmatch(code, -1) {
		jf.imports = append(jf.imports, m[1])
	}
	jf.collectConsts()
	jf.collectDecls()
	return jf
}

// cleanJVM returns src with comments blanked (raw) and additionally with string/char literal contents blanked
// (code). Quotes stay in code; newlines and byte offsets are preserved in both.
func cleanJVM(src []byte) (raw, code string) {
	n := len(src)
	r, c := make([]byte, n), make([]byte, n)
	blank := func(i int) {
		if src[i] == '\n' {
			r[i], c[i] = '\n', '\n'
		} else {
			r[i], c[i] = ' ', ' '
		}
	}
	keep := func(i int) { r[i], c[i] = src[i], src[i] }
	str := func(i int) {
		r[i] = src[i]
		if src[i] == '\n' {
			c[i] = '\n'
		} else {
			c[i] = ' '
		}
	}
	triple := func(i int) bool { return i+2 < n && src[i] == '"' && src[i+1] == '"' && src[i+2] == '"' }
	for i := 0; i < n; {
		ch := src[i]
		switch {
		case ch == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				blank(i)
				i++
			}
		case ch == '/' && i+1 < n && src[i+1] == '*':
			blank(i)
			blank(i + 1)
			i += 2
			for i < n && !(src[i] == '*' && i+1 < n && src[i+1] == '/') {
				blank(i)
				i++
			}
			for k := 0; k < 2 && i < n; k++ {
				blank(i)
				i++
			}
		case triple(i):
			for k := 0; k < 3; k++ {
				keep(i)
				i++
			}
			for i < n && !triple(i) {
				if src[i] == '\\' && i+1 < n {
					str(i)
					i++
				}
				str(i)
				i++
			}
			for k := 0; k < 3 && i < n; k++ {
				keep(i)
				i++
			}
		case ch == '"':
			keep(i)
			i++
			for i < n && src[i] != '"' && src[i] != '\n' {
				switch {
				case src[i] == '\\' && i+1 < n:
					str(i)
					str(i + 1)
					i += 2
				case src[i] == '$' && i+1 < n && src[i+1] == '{': // Kotlin template / Spring placeholder: may hold quotes
					str(i)
					i++
					for depth := 0; i < n && src[i] != '\n'; {
						if src[i] == '{' {
							depth++
						} else if src[i] == '}' {
							depth--
						}
						str(i)
						i++
						if depth == 0 {
							break
						}
					}
				default:
					str(i)
					i++
				}
			}
			if i < n {
				keep(i)
				i++
			}
		case ch == '\'':
			k := i + 1
			if k < n && src[k] == '\\' {
				k += 2
			}
			for k < n && k < i+10 && src[k] != '\'' && src[k] != '\n' {
				k++
			}
			if k < n && src[k] == '\'' {
				keep(i)
				for m := i + 1; m < k; m++ {
					str(m)
				}
				keep(k)
				i = k + 1
			} else {
				keep(i)
				i++
			}
		default:
			keep(i)
			i++
		}
	}
	return string(r), string(c)
}

func (jf *jvmFile) line(off int) int {
	return sort.Search(len(jf.starts), func(i int) bool { return jf.starts[i] > off })
}

func (jf *jvmFile) lang() string { return jf.f.lang }

func (jf *jvmFile) hasImport(prefix string) bool {
	for _, im := range jf.imports {
		if im == prefix || strings.HasPrefix(im, prefix+".") {
			return true
		}
	}
	return false
}

// lit returns the raw text of the string literal whose quotes span code[s:e] (quotes excluded, unescaped).
func (jf *jvmFile) lit(s, e int) string {
	return jf.template(unescapeJVM(jf.raw[s+1 : e-1]))
}

func unescapeJVM(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// template resolves Kotlin string templates that name a constant of the file.
func (jf *jvmFile) template(s string) string {
	if jf.lang() != "kotlin" || !strings.Contains(s, "$") {
		return s
	}
	return reJvmTemplate.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.Trim(m, "${}")
		if v, ok := jf.consts[name]; ok {
			return v
		}
		return m
	})
}

// matchClose returns the offset of the bracket closing code[open], or -1.
func jvmClose(code string, open int) int {
	o := code[open]
	cl := map[byte]byte{'(': ')', '{': '}', '[': ']', '<': '>'}[o]
	depth := 0
	for i := open; i < len(code); i++ {
		switch code[i] {
		case o:
			depth++
		case cl:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// jvmSplit splits s at sep outside quotes and brackets.
func jvmSplit(s string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		default:
			if c == sep && depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func jvmArgs(s string) map[string]string {
	m := map[string]string{}
	var pos []string
	for _, p := range jvmSplit(s, ',') {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if k := reJvmNamedArg.FindStringSubmatch(p); k != nil && !strings.HasPrefix(k[2], "=") {
			m[k[1]] = strings.TrimSpace(k[2])
			continue
		}
		pos = append(pos, p)
	}
	if _, ok := m["value"]; !ok && len(pos) > 0 {
		m["value"] = strings.Join(pos, ",") // Kotlin vararg-style array value: @GetMapping("/a", "/b")
	}
	return m
}

func jvmUnwrap(expr string) string {
	for {
		expr = strings.TrimSpace(expr)
		switch {
		case len(expr) >= 2 && ((expr[0] == '{' && expr[len(expr)-1] == '}') || (expr[0] == '[' && expr[len(expr)-1] == ']')):
			expr = expr[1 : len(expr)-1]
			continue
		}
		if m := reJvmArrayCall.FindStringSubmatch(expr); m != nil {
			expr = m[1]
			continue
		}
		return expr
	}
}

// strs evaluates an annotation argument to its string values: literals, arrays, constants and concatenations.
// Elements that cannot be resolved are dropped.
func (jf *jvmFile) strs(expr string) []string {
	var out []string
	for _, el := range jvmSplit(jvmUnwrap(expr), ',') {
		if strings.TrimSpace(el) == "" {
			continue
		}
		if v, ok := jf.eval(el); ok {
			out = append(out, v)
		}
	}
	return out
}

func (jf *jvmFile) eval(el string) (string, bool) {
	var b strings.Builder
	for _, p := range jvmSplit(el, '+') {
		p = strings.TrimSpace(p)
		switch {
		case len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"':
			b.WriteString(jf.template(unescapeJVM(p[1 : len(p)-1])))
		case reJvmQualIdent.MatchString(p):
			v, ok := jf.consts[jvmLast(p)]
			if !ok {
				return "", false
			}
			b.WriteString(v)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// jvmIdents returns the last segments of the identifiers of an argument (RequestMethod.GET → GET).
func jvmIdents(expr string) []string {
	var out []string
	for _, el := range jvmSplit(jvmUnwrap(expr), ',') {
		if el = strings.TrimSpace(el); el != "" {
			out = append(out, jvmLast(el))
		}
	}
	return out
}

func jvmLast(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (jf *jvmFile) collectConsts() {
	ms := reJvmConst.FindAllStringSubmatchIndex(jf.code, -1)
	for pass := 0; pass < 2; pass++ { // second pass resolves constants built from later ones
		for _, m := range ms {
			name := jf.code[m[2]:m[3]]
			if _, ok := jf.consts[name]; ok {
				continue
			}
			expr := jf.raw[m[4]:m[5]]
			if !strings.Contains(expr, `"`) && pass == 0 {
				continue
			}
			if v, ok := jf.eval(strings.TrimSpace(expr)); ok {
				jf.consts[name] = v
			}
		}
	}
}

// annotation parses the annotation at code[m[0]:m[1]].
func (jf *jvmFile) annotation(m []int) jvmAnno {
	a := jvmAnno{name: jvmLast(jf.code[m[2]:m[3]]), off: m[0], end: m[1]}
	j := m[1]
	for j < len(jf.code) && (jf.code[j] == ' ' || jf.code[j] == '\t') {
		j++
	}
	if j < len(jf.code) && jf.code[j] == '(' {
		if cl := jvmClose(jf.code, j); cl > 0 {
			a.hasArgs, a.args, a.end = true, jvmArgs(jf.raw[j+1:cl]), cl+1
		}
	}
	if a.args == nil {
		a.args = map[string]string{}
	}
	return a
}

// collectDecls groups annotations with the class/method they annotate and finds every class declaration.
func (jf *jvmFile) collectDecls() {
	code := jf.code
	var annos []jvmAnno
	last := -1
	for _, m := range reJvmAnno.FindAllStringSubmatchIndex(code, -1) {
		if m[0] < last || code[m[2]:m[3]] == "interface" {
			continue // nested in another annotation's arguments, or an annotation declaration
		}
		if m[0] > 0 && jvmWordByte(code[m[0]-1]) {
			continue // Kotlin label / qualified this: return@forEach, this@MainActivity
		}
		a := jf.annotation(m)
		annos = append(annos, a)
		last = a.end
	}
	classAnnos := map[int][]jvmAnno{}
	skipTo := 0
	for i := 0; i < len(annos); {
		if annos[i].off < skipTo { // parameter annotations
			i++
			continue
		}
		group := []jvmAnno{annos[i]}
		j := i + 1
		for j < len(annos) && reJvmGap.MatchString(code[group[len(group)-1].end:annos[j].off]) {
			group = append(group, annos[j])
			j++
		}
		i = j
		kind, name, declOff, nameOff, next := jf.declAt(group[len(group)-1].end)
		switch kind {
		case "class":
			classAnnos[nameOff] = group
		case "method":
			jf.methods = append(jf.methods, &jvmMethod{name: name, annos: group, decl: declOff})
		}
		skipTo = next
	}
	for _, m := range reJvmClassDecl.FindAllStringSubmatchIndex(code, -1) {
		c := &jvmClass{name: code[m[4]:m[5]], nameOff: m[4], bodyOpen: -1, bodyClose: -1, annos: classAnnos[m[4]],
			iface: code[m[2]:m[3]] == "interface"}
		ls := jf.starts[jf.line(m[2])-1]
		pre := code[ls:m[2]]
		c.abstract = strings.Contains(pre, "abstract") || strings.Contains(pre, "sealed")
		hdr, open := jf.classHeader(m[5])
		if open >= 0 {
			c.bodyOpen, c.bodyClose = open, jvmClose(code, open)
			if c.bodyClose < 0 {
				c.bodyClose = len(code)
			}
		}
		c.bases = jvmBases(hdr)
		jf.classes = append(jf.classes, c)
	}
	for _, m := range jf.methods {
		for _, c := range jf.classes {
			if c.bodyOpen >= 0 && c.bodyOpen < m.decl && m.decl < c.bodyClose &&
				(m.class == nil || c.bodyOpen > m.class.bodyOpen) {
				m.class = c
			}
		}
	}
}

// declAt classifies the declaration following annotations at pos.
func (jf *jvmFile) declAt(pos int) (kind, name string, declOff, nameOff, next int) {
	code := jf.code
	for pos < len(code) && (code[pos] == ' ' || code[pos] == '\t' || code[pos] == '\n' || code[pos] == '\r') {
		pos++
	}
	end := min(len(code), pos+600)
	stop := strings.IndexAny(code[pos:end], "({;=")
	if stop < 0 {
		return "", "", pos, 0, pos
	}
	hdr := code[pos : pos+stop]
	if m := reJvmClassKw.FindStringSubmatchIndex(hdr); m != nil {
		return "class", hdr[m[4]:m[5]], pos, pos + m[4], pos + m[5]
	}
	if code[pos+stop] != '(' {
		return "field", "", pos, 0, pos + stop
	}
	next = pos + stop + 1
	if cl := jvmClose(code, pos+stop); cl > 0 {
		next = cl + 1
	}
	m := reJvmLastIdent.FindStringSubmatch(hdr)
	if m == nil {
		return "", "", pos, 0, next
	}
	switch m[1] {
	case "if", "for", "while", "switch", "catch", "return", "new", "synchronized", "when", "throw":
		return "", "", pos, 0, next
	}
	return "method", m[1], pos, 0, next
}

// classHeader returns the supertype part of a class header (after name, generics and primary constructor) and
// the offset of the body's '{' (-1 when the class has no body).
func (jf *jvmFile) classHeader(i int) (string, int) {
	code := jf.code
	skipWS := func() {
		for i < len(code) && (code[i] == ' ' || code[i] == '\t' || code[i] == '\n' || code[i] == '\r') {
			i++
		}
	}
	skipWS()
	if i < len(code) && code[i] == '<' {
		if cl := jvmClose(code, i); cl > 0 {
			i = cl + 1
		}
	}
	skipWS()
	for i < len(code) && (code[i] == '(' || strings.HasPrefix(code[i:], "private constructor") || strings.HasPrefix(code[i:], "constructor")) {
		if code[i] != '(' {
			j := strings.IndexByte(code[i:], '(')
			if j < 0 {
				break
			}
			i += j
		}
		cl := jvmClose(code, i)
		if cl < 0 {
			break
		}
		i = cl + 1
		skipWS()
	}
	start := i
	for ; i < len(code) && i < start+2000; i++ {
		switch code[i] {
		case '{':
			return code[start:i], i
		case ';', '}', '@':
			return code[start:i], -1
		case '(':
			if cl := jvmClose(code, i); cl > 0 {
				i = cl
			}
		case '\n':
			hdr := strings.TrimSpace(code[start:i])
			rest := strings.TrimLeft(code[i:], " \t\r\n")
			if !strings.HasSuffix(hdr, ",") && !strings.HasSuffix(hdr, ":") && hdr != "" &&
				!strings.HasPrefix(rest, "{") && !strings.HasPrefix(rest, ":") && !strings.HasPrefix(rest, ",") &&
				!strings.HasPrefix(rest, "extends") && !strings.HasPrefix(rest, "implements") && !strings.HasPrefix(rest, "where") &&
				!strings.HasPrefix(rest, "permits") {
				return code[start:i], -1
			}
		}
	}
	return code[start:i], -1
}

func jvmBases(hdr string) []string {
	var out []string
	add := func(s string) {
		if m := reJvmLeadIdent.FindStringSubmatch(s); m != nil {
			out = append(out, jvmLast(m[1]))
		}
	}
	if m := reJvmExtends.FindStringSubmatch(hdr); m != nil {
		add(m[1])
	}
	if t := strings.TrimSpace(hdr); strings.HasPrefix(t, ":") {
		for _, s := range jvmSplit(t[1:], ',') {
			add(s)
		}
	}
	return out
}

func jvmAnnoNamed(annos []jvmAnno, name string) (jvmAnno, bool) {
	for _, a := range annos {
		if a.name == name {
			return a, true
		}
	}
	return jvmAnno{}, false
}

func (jf *jvmFile) snippet(first, decl int) string {
	fl, dl := jf.line(first), jf.line(decl)
	if dl > fl {
		return linesRange(jf.f.src, fl, dl-1) + "\n" + braceBlock(jf.f.src, dl)
	}
	return braceBlock(jf.f.src, dl)
}

func (jf *jvmFile) item(kind, key, source, fw, handler string, off int, snip string) model.Item {
	return model.Item{Kind: kind, Key: key, Source: source, File: jf.f.rel, Line: jf.line(off), Framework: fw,
		Handler: handler, Language: jf.lang(), Snippet: snip}
}

// routePaths returns the route values of an annotation: [""] when it names none (the prefix itself), nothing when
// they cannot be resolved (an unknown constant).
func (jf *jvmFile) routePaths(a jvmAnno, keys ...string) []string {
	for _, k := range keys {
		if _, ok := a.args[k]; ok {
			return jf.paths(a, keys...)
		}
	}
	return []string{""}
}

func (jf *jvmFile) paths(a jvmAnno, keys ...string) []string {
	var out []string
	for _, k := range keys {
		if v, ok := a.args[k]; ok {
			out = append(out, jf.strs(v)...)
		}
	}
	return out
}

// annotated extracts annotation-driven endpoints, consumers and crons.
func (jf *jvmFile) annotated() []model.Item {
	var items []model.Item
	retrofit := jf.hasImport("retrofit2")
	quarkusSched, micronautSched := jf.hasImport("io.quarkus.scheduler"), jf.hasImport("io.micronaut.scheduling")
	micronaut := jf.hasImport("io.micronaut")
	for _, m := range jf.methods {
		var cAnnos []jvmAnno
		cname := ""
		if m.class != nil {
			cAnnos, cname = m.class.annos, m.class.name+"."
		}
		_, feign := jvmAnnoNamed(cAnnos, "FeignClient")
		_, exchange := jvmAnnoNamed(cAnnos, "HttpExchange")
		_, restClient := jvmAnnoNamed(cAnnos, "RegisterRestClient")
		_, mnClient := jvmAnnoNamed(cAnnos, "Client")
		handler := cname + m.name
		first := m.annos[0].off
		snip := jf.snippet(first, m.decl)
		emit := func(kind, key, source, fw string, a jvmAnno) {
			items = append(items, jf.item(kind, key, source, fw, handler, a.off, snip))
		}
		// Spring MVC / WebFlux annotations
		if !feign && !exchange {
			for _, a := range m.annos {
				method, ok := jvmMappings[a.name]
				if !ok {
					continue
				}
				methods := []string{method}
				if a.name == "RequestMapping" {
					methods = jvmIdents(a.args["method"])
					if len(methods) == 0 {
						methods = []string{"ANY"}
					}
				}
				prefixes := []string{""}
				if ca, ok := jvmAnnoNamed(cAnnos, "RequestMapping"); ok {
					prefixes = jf.routePaths(ca, "value", "path")
				}
				for _, pre := range prefixes {
					for _, p := range jf.routePaths(a, "value", "path") {
						for _, me := range methods {
							emit(model.KindEndpoint, route.Key(me, route.Join(pre, p)), "", "spring", a)
						}
					}
				}
			}
		}
		// JAX-RS / Quarkus / Jakarta
		if !retrofit && !restClient {
			cp, cHas := jvmAnnoNamed(cAnnos, "Path")
			mp, mHas := jvmAnnoNamed(m.annos, "Path")
			if cHas || mHas {
				for _, a := range m.annos {
					if !jaxrsMethods[a.name] || a.hasArgs {
						continue
					}
					for _, pre := range jf.routePaths(cp, "value") {
						for _, p := range jf.routePaths(mp, "value") {
							emit(model.KindEndpoint, route.Key(a.name, route.Join(pre, p)), "", "jaxrs", a)
						}
					}
				}
			}
		}
		// Micronaut
		if ctl, ok := jvmAnnoNamed(cAnnos, "Controller"); (ok || micronaut) && !mnClient {
			for _, a := range m.annos {
				method, ok := micronautMethods[a.name]
				if !ok {
					continue
				}
				for _, pre := range jf.routePaths(ctl, "value") {
					for _, p := range jf.routePaths(a, "value", "uri", "uris") {
						emit(model.KindEndpoint, route.Key(method, route.Join(pre, p)), "", "micronaut", a)
					}
				}
			}
		}
		// Consumers
		for _, a := range m.annos {
			switch a.name {
			case "KafkaListener":
				if _, ok := jvmAnnoNamed(m.annos, "Topic"); ok {
					continue // Micronaut, handled with @Topic below
				}
				if c := jf.springKafka(a); c != nil {
					emit(model.KindConsumer, c[0], c[1], "spring-kafka", a)
				}
			case "Topic":
				topics := strings.Join(jf.paths(a, "value", "topics"), ",")
				group := ""
				for _, src := range [][]jvmAnno{m.annos, cAnnos} {
					if k, ok := jvmAnnoNamed(src, "KafkaListener"); ok && group == "" {
						group = strings.Join(jf.paths(k, "groupId", "value"), ",")
					}
				}
				if topics != "" {
					emit(model.KindConsumer, jvmFirst(group, topics), topics, "micronaut-kafka", a)
				}
			case "RabbitListener":
				if c := jf.rabbit(a); c != nil {
					emit(model.KindConsumer, c[0], c[1], "spring-amqp", a)
				}
			case "Queue": // Micronaut RabbitMQ: @RabbitListener class + @Queue method
				if _, ok := jvmAnnoNamed(cAnnos, "RabbitListener"); ok {
					if q := strings.Join(jf.paths(a, "value"), ","); q != "" {
						emit(model.KindConsumer, q, q, "micronaut-rabbitmq", a)
					}
				}
			case "JmsListener":
				if d := strings.Join(jf.paths(a, "destination"), ","); d != "" {
					emit(model.KindConsumer, jvmFirst(strings.Join(jf.paths(a, "id"), ""), d), d, "spring-jms", a)
				}
			case "SqsListener":
				if q := strings.Join(jf.paths(a, "value", "queueNames"), ","); q != "" {
					emit(model.KindConsumer, jvmFirst(strings.Join(jf.paths(a, "id"), ""), q), q, "spring-cloud-aws", a)
				}
			case "Incoming":
				if ch := strings.Join(jf.paths(a, "value"), ","); ch != "" {
					emit(model.KindConsumer, ch, ch, "smallrye-reactive-messaging", a)
				}
			case "Scheduled":
				fw := "spring"
				if quarkusSched {
					fw = "quarkus"
				} else if micronautSched {
					fw = "micronaut"
				}
				if s := jf.schedule(a); s != "" {
					emit(model.KindCron, m.name, s, fw, a)
				}
			}
		}
	}
	// Class-level listeners with @KafkaHandler / @RabbitHandler methods, Feign / REST clients as dependencies.
	for _, c := range jf.classes {
		for _, a := range c.annos {
			snip := braceBlock(jf.f.src, jf.line(c.nameOff))
			switch a.name {
			case "KafkaListener":
				if cc := jf.springKafka(a); cc != nil {
					items = append(items, jf.item(model.KindConsumer, cc[0], cc[1], "spring-kafka", c.name, a.off, snip))
				}
			case "RabbitListener":
				if cc := jf.rabbit(a); cc != nil {
					items = append(items, jf.item(model.KindConsumer, cc[0], cc[1], "spring-amqp", c.name, a.off, snip))
				}
			case "FeignClient", "RegisterRestClient", "Client":
				for _, u := range jf.paths(a, "url", "baseUri", "value") {
					if h := hostOf(u); h != "" {
						items = append(items, depItem("http", h, jf.f.rel, jf.line(a.off), jf.lang()))
					}
				}
			}
		}
	}
	return items
}

func jvmWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func jvmFirst(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// springKafka returns [key, source] of a spring-kafka @KafkaListener (nil without topics).
func (jf *jvmFile) springKafka(a jvmAnno) []string {
	topics := strings.Join(jf.paths(a, "topics"), ",")
	if topics == "" {
		topics = strings.Join(jf.paths(a, "topicPattern"), ",")
	}
	if topics == "" {
		return nil
	}
	group := strings.Join(jf.paths(a, "groupId"), ",")
	if group == "" {
		group = strings.Join(jf.paths(a, "id", "value"), ",") // the listener id is the group by default (idIsGroup)
	}
	return []string{jvmFirst(group, topics), topics}
}

var reJvmQueueAnno = regexp.MustCompile(`@Queue\s*\(\s*(?:(?:value|name)\s*=\s*)?("[^"\n]*"|[A-Za-z_][\w.]*)`)

func (jf *jvmFile) rabbit(a jvmAnno) []string {
	qs := jf.paths(a, "queues", "value")
	for _, k := range []string{"queuesToDeclare", "bindings"} {
		for _, m := range reJvmQueueAnno.FindAllStringSubmatch(a.args[k], -1) {
			qs = append(qs, jf.strs(m[1])...)
		}
	}
	if len(qs) == 0 {
		return nil
	}
	q := strings.Join(qs, ",")
	return []string{jvmFirst(strings.Join(jf.paths(a, "id"), ""), q), q}
}

// schedule renders a @Scheduled annotation: cron expression or "every <interval>".
func (jf *jvmFile) schedule(a jvmAnno) string {
	if c := jf.paths(a, "cron", "value"); len(c) > 0 && c[0] != "" {
		return c[0]
	}
	for _, k := range []string{"every", "fixedRate", "fixedDelay", "fixedRateString", "fixedDelayString"} {
		v, ok := a.args[k]
		if !ok {
			continue
		}
		if s := jf.strs(v); len(s) > 0 {
			return "every " + s[0]
		}
		n := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(v), "_", ""), "lL")
		if n != "" && strings.Trim(n, "0123456789") == "" {
			unit := map[string]string{"NANOSECONDS": "ns", "MICROSECONDS": "us", "SECONDS": "s", "MINUTES": "m", "HOURS": "h",
				"DAYS": "d"}[jvmLast(strings.TrimSpace(a.args["timeUnit"]))]
			return "every " + n + jvmFirst(unit, "ms")
		}
	}
	return ""
}

// scope is an open brace that adds a route prefix (Ktor route(), WebFlux nest) or starts a Ktor Route extension.
type jvmScope struct {
	prefix string
	fn     string
	depth  int
}

type jvmEvent struct {
	off    int
	brace  int // scope: offset of its '{'
	scope  bool
	call   bool // call of a Route extension function
	prefix string
	fn     string
	method string
	path   string
	handle string
}

// jvmWalkScopes visits events in order with the route prefix of the enclosing scopes and the enclosing Route
// extension function.
func jvmWalkScopes(code string, evs []jvmEvent, visit func(ev jvmEvent, prefix, fn string)) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].off < evs[j].off })
	pending := map[int]jvmEvent{}
	var stack []jvmScope
	depth, k := 0, 0
	cur := func() (string, string) {
		p, fn := "", ""
		for _, s := range stack {
			p = route.Join(p, s.prefix)
			if s.fn != "" {
				fn, p = s.fn, ""
			}
		}
		return p, fn
	}
	for i := 0; i <= len(code); i++ {
		for k < len(evs) && evs[k].off == i {
			ev := evs[k]
			if ev.scope {
				pending[ev.brace] = ev
			} else {
				p, fn := cur()
				visit(ev, p, fn)
			}
			k++
		}
		if i == len(code) {
			break
		}
		switch code[i] {
		case '{':
			depth++
			if ev, ok := pending[i]; ok {
				stack = append(stack, jvmScope{prefix: ev.prefix, fn: ev.fn, depth: depth})
				delete(pending, i)
			}
		case '}':
			for len(stack) > 0 && stack[len(stack)-1].depth >= depth {
				stack = stack[:len(stack)-1]
			}
			depth--
		}
	}
}

// jvmKtorRoutes extracts Ktor routing DSL endpoints with nested route() prefixes; prefixes of call sites of
// `fun Route.x()` extensions apply to the endpoints declared in them (across files).
func jvmKtorRoutes(files []*jvmFile) []model.Item {
	type pending struct {
		item   model.Item
		prefix string
		fn     string
	}
	var eps []pending
	callPrefix := map[string]struct{ prefix, fn string }{}
	for _, jf := range files {
		if jf.lang() != "kotlin" || (!jf.hasImport("io.ktor.server") && !jf.hasImport("io.ktor.routing")) {
			continue
		}
		var evs []jvmEvent
		for _, m := range reKtorExtFun.FindAllStringSubmatchIndex(jf.code, -1) {
			name := jf.code[m[2]:m[3]]
			evs = append(evs, jvmEvent{off: m[0], brace: m[1] - 1, scope: true, fn: name})
		}
		for _, m := range reKtorCall.FindAllStringSubmatchIndex(jf.code, -1) {
			verb, off := jf.code[m[2]:m[3]], m[2]
			p := ""
			if m[4] >= 0 {
				p = strings.ReplaceAll(jf.lit(m[4], m[5]), "?}", "}") // optional parameter {id?}
			}
			if verb == "route" {
				if m[4] >= 0 {
					evs = append(evs, jvmEvent{off: off, brace: m[1] - 1, scope: true, prefix: p})
				}
				continue
			}
			evs = append(evs, jvmEvent{off: off, method: strings.ToUpper(verb), path: p})
		}
		for _, m := range reKtorCallFn.FindAllStringSubmatchIndex(jf.code, -1) {
			evs = append(evs, jvmEvent{off: m[2], call: true, fn: jf.code[m[2]:m[3]]})
		}
		jvmWalkScopes(jf.code, evs, func(ev jvmEvent, prefix, fn string) {
			if ev.call {
				if _, seen := callPrefix[ev.fn]; !seen && (prefix != "" || fn != "") {
					callPrefix[ev.fn] = struct{ prefix, fn string }{prefix, fn}
				}
				return
			}
			it := jf.item(model.KindEndpoint, "", "", "ktor", "", ev.off, braceBlock(jf.f.src, jf.line(ev.off)))
			it.Meta = map[string]any{"method": ev.method, "path": ev.path}
			eps = append(eps, pending{item: it, prefix: route.Join(prefix, ev.path), fn: fn})
		})
	}
	var items []model.Item
	for _, e := range eps {
		p, fn := e.prefix, e.fn
		for depth := 0; fn != "" && depth < 8; depth++ {
			c, ok := callPrefix[fn]
			if !ok {
				break
			}
			p, fn = route.Join(c.prefix, p), c.fn
		}
		e.item.Key = route.Key(e.item.Meta["method"].(string), p)
		e.item.Handler = e.fn
		e.item.Meta = nil
		items = append(items, e.item)
	}
	return items
}

// functionalRoutes extracts Spring WebFlux / WebMvc.fn router functions: route(GET("/x"), h::x), RouterFunctions
// builders and the Kotlin router/coRouter DSL with "/prefix".nest { } scopes.
func (jf *jvmFile) functionalRoutes() []model.Item {
	reactive := jf.hasImport("org.springframework.web.reactive.function.server")
	if !reactive && !jf.hasImport("org.springframework.web.servlet.function") {
		return nil
	}
	fw := "spring-webflux"
	if !reactive {
		fw = "spring"
	}
	var evs []jvmEvent
	for _, m := range reJvmFnNest.FindAllStringSubmatchIndex(jf.code, -1) {
		s, e := m[2], m[3]
		if s < 0 {
			s, e = m[4], m[5]
		}
		evs = append(evs, jvmEvent{off: m[0], brace: m[1] - 1, scope: true, prefix: jf.lit(s, e)})
	}
	for _, m := range reJvmFnRoute.FindAllStringSubmatchIndex(jf.code, -1) {
		ev := jvmEvent{off: m[2], method: jf.code[m[2]:m[3]], path: jf.lit(m[4], m[5])}
		if m[6] >= 0 {
			ev.handle = jf.code[m[6]:m[7]]
		}
		evs = append(evs, ev)
	}
	var items []model.Item
	jvmWalkScopes(jf.code, evs, func(ev jvmEvent, prefix, _ string) {
		items = append(items, jf.item(model.KindEndpoint, route.Key(ev.method, route.Join(prefix, ev.path)), "", fw, ev.handle, ev.off,
			braceBlock(jf.f.src, jf.line(ev.off))))
	})
	return items
}

// composeViews extracts Jetpack Compose Navigation destinations, named like the Android SDK names them
// (query dropped, {arg} → :arg, type-safe routes by their class name).
func (jf *jvmFile) composeViews() []model.Item {
	if !jf.hasImport("androidx.navigation") {
		return nil
	}
	var items []model.Item
	for _, m := range reJvmCompose.FindAllStringSubmatchIndex(jf.code, -1) {
		r := ""
		switch {
		case m[2] >= 0:
			r = jvmLast(jf.code[m[2]:m[3]])
		case m[4] >= 0:
			r = jf.lit(m[4], m[5])
		case m[6] >= 0:
			r = jf.consts[jvmLast(jf.code[m[6]:m[7]])]
		}
		if r == "" {
			continue
		}
		items = append(items, jf.item(model.KindView, composeViewName(r), "", "compose-navigation", "", m[0]+1,
			braceBlock(jf.f.src, jf.line(m[0]+1))))
	}
	return items
}

var reJvmBraceParam = regexp.MustCompile(`\{([^}]+)\}`)

func composeViewName(r string) string {
	if i := strings.IndexByte(r, '?'); i >= 0 {
		r = r[:i]
	}
	return reJvmBraceParam.ReplaceAllString(r, ":$1")
}

// jvmAndroidViews extracts Activities and Fragments (directly or through base classes of the repo); abstract
// bases are not screens.
func jvmAndroidViews(files []*jvmFile) []model.Item {
	screen := map[string]bool{}
	for k := range androidBases {
		screen[k] = true
	}
	for changed := true; changed; {
		changed = false
		for _, jf := range files {
			for _, c := range jf.classes {
				if screen[c.name] || c.iface {
					continue
				}
				for _, b := range c.bases {
					if screen[b] {
						screen[c.name], changed = true, true
						break
					}
				}
			}
		}
	}
	var items []model.Item
	for _, jf := range files {
		for _, c := range jf.classes {
			if c.abstract || c.iface || androidBases[c.name] || !screen[c.name] {
				continue
			}
			l := jf.line(c.nameOff)
			snip := linesRange(jf.f.src, l, l)
			if c.bodyOpen >= 0 {
				snip = braceBlock(jf.f.src, l)
			}
			items = append(items, jf.item(model.KindView, c.name, "", "android", c.name, c.nameOff, snip))
		}
	}
	return items
}

// dependencies extracts driver/SDK imports, connection-string literals and HTTP client URL literals.
func (jf *jvmFile) dependencies() []model.Item {
	var items []model.Item
	for _, m := range reJvmImport.FindAllStringSubmatchIndex(jf.code, -1) {
		im := jf.code[m[2]:m[3]]
		for _, d := range jvmDeps {
			if im == d.prefix || strings.HasPrefix(im, d.prefix+".") {
				items = append(items, depItem(d.kind, d.name, jf.f.rel, jf.line(m[2]), jf.lang()))
				break
			}
		}
	}
	lineOf := func(off int) string {
		l := jf.line(off)
		e := len(jf.code)
		if l < len(jf.starts) {
			e = jf.starts[l] - 1
		}
		return jf.code[jf.starts[l-1]:e]
	}
	for _, m := range reJvmStrLit.FindAllStringIndex(jf.code, -1) {
		v := jf.lit(m[0], m[1])
		ln := jf.line(m[0])
		switch {
		case strings.HasPrefix(v, "jdbc:postgresql:"), strings.HasPrefix(v, "r2dbc:postgresql:"):
			items = append(items, depItem("db", "postgres", jf.f.rel, ln, jf.lang()))
		case strings.HasPrefix(v, "jdbc:mysql:"), strings.HasPrefix(v, "jdbc:mariadb:"):
			items = append(items, depItem("db", "mysql", jf.f.rel, ln, jf.lang()))
		case strings.HasPrefix(v, "mongodb://"), strings.HasPrefix(v, "mongodb+srv://"):
			items = append(items, depItem("db", "mongodb", jf.f.rel, ln, jf.lang()))
		case strings.HasPrefix(v, "redis://"), strings.HasPrefix(v, "rediss://"):
			items = append(items, depItem("cache", "redis", jf.f.rel, ln, jf.lang()))
		case strings.HasPrefix(v, "amqp://"), strings.HasPrefix(v, "amqps://"):
			items = append(items, depItem("queue", "rabbitmq", jf.f.rel, ln, jf.lang()))
		case strings.HasPrefix(v, "http"):
			if h := hostOf(v); h != "" && reJvmHTTPMark.MatchString(lineOf(m[0])) {
				items = append(items, depItem("http", h, jf.f.rel, ln, jf.lang()))
			}
		}
	}
	// URL constants passed to an HTTP client: baseUrl(BASE_URL)
	for i := 1; i <= len(jf.starts); i++ {
		s := jf.starts[i-1]
		e := len(jf.code)
		if i < len(jf.starts) {
			e = jf.starts[i] - 1
		}
		l := jf.code[s:e]
		if !reJvmHTTPMark.MatchString(l) {
			continue
		}
		for _, m := range reJvmUpperId.FindAllStringSubmatch(l, -1) {
			if h := hostOf(jf.consts[m[1]]); h != "" {
				items = append(items, depItem("http", h, jf.f.rel, i, jf.lang()))
			}
		}
	}
	return items
}
