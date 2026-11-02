package extract

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

// C# (.NET) extractor: ASP.NET Core controllers (attribute + conventional routing), minimal APIs (MapGroup chains,
// variables and extension-method mounts across files), gRPC services, MassTransit / NServiceBus / Confluent.Kafka /
// RabbitMQ.Client / Azure Service Bus consumers, queue-reading BackgroundServices, Hangfire / Quartz / Coravel
// schedules and driver dependencies. Source is scanned after cleanCSharp: comments and string contents are blanked
// (offsets preserved), string values are kept aside by offset so attribute and call arguments stay readable.

var (
	reCsType      = regexp.MustCompile(`\b(class|record|struct|interface)\s+(?:(?:class|struct)\s+)?([A-Za-z_]\w*)`)
	reCsConst     = regexp.MustCompile(`\b(?:const|static\s+readonly)\s+string\s+([A-Za-z_]\w*)\s*=`)
	reCsIdentPath = regexp.MustCompile(`^@?[A-Za-z_][\w.]*$`)
	reCsMethodNm  = regexp.MustCompile(`([A-Za-z_]\w*)\s*(?:<[^()]*>)?\s*$`)
	reCsWord      = regexp.MustCompile(`[A-Za-z_]\w*`)
	reCsCall      = regexp.MustCompile(`\.\s*([A-Za-z_]\w*)\s*(?:<[^<>()]*(?:<[^<>()]*>[^<>()]*)*>)?\s*\(`)
	reCsAssign    = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*=`)
	reCsExtParam  = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*\(\s*this\s+[\w.<>?,\s]+?\s([A-Za-z_]\w*)\s*[,)]`)
	reCsGrpcBase  = regexp.MustCompile(`^(?:[\w.]+\.)?([A-Za-z_]\w*)\.([A-Za-z_]\w*)Base$`)
	reCsMsgBase   = regexp.MustCompile(`^(?:[\w.]+\.)?(IConsumer|IJobConsumer|IHandleMessages)\s*<(.+)>$`)
	reCsUsing     = regexp.MustCompile(`(?m)^[ \t]*(?:global\s+)?using\s+(?:static\s+)?(?:\w+\s*=\s*)?([A-Za-z_][\w.]*)\s*;`)
	reCsDepCall   = regexp.MustCompile(`\b(UseNpgsql|AddNpgsqlDataSource|AddNpgsqlDbContext|UseSqlServer|AddSqlServerDbContext|UseMySql|UseMySQL|UseMongoDB|AddStackExchangeRedisCache|ConnectionMultiplexer\s*\.\s*Connect(?:Async)?|UsingRabbitMq|UsingAzureServiceBus|UsingAmazonSqs|UsingKafka)\s*\(`)
	reCsURLCtx    = regexp.MustCompile(`(?:new\s+Uri\s*\(|BaseAddress\s*=|Async\s*(?:<[^<>]*>)?\s*\(|HttpRequestMessage\s*\([^;]*,|(?:Url|Uri|Endpoint)\s*=)\s*[@$]*$`)
	reCsGroupID   = regexp.MustCompile(`\bGroupId\s*=`)
	reCsQueueRead = regexp.MustCompile(`\.\s*(ReceiveMessageAsync|ReceiveMessagesAsync|StreamReadGroupAsync|ListLeftPopAsync|ListRightPopAsync|BasicGet(?:Async)?)\s*\(`)
	reCsQueueName = regexp.MustCompile(`\b(?:QueueUrl|QueueName)\s*=|\.\s*(?:CreateReceiver|GetQueueClient)\s*\(|new\s+QueueClient\s*\(`)
	reCsJobKey    = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*=\s*new\s+JobKey\s*\(`)
	reCsGenericT  = regexp.MustCompile(`\b(?:ScheduleJob|AddJob|Schedule)\s*<\s*([A-Za-z_][\w.]*)\s*>`)
	reCsWhere     = regexp.MustCompile(`\bwhere\b`)
	reCsHole      = regexp.MustCompile(`\{[^{}]+\}`)
	reCsHTTPVerb  = regexp.MustCompile(`HttpMethods?\s*\.\s*(\w+)`)
	reCsLastCall  = regexp.MustCompile(`\.\s*(\w+)\s*\([^=]*$`)
	reCsTypeArg   = regexp.MustCompile(`^\s*<\s*([\w.]+)\s*>`)
	reCsChainCall = regexp.MustCompile(`^\s*\.\s*(\w+)\s*\(`)
	reCsCronHelp  = regexp.MustCompile(`^(?:Hangfire\.)?Cron\.(\w+)(?:\(([^)]*)\))?$`)
)

var csDepNamespaces = []struct{ ns, kind, name string }{
	{"Npgsql", "db", "postgres"}, {"Microsoft.Data.SqlClient", "db", "sqlserver"}, {"System.Data.SqlClient", "db", "sqlserver"},
	{"MySqlConnector", "db", "mysql"}, {"MySql.Data", "db", "mysql"}, {"MongoDB.Driver", "db", "mongodb"},
	{"StackExchange.Redis", "cache", "redis"}, {"Confluent.Kafka", "queue", "kafka"}, {"RabbitMQ.Client", "queue", "rabbitmq"},
	{"Azure.Messaging.ServiceBus", "queue", "azure-servicebus"}, {"Amazon.SQS", "queue", "sqs"}, {"Stripe", "http", "api.stripe.com"},
}

var csDepCalls = map[string][2]string{
	"UseNpgsql": {"db", "postgres"}, "AddNpgsqlDataSource": {"db", "postgres"}, "AddNpgsqlDbContext": {"db", "postgres"},
	"UseSqlServer": {"db", "sqlserver"}, "AddSqlServerDbContext": {"db", "sqlserver"}, "UseMySql": {"db", "mysql"},
	"UseMySQL": {"db", "mysql"}, "UseMongoDB": {"db", "mongodb"}, "AddStackExchangeRedisCache": {"cache", "redis"},
	"ConnectionMultiplexer": {"cache", "redis"}, "UsingRabbitMq": {"queue", "rabbitmq"},
	"UsingAzureServiceBus": {"queue", "azure-servicebus"}, "UsingAmazonSqs": {"queue", "sqs"}, "UsingKafka": {"queue", "kafka"},
}

var csHTTPAttrs = map[string]string{"HttpGet": "GET", "HttpPost": "POST", "HttpPut": "PUT", "HttpDelete": "DELETE",
	"HttpPatch": "PATCH", "HttpHead": "HEAD", "HttpOptions": "OPTIONS"}

var csMapVerbs = map[string]string{"MapGet": "GET", "MapPost": "POST", "MapPut": "PUT", "MapDelete": "DELETE",
	"MapPatch": "PATCH", "Map": "ANY", "MapHealthChecks": "GET"}

var csModifiers = map[string]bool{"public": true, "private": true, "protected": true, "internal": true, "sealed": true,
	"abstract": true, "static": true, "partial": true, "file": true, "readonly": true, "ref": true, "unsafe": true, "new": true}

// Hangfire Cron.* helpers → cron expressions (args fill minute/hour/day).
var csHangfireCron = map[string]string{"Minutely": "* * * * *", "Hourly": "{0} * * * *", "Daily": "{1} {0} * * *",
	"Weekly": "{2} {1} * * {0}", "Monthly": "{2} {1} {0} * *", "Yearly": "{3} {2} {1} {0} *", "Never": "0 0 31 2 *"}

// Coravel interval helpers → cron expressions.
var csCoravel = map[string]string{"EveryMinute": "* * * * *", "EveryFiveMinutes": "*/5 * * * *", "EveryTenMinutes": "*/10 * * * *",
	"EveryFifteenMinutes": "*/15 * * * *", "EveryThirtyMinutes": "*/30 * * * *", "Hourly": "0 * * * *", "Daily": "0 0 * * *",
	"Weekly": "0 0 * * 1", "Monthly": "0 0 1 * *", "EverySecond": "@every 1s", "EveryFiveSeconds": "@every 5s",
	"EveryTenSeconds": "@every 10s", "EveryFifteenSeconds": "@every 15s", "EveryThirtySeconds": "@every 30s"}

type csFile struct {
	f       srcFile
	code    []byte         // comments, preprocessor lines and string/char contents blanked; same length as src
	strs    map[int]string // offset of a literal's opening quote → decoded value
	classes []*csClass
}

type csAttr struct {
	name string // last segment without the Attribute suffix
	args []csArg
}

type csArg struct {
	name string // named argument (Name = x / name: x) or ""
	val  string
	ok   bool // val resolved to a string
	s, e int  // value span in code
}

type csClass struct {
	name                string
	bases               []string
	attrs               []csAttr
	mods                string
	declOff, open, end  int // open/end: body braces (-1 without a body)
	members             []csMember
	membersParsed, abst bool
}

type csMember struct {
	name     string
	decl     string
	isMethod bool
	attrs    []csAttr
	start    int // first attribute or declaration
	declOff  int
	end      int // last byte of the body
}

type csVar struct {
	name     string
	prefixes []string
	from, to int
}

type csRepo struct {
	files        []*csFile
	consts       map[string]string
	classes      map[string]*csClass
	classFile    map[*csClass]*csFile
	mounts       map[string][]string // extension method name → route prefixes of the group it is called on
	conventional bool                // MapDefaultControllerRoute / MapControllerRoute present
}

func extractDotnet(files []srcFile) []model.Item {
	rp := &csRepo{consts: map[string]string{}, classes: map[string]*csClass{}, classFile: map[*csClass]*csFile{}, mounts: map[string][]string{}}
	for _, f := range files {
		code, strs := cleanCSharp(f.src)
		cf := &csFile{f: f, code: code, strs: strs}
		cf.parseClasses()
		for _, c := range cf.classes {
			if _, ok := rp.classes[c.name]; !ok {
				rp.classes[c.name] = c
				rp.classFile[c] = cf
			}
		}
		if bytes.Contains(code, []byte("MapDefaultControllerRoute")) || bytes.Contains(code, []byte("MapControllerRoute")) {
			rp.conventional = true
		}
		rp.files = append(rp.files, cf)
	}
	for range 2 { // consts referencing consts
		for _, cf := range rp.files {
			cf.collectConsts(rp)
		}
	}
	for _, cf := range rp.files {
		for _, c := range cf.classes {
			cf.parseMembers(c, rp)
		}
	}
	for range 3 { // extension-method mounts can nest across files
		for _, cf := range rp.files {
			cf.minimal(rp, false)
		}
	}
	var items []model.Item
	for _, cf := range rp.files {
		for _, c := range cf.classes {
			items = append(items, cf.controller(c, rp)...)
			items = append(items, cf.typeItems(c)...)
		}
		items = append(items, cf.minimal(rp, true)...)
		items = append(items, cf.consumers(rp)...)
		items = append(items, cf.crons(rp)...)
		items = append(items, cf.deps()...)
	}
	return items
}

// ---- cleaning ----

// cleanCSharp blanks comments, preprocessor lines and the contents of string and char literals (newlines kept, so
// offsets and line numbers match src). Each string literal's decoded value is returned under the offset of its
// opening quote: regular, verbatim @"", interpolated $"" (holes kept as text) and raw """…""" strings.
func cleanCSharp(src []byte) ([]byte, map[int]string) {
	n := len(src)
	code := make([]byte, n)
	copy(code, src)
	strs := map[int]string{}
	blank := func(from, to int) {
		for k := max(from, 0); k < to && k < n; k++ {
			if code[k] != '\n' {
				code[k] = ' '
			}
		}
	}
	lineStart := true
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '\n':
			lineStart = true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r':
			i++
			continue
		case c == '#' && lineStart:
			j := i
			for j < n && src[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j
		case c == '/' && i+1 < n && src[i+1] == '/':
			j := i
			for j < n && src[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := n
			if j := bytes.Index(src[i+2:], []byte("*/")); j >= 0 {
				end = i + 2 + j + 2
			}
			blank(i, end)
			i = end
		case c == '\'':
			j := i + 1
			for j < n && src[j] != '\'' && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			end := min(j+1, n)
			blank(i+1, end-1)
			i = end
		case c == '"':
			verbatim, interp := false, false
			for k := i - 1; k >= 0 && (src[k] == '@' || src[k] == '$'); k-- {
				if src[k] == '@' {
					verbatim = true
				} else {
					interp = true
				}
			}
			end, val := csString(src, i, verbatim, interp)
			strs[i] = val
			blank(i+1, end-1)
			i = end
		default:
			i++
		}
		lineStart = false
	}
	return code, strs
}

// csString scans the literal whose opening quote is at i and returns the offset after it and its value.
func csString(src []byte, i int, verbatim, interp bool) (int, string) {
	n := len(src)
	q := 0
	for i+q < n && src[i+q] == '"' {
		q++
	}
	if !verbatim && q >= 3 { // raw string literal
		delim := bytes.Repeat([]byte{'"'}, q)
		j := bytes.Index(src[i+q:], delim)
		if j < 0 {
			return n, string(src[i+q:])
		}
		content := string(src[i+q : i+q+j])
		if strings.Contains(content, "\n") {
			ls := strings.Split(content, "\n")
			indent := ls[len(ls)-1]
			ls = ls[1 : len(ls)-1]
			for k, l := range ls {
				ls[k] = strings.TrimPrefix(strings.TrimRight(l, "\r"), indent)
			}
			content = strings.Join(ls, "\n")
		}
		return i + q + j + q, content
	}
	if !verbatim && q == 2 {
		return i + 2, ""
	}
	var b strings.Builder
	j := i + 1
	for j < n {
		c := src[j]
		if c == '"' {
			if verbatim && j+1 < n && src[j+1] == '"' {
				b.WriteByte('"')
				j += 2
				continue
			}
			break
		}
		if c == '\n' && !verbatim {
			return j, b.String()
		}
		if c == '\\' && !verbatim && j+1 < n {
			switch src[j+1] {
			case '"', '\\', '\'':
				b.WriteByte(src[j+1])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(c)
				b.WriteByte(src[j+1])
			}
			j += 2
			continue
		}
		if interp && c == '{' {
			if j+1 < n && src[j+1] == '{' {
				b.WriteByte('{')
				j += 2
				continue
			}
			// hole: copy up to the matching '}', skipping nested literals
			depth, k := 0, j
			for k < n {
				switch src[k] {
				case '{':
					depth++
				case '}':
					depth--
				case '"':
					k++
					for k < n && src[k] != '"' && src[k] != '\n' {
						if src[k] == '\\' {
							k++
						}
						k++
					}
				}
				if k >= n || depth == 0 || (src[k] == '\n' && !verbatim) {
					break
				}
				k++
			}
			if k >= n {
				return n, b.String()
			}
			b.Write(src[j : k+1])
			j = k + 1
			continue
		}
		if interp && c == '}' && j+1 < n && src[j+1] == '}' {
			b.WriteByte('}')
			j += 2
			continue
		}
		b.WriteByte(c)
		j++
	}
	return min(j+1, n), b.String()
}

// ---- low-level helpers over the cleaned code ----

var csPairs = map[byte]byte{'(': ')', '[': ']', '{': '}', '<': '>'}

// csClose returns the offset of the bracket closing the one at open (len(code) when unbalanced).
func csClose(code []byte, open int) int {
	o := code[open]
	cl := csPairs[o]
	depth := 0
	for k := open; k < len(code); k++ {
		switch code[k] {
		case o:
			depth++
		case cl:
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return len(code)
}

// csOpen returns the offset of the bracket opening the one at close (-1 when unbalanced).
func csOpen(code []byte, close int) int {
	cl := code[close]
	var o byte
	for k, v := range csPairs {
		if v == cl {
			o = k
		}
	}
	depth := 0
	for k := close; k >= 0; k-- {
		switch code[k] {
		case cl:
			depth++
		case o:
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return -1
}

func csSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func csIdent(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// csSplit splits code[s:e] at top-level sep bytes and returns trimmed spans.
func csSplit(code []byte, s, e int, sep byte) [][2]int {
	var out [][2]int
	depth, from := 0, s
	add := func(a, b int) {
		for a < b && csSpace(code[a]) {
			a++
		}
		for b > a && csSpace(code[b-1]) {
			b--
		}
		if a < b {
			out = append(out, [2]int{a, b})
		}
	}
	for k := s; k < e; k++ {
		switch code[k] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case sep:
			if depth == 0 {
				add(from, k)
				from = k + 1
			}
		}
	}
	add(from, e)
	return out
}

// callArgs returns the argument spans of the call whose '(' is at open, and the ')' offset.
func (cf *csFile) callArgs(open int) ([][2]int, int) {
	cl := csClose(cf.code, open)
	if cl >= len(cf.code) {
		return nil, cl
	}
	return csSplit(cf.code, open+1, cl, ','), cl
}

// strAt returns the string literal starting at off (after whitespace and @/$ prefixes).
func (cf *csFile) strAt(off int) (string, bool) {
	for off < len(cf.code) && (csSpace(cf.code[off]) || cf.code[off] == '@' || cf.code[off] == '$') {
		off++
	}
	v, ok := cf.strs[off]
	return v, ok
}

// eval resolves code[s:e] to a string: literals, consts (A.B / B), nameof(x) and '+' concatenations.
func (cf *csFile) eval(rp *csRepo, s, e int) (string, bool) {
	var b strings.Builder
	for _, p := range csSplit(cf.code, s, e, '+') {
		txt := strings.TrimSpace(string(cf.code[p[0]:p[1]]))
		if q := bytes.IndexByte(cf.code[p[0]:p[1]], '"'); q >= 0 && strings.Trim(txt, "@$\" \t\r\n") == "" {
			if v, ok := cf.strs[p[0]+q]; ok {
				if strings.Contains(txt[:strings.IndexByte(txt, '"')], "$") {
					v = cf.holes(rp, v)
				}
				b.WriteString(v)
				continue
			}
		}
		if inner, ok := strings.CutPrefix(txt, "nameof("); ok {
			inner = strings.TrimSpace(strings.TrimSuffix(inner, ")"))
			b.WriteString(inner[strings.LastIndexByte(inner, '.')+1:])
			continue
		}
		if !reCsIdentPath.MatchString(txt) || rp == nil {
			return "", false
		}
		v, ok := rp.lookupConst(strings.TrimPrefix(txt, "@"))
		if !ok {
			return "", false
		}
		b.WriteString(v)
	}
	return b.String(), true
}

// holes resolves interpolation holes of an interpolated literal: {nameof(x)} → x, {Const} → its value; other
// holes stay as "{expr}" (route parameters after normalization).
func (cf *csFile) holes(rp *csRepo, v string) string {
	return reCsHole.ReplaceAllStringFunc(v, func(h string) string {
		e := strings.TrimSpace(h[1 : len(h)-1])
		if inner, ok := strings.CutPrefix(e, "nameof("); ok {
			inner = strings.TrimSpace(strings.TrimSuffix(inner, ")"))
			return inner[strings.LastIndexByte(inner, '.')+1:]
		}
		if rp != nil && reCsIdentPath.MatchString(e) {
			if c, ok := rp.lookupConst(e); ok {
				return c
			}
		}
		return h
	})
}

func (rp *csRepo) lookupConst(name string) (string, bool) {
	name = strings.TrimPrefix(name, "global::")
	for {
		if v, ok := rp.consts[name]; ok {
			return v, true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return "", false
		}
		name = name[i+1:]
	}
}

func (cf *csFile) line(off int) int { return lineAt(cf.f.src, off) }

// lineStart returns the offset of the start of off's line.
func (cf *csFile) lineStart(off int) int {
	return bytes.LastIndexByte(cf.f.src[:min(off, len(cf.f.src))], '\n') + 1
}

// stmtEnd returns the offset of the ';' ending the statement that contains off (depth-aware).
func (cf *csFile) stmtEnd(off int) int {
	depth := 0
	for k := off; k < len(cf.code); k++ {
		switch cf.code[k] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth < 0 {
				return k
			}
		case ';':
			if depth == 0 {
				return k
			}
		}
	}
	return len(cf.code)
}

func (cf *csFile) enclosing(off int) *csClass {
	var best *csClass
	for _, c := range cf.classes {
		if c.open >= 0 && c.open < off && off < c.end && (best == nil || c.open > best.open) {
			best = c
		}
	}
	return best
}

// ---- classes, members, attributes ----

func (cf *csFile) parseClasses() {
	code := cf.code
	for _, m := range reCsType.FindAllSubmatchIndex(code, -1) {
		if !cf.declContext(m[0]) {
			continue
		}
		c := &csClass{name: string(code[m[4]:m[5]]), declOff: m[0], open: -1, end: -1}
		p := m[1]
		skip := func() {
			for p < len(code) && csSpace(code[p]) {
				p++
			}
		}
		skip()
		if p < len(code) && code[p] == '<' {
			p = csClose(code, p) + 1
			skip()
		}
		if p < len(code) && code[p] == '(' {
			p = csClose(code, p) + 1
		}
		q := p
		for q < len(code) && code[q] != '{' && code[q] != ';' {
			q++
		}
		if q >= len(code) {
			continue
		}
		head := string(code[p:q])
		if loc := reCsWhere.FindStringIndex(head); loc != nil {
			head = head[:loc[0]]
		}
		if bl, ok := strings.CutPrefix(strings.TrimSpace(head), ":"); ok {
			hb := []byte(bl)
			for _, sp := range csSplitAngle(hb) {
				c.bases = append(c.bases, strings.Join(strings.Fields(string(hb[sp[0]:sp[1]])), ""))
			}
		}
		if code[q] == '{' {
			c.open, c.end = q, csClose(code, q)
		}
		c.attrs, c.mods = cf.attrsBefore(m[0])
		c.abst = strings.Contains(" "+c.mods+" ", " abstract ") || m[2] >= 0 && string(code[m[2]:m[3]]) == "interface"
		cf.classes = append(cf.classes, c)
	}
}

// csSplitAngle splits a base list at commas outside <> and ().
func csSplitAngle(b []byte) [][2]int {
	var out [][2]int
	depth, from := 0, 0
	for k, c := range b {
		switch c {
		case '<', '(':
			depth++
		case '>', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, [2]int{from, k})
				from = k + 1
			}
		}
	}
	out = append(out, [2]int{from, len(b)})
	return out
}

// declContext reports whether the type keyword at off starts a declaration (not `foreach (var record in …)`).
func (cf *csFile) declContext(off int) bool {
	k := off - 1
	for {
		for k >= 0 && csSpace(cf.code[k]) {
			k--
		}
		if k < 0 {
			return true
		}
		switch cf.code[k] {
		case ';', '{', '}', ']':
			return true
		}
		if !csIdent(cf.code[k]) {
			return false
		}
		e := k + 1
		for k >= 0 && csIdent(cf.code[k]) {
			k--
		}
		if w := string(cf.code[k+1 : e]); !csModifiers[w] && w != "record" {
			return false
		}
	}
}

// attrsBefore collects modifiers and attribute sections preceding a declaration at off.
func (cf *csFile) attrsBefore(off int) ([]csAttr, string) {
	var attrs []csAttr
	var mods []string
	k := off - 1
	for k >= 0 {
		for k >= 0 && csSpace(cf.code[k]) {
			k--
		}
		if k < 0 {
			break
		}
		if cf.code[k] == ']' {
			o := csOpen(cf.code, k)
			if o < 0 {
				break
			}
			attrs = append(cf.parseAttrSection(nil, o, k), attrs...)
			k = o - 1
			continue
		}
		if !csIdent(cf.code[k]) {
			break
		}
		e := k + 1
		for k >= 0 && csIdent(cf.code[k]) {
			k--
		}
		w := string(cf.code[k+1 : e])
		if !csModifiers[w] {
			break
		}
		mods = append(mods, w)
	}
	return attrs, strings.Join(mods, " ")
}

// parseAttrSection parses `[A, B("x", Name = "y")]` at code[l..r]. rp may be nil (consts unresolved).
func (cf *csFile) parseAttrSection(rp *csRepo, l, r int) []csAttr {
	inner := string(cf.code[l+1 : r])
	s := l + 1
	if i := strings.IndexByte(inner, ':'); i > 0 && (i+1 >= len(inner) || inner[i+1] != ':') {
		switch strings.TrimSpace(inner[:i]) {
		case "return", "assembly", "module", "field", "property", "method", "param", "type", "event":
			s += i + 1
		}
	}
	var out []csAttr
	for _, sp := range csSplit(cf.code, s, r, ',') {
		k := sp[0]
		for k < sp[1] && (csIdent(cf.code[k]) || cf.code[k] == '.' || cf.code[k] == ':') {
			k++
		}
		name := string(cf.code[sp[0]:k])
		name = strings.TrimSuffix(name[strings.LastIndexAny(name, ".:")+1:], "Attribute")
		a := csAttr{name: name}
		for k < sp[1] && csSpace(cf.code[k]) {
			k++
		}
		if k < sp[1] && cf.code[k] == '(' {
			args, _ := cf.callArgs(k)
			for _, ar := range args {
				a.args = append(a.args, cf.arg(rp, ar))
			}
		}
		if name != "" {
			out = append(out, a)
		}
	}
	return out
}

// arg parses one argument span: named (`Name = x`, `name: x`) or positional.
func (cf *csFile) arg(rp *csRepo, sp [2]int) csArg {
	a := csArg{s: sp[0], e: sp[1]}
	k := sp[0]
	for k < sp[1] && csIdent(cf.code[k]) {
		k++
	}
	j := k
	for j < sp[1] && csSpace(cf.code[j]) {
		j++
	}
	if k > sp[0] && j < sp[1] && (cf.code[j] == '=' || cf.code[j] == ':') && (j+1 >= sp[1] || (cf.code[j+1] != '=' && cf.code[j+1] != ':' && cf.code[j+1] != '>')) {
		a.name = string(cf.code[sp[0]:k])
		a.s = j + 1
	}
	a.val, a.ok = cf.eval(rp, a.s, a.e)
	return a
}

func (cf *csFile) parseMembers(c *csClass, rp *csRepo) {
	if c.open < 0 || c.membersParsed {
		return
	}
	c.membersParsed = true
	// re-parse class attributes with consts known
	if len(c.attrs) > 0 {
		c.attrs = nil
		k := c.declOff - 1
		for k >= 0 {
			for k >= 0 && csSpace(cf.code[k]) {
				k--
			}
			if k < 0 {
				break
			}
			if cf.code[k] == ']' {
				o := csOpen(cf.code, k)
				if o < 0 {
					break
				}
				c.attrs = append(cf.parseAttrSection(rp, o, k), c.attrs...)
				k = o - 1
				continue
			}
			if !csIdent(cf.code[k]) {
				break
			}
			e := k + 1
			for k >= 0 && csIdent(cf.code[k]) {
				k--
			}
			if !csModifiers[string(cf.code[k+1:e])] {
				break
			}
		}
	}
	code := cf.code
	var attrs []csAttr
	attrStart := -1
	for i := c.open + 1; i < c.end; {
		if csSpace(code[i]) {
			i++
			continue
		}
		if code[i] == '[' {
			r := csClose(code, i)
			if attrStart < 0 {
				attrStart = i
			}
			attrs = append(attrs, cf.parseAttrSection(rp, i, r)...)
			i = r + 1
			continue
		}
		start, depth := i, 0
		for i < c.end {
			ch := code[i]
			if ch == '(' || ch == '[' {
				depth++
			} else if ch == ')' || ch == ']' {
				depth--
			} else if depth == 0 && (ch == '{' || ch == ';' || (ch == '=' && i+1 < len(code) && code[i+1] == '>')) {
				break
			}
			i++
		}
		if i >= c.end {
			break
		}
		decl := strings.Join(strings.Fields(string(code[start:i])), " ")
		end := i
		switch {
		case code[i] == '{':
			end = csClose(code, i)
		case code[i] == '=':
			end = cf.stmtEnd(i + 2)
		}
		m := csMember{decl: decl, attrs: attrs, start: start, declOff: start, end: min(end, len(code)-1)}
		if attrStart >= 0 {
			m.start = attrStart
		}
		if pi := csTopParen(decl); pi > 0 && !strings.Contains(decl[:pi], "=") && !csHasWord(decl, "class", "struct", "interface", "record", "enum", "delegate", "event", "operator") {
			if nm := reCsMethodNm.FindStringSubmatch(decl[:pi]); nm != nil {
				m.name, m.isMethod = nm[1], true
			}
		}
		c.members = append(c.members, m)
		attrs, attrStart = nil, -1
		i = end + 1
	}
}

func csTopParen(decl string) int {
	depth := 0
	for k := 0; k < len(decl); k++ {
		switch decl[k] {
		case '<':
			depth++
		case '>':
			depth--
		case '(':
			if depth <= 0 {
				return k
			}
		}
	}
	return -1
}

func csHasWord(s string, words ...string) bool {
	for _, w := range reCsWord.FindAllString(s, -1) {
		for _, x := range words {
			if w == x {
				return true
			}
		}
	}
	return false
}

func findAttrs(attrs []csAttr, name string) []csAttr {
	var out []csAttr
	for _, a := range attrs {
		if a.name == name {
			out = append(out, a)
		}
	}
	return out
}

// template returns the route template argument of an attribute (first positional or template:/Template =).
func (a csAttr) template() (string, bool) {
	for _, ar := range a.args {
		if ar.ok && (ar.name == "" || strings.EqualFold(ar.name, "template")) {
			return ar.val, true
		}
	}
	return "", false
}

func (a csAttr) named(name string) (string, bool) {
	for _, ar := range a.args {
		if ar.ok && ar.name == name {
			return ar.val, true
		}
	}
	return "", false
}

func (cf *csFile) snippet(from, to int) string {
	from = cf.lineStart(from)
	to = min(to+1, len(cf.f.src))
	if from >= to {
		return ""
	}
	return string(cf.f.src[from:to])
}

// ---- consts ----

func (cf *csFile) collectConsts(rp *csRepo) {
	for _, m := range reCsConst.FindAllSubmatchIndex(cf.code, -1) {
		end := cf.stmtEnd(m[1])
		v, ok := cf.eval(rp, m[1], end)
		if !ok {
			continue
		}
		name := string(cf.code[m[2]:m[3]])
		rp.consts[name] = v
		if c := cf.enclosing(m[0]); c != nil {
			rp.consts[c.name+"."+name] = v
		}
	}
}

// ---- controllers ----

func (rp *csRepo) isController(c *csClass, depth int) bool {
	if c == nil || depth > 5 {
		return false
	}
	if len(findAttrs(c.attrs, "NonController")) > 0 {
		return false
	}
	if strings.HasSuffix(c.name, "Controller") || len(findAttrs(c.attrs, "ApiController")) > 0 || len(findAttrs(c.attrs, "Controller")) > 0 {
		return true
	}
	for _, b := range c.bases {
		b = b[strings.LastIndexByte(b, '.')+1:]
		if b == "Controller" || b == "ControllerBase" || rp.isController(rp.classes[b], depth+1) {
			return true
		}
	}
	return false
}

// classAttrs returns attrs named name on c or, when absent, on its base classes.
func (rp *csRepo) classAttrs(c *csClass, name string, depth int) []csAttr {
	if c == nil || depth > 5 {
		return nil
	}
	if a := findAttrs(c.attrs, name); len(a) > 0 {
		return a
	}
	for _, b := range c.bases {
		if bc := rp.classes[b[strings.LastIndexByte(b, '.')+1:]]; bc != nil && bc != c {
			if cf := rp.classFile[bc]; cf != nil {
				cf.parseMembers(bc, rp)
			}
			if a := rp.classAttrs(bc, name, depth+1); len(a) > 0 {
				return a
			}
		}
	}
	return nil
}

var reCsToken = regexp.MustCompile(`(?i)\[(controller|action|area)\]`)

func csTokens(p, ctrl, action, area string) string {
	return reCsToken.ReplaceAllStringFunc(p, func(t string) string {
		switch strings.ToLower(t) {
		case "[controller]":
			return ctrl
		case "[action]":
			return action
		}
		return area
	})
}

func (cf *csFile) controller(c *csClass, rp *csRepo) []model.Item {
	if c.abst || c.open < 0 || !rp.isController(c, 0) {
		return nil
	}
	var classTmpls []string
	for _, a := range rp.classAttrs(c, "Route", 0) {
		if t, ok := a.template(); ok {
			classTmpls = append(classTmpls, t)
		}
	}
	attrRouted := len(classTmpls) > 0
	if !attrRouted {
		classTmpls = []string{""}
	}
	area := ""
	if a := rp.classAttrs(c, "Area", 0); len(a) > 0 {
		area, _ = a[0].template()
	}
	isAPI := len(rp.classAttrs(c, "ApiController", 0)) > 0
	ctrl := strings.TrimSuffix(c.name, "Controller")
	var items []model.Item
	for _, m := range c.members {
		if !m.isMethod || m.name == c.name || !csHasWord(m.decl, "public") || csHasWord(m.decl, "static", "override") || len(findAttrs(m.attrs, "NonAction")) > 0 {
			continue
		}
		action := strings.TrimSuffix(m.name, "Async")
		if a := findAttrs(m.attrs, "ActionName"); len(a) > 0 {
			if v, ok := a[0].template(); ok {
				action = v
			}
		}
		type vr struct{ verb, tmpl string }
		var routes []vr
		var verbs, tmpls []string
		for _, a := range m.attrs {
			if verb, ok := csHTTPAttrs[a.name]; ok {
				if t, ok := a.template(); ok {
					routes = append(routes, vr{verb, t})
				} else {
					verbs = append(verbs, verb)
				}
				continue
			}
			switch a.name {
			case "Route":
				if t, ok := a.template(); ok {
					tmpls = append(tmpls, t)
				}
			case "AcceptVerbs":
				var vs []string
				for _, ar := range a.args {
					if ar.ok && ar.name == "" {
						vs = append(vs, strings.ToUpper(ar.val))
					}
				}
				if t, ok := a.named("Route"); ok {
					for _, v := range vs {
						routes = append(routes, vr{v, t})
					}
				} else {
					verbs = append(verbs, vs...)
				}
			}
		}
		vs := verbs
		if len(vs) == 0 {
			vs = []string{"ANY"}
		}
		for _, t := range tmpls {
			for _, v := range vs {
				routes = append(routes, vr{v, t})
			}
		}
		conventional := false
		if len(routes) == 0 {
			switch {
			case attrRouted && len(verbs) > 0:
				for _, v := range verbs {
					routes = append(routes, vr{v, ""})
				}
			case !attrRouted && !isAPI && rp.conventional:
				conventional = true
				for _, v := range vs {
					routes = append(routes, vr{v, "/[controller]/[action]"})
				}
			default:
				continue
			}
		}
		snip := cf.snippet(m.start, m.end)
		for _, r := range routes {
			var paths []string
			if strings.HasPrefix(r.tmpl, "/") || strings.HasPrefix(r.tmpl, "~/") {
				paths = []string{strings.TrimPrefix(r.tmpl, "~")}
			} else {
				for _, ct := range classTmpls {
					paths = append(paths, route.Join("/"+strings.TrimPrefix(ct, "/"), r.tmpl))
				}
			}
			for _, p := range paths {
				it := model.Item{Kind: model.KindEndpoint, Key: route.Key(r.verb, csTokens(p, ctrl, action, area)), File: cf.f.rel,
					Line: cf.line(m.declOff), Framework: "aspnetcore", Handler: c.name + "." + m.name, Language: "csharp", Snippet: snip}
				if conventional {
					it.Meta = map[string]any{"routing": "conventional"}
				}
				items = append(items, it)
			}
		}
	}
	return items
}

// ---- gRPC services, message consumers (by base type) ----

var csGrpcSkip = map[string]bool{"ToString": true, "Equals": true, "GetHashCode": true, "Dispose": true, "DisposeAsync": true}

func (cf *csFile) typeItems(c *csClass) []model.Item {
	if c.abst || c.open < 0 {
		return nil
	}
	var items []model.Item
	for _, b := range c.bases {
		if m := reCsGrpcBase.FindStringSubmatch(b); m != nil && m[1] == m[2] {
			for _, mb := range c.members {
				if !mb.isMethod || !csHasWord(mb.decl, "override") || csGrpcSkip[mb.name] {
					continue
				}
				items = append(items, model.Item{Kind: model.KindEndpoint, Key: "GRPC /" + m[1] + "/" + mb.name, File: cf.f.rel,
					Line: cf.line(mb.declOff), Framework: "grpc-dotnet", Handler: c.name + "." + mb.name, Language: "csharp",
					Snippet: cf.snippet(mb.start, mb.end)})
			}
			continue
		}
		if m := reCsMsgBase.FindStringSubmatch(b); m != nil {
			msg := m[2]
			if inner, ok := strings.CutPrefix(msg, "Batch<"); ok {
				msg = strings.TrimSuffix(inner, ">")
			}
			if i := strings.IndexByte(msg, '<'); i >= 0 {
				msg = msg[:i]
			}
			msg = msg[strings.LastIndexByte(msg, '.')+1:]
			fw, method := "masstransit", "Consume"
			if m[1] == "IHandleMessages" {
				fw, method = "nservicebus", "Handle"
			}
			snip := ""
			for _, mb := range c.members {
				if mb.isMethod && mb.name == method && strings.Contains(mb.decl, msg) {
					snip = cf.snippet(mb.start, mb.end)
					break
				}
			}
			if snip == "" {
				snip = cf.snippet(c.declOff, c.end)
			}
			items = append(items, model.Item{Kind: model.KindConsumer, Key: c.name, Source: msg, File: cf.f.rel, Line: cf.line(c.declOff),
				Framework: fw, Handler: c.name + "." + method, Language: "csharp", Snippet: snip})
		}
	}
	return items
}

// ---- minimal APIs ----

var csMapSkip = map[string]bool{"MapGroup": true, "MapMethods": true, "MapControllers": true, "MapControllerRoute": true,
	"MapDefaultControllerRoute": true, "MapAreaControllerRoute": true, "MapRazorPages": true, "MapHub": true, "MapGrpcService": true,
	"MapFallback": true, "MapFallbackToFile": true, "MapFallbackToPage": true, "MapFallbackToController": true, "MapBlazorHub": true,
	"MapWhen": true, "MapOpenApi": true, "MapSwagger": true, "MapStaticAssets": true, "MapRazorComponents": true}

// minimal scans minimal-API registrations. With emit=false it only records extension-method mounts.
func (cf *csFile) minimal(rp *csRepo, emit bool) []model.Item {
	code := cf.code
	if !bytes.Contains(code, []byte(".Map")) && !bytes.Contains(code, []byte("this ")) {
		return nil
	}
	var vars []csVar
	// extension methods called on a group elsewhere: `static … MapOrders(this RouteGroupBuilder group)`
	for _, m := range reCsExtParam.FindAllSubmatchIndex(code, -1) {
		pre, ok := rp.mounts[string(code[m[2]:m[3]])]
		if !ok {
			continue
		}
		open := m[2] + bytes.IndexByte(code[m[2]:], '(')
		p := csClose(code, open) + 1
		for p < len(code) && code[p] != '{' && code[p] != ';' && !(code[p] == '=' && p+1 < len(code) && code[p+1] == '>') {
			p++
		}
		to := len(code)
		if p < len(code) && code[p] == '{' {
			to = csClose(code, p)
		} else if p < len(code) {
			to = cf.stmtEnd(p)
		}
		vars = append(vars, csVar{name: string(code[m[4]:m[5]]), prefixes: pre, from: m[0], to: to})
	}
	// group variables: `var api = app.MapGroup("/api").WithTags("x");`
	for _, m := range reCsAssign.FindAllSubmatchIndex(code, -1) {
		if m[1] < len(code) && (code[m[1]] == '=' || code[m[1]] == '>') {
			continue
		}
		end := cf.stmtEnd(m[1])
		if end >= len(code) || !bytes.Contains(code[m[1]:end], []byte("MapGroup")) {
			continue
		}
		pre, found := cf.prefixOf(rp, vars, end)
		if found {
			vars = append(vars, csVar{name: string(code[m[2]:m[3]]), prefixes: pre, from: end, to: len(code)})
		}
	}
	var items []model.Item
	for _, m := range reCsCall.FindAllSubmatchIndex(code, -1) {
		name := string(code[m[2]:m[3]])
		open := m[1] - 1
		verb, isVerb := csMapVerbs[name]
		switch {
		case isVerb || name == "MapMethods":
			if !emit {
				continue
			}
		case strings.HasPrefix(name, "Map") && csMapSkip[name]:
			continue
		default: // a mount: extension method called on a known group
			if pre, found := cf.prefixOf(rp, vars, m[0]); found {
				rp.mounts[name] = csMerge(rp.mounts[name], pre)
			}
			continue
		}
		args, cl := cf.callArgs(open)
		if len(args) == 0 || (name != "MapHealthChecks" && len(args) < 2) {
			continue
		}
		p, ok := cf.eval(rp, args[0][0], args[0][1])
		if !ok {
			continue
		}
		verbs := []string{verb}
		if name == "MapMethods" {
			verbs = nil
			for k := args[1][0]; k < args[1][1]; k++ {
				if v, ok := cf.strs[k]; ok {
					verbs = append(verbs, strings.ToUpper(v))
				}
			}
			for _, w := range reCsHTTPVerb.FindAllSubmatch(code[args[1][0]:args[1][1]], -1) {
				verbs = append(verbs, strings.ToUpper(string(w[1])))
			}
			if len(verbs) == 0 {
				verbs = []string{"ANY"}
			}
		}
		prefixes, found := cf.prefixOf(rp, vars, m[0])
		if !found {
			prefixes = []string{""}
		}
		handler, snip := "", ""
		if len(args) >= 2 {
			h := args[len(args)-1]
			txt := strings.TrimSpace(string(code[h[0]:h[1]]))
			if reCsIdentPath.MatchString(txt) {
				handler = txt[strings.LastIndexByte(txt, '.')+1:]
				snip = rp.methodSnippet(handler, cf)
			}
		}
		if snip == "" {
			snip = cf.snippet(m[0], cl)
		}
		for _, pre := range prefixes {
			for _, v := range verbs {
				items = append(items, model.Item{Kind: model.KindEndpoint, Key: route.Key(v, route.Join(pre, p)), File: cf.f.rel,
					Line: cf.line(m[2]), Framework: "aspnetcore-minimal", Handler: handler, Language: "csharp", Snippet: snip})
			}
		}
	}
	return items
}

func csMerge(a, b []string) []string {
	for _, x := range b {
		dup := false
		for _, y := range a {
			dup = dup || x == y
		}
		if !dup {
			a = append(a, x)
		}
	}
	sort.Strings(a)
	return a
}

// prefixOf resolves the route group prefixes of the receiver expression ending right before off (a '.' or the end
// of an expression): MapGroup chains, other chained calls (WithTags, RequireAuthorization) and group variables.
func (cf *csFile) prefixOf(rp *csRepo, vars []csVar, off int) ([]string, bool) {
	code := cf.code
	for range 50 {
		k := off - 1
		for k >= 0 && csSpace(code[k]) {
			k--
		}
		if k < 0 {
			return nil, false
		}
		if code[k] == ')' {
			o := csOpen(code, k)
			if o < 0 {
				return nil, false
			}
			n := o - 1
			for n >= 0 && csSpace(code[n]) {
				n--
			}
			if n > 0 && code[n] == '>' && code[n-1] != '=' { // generic call
				if g := csOpen(code, n); g >= 0 {
					n = g - 1
				}
			}
			e := n + 1
			for n >= 0 && csIdent(code[n]) {
				n--
			}
			name := string(code[n+1 : e])
			d := n
			for d >= 0 && csSpace(code[d]) {
				d--
			}
			hasRecv := d >= 0 && code[d] == '.'
			if name == "MapGroup" {
				args, _ := cf.callArgs(o)
				sub := ""
				if len(args) > 0 {
					sub, _ = cf.eval(rp, args[0][0], args[0][1])
				}
				base := []string{""}
				if hasRecv {
					if b, ok := cf.prefixOf(rp, vars, d); ok {
						base = b
					}
				}
				out := make([]string, 0, len(base))
				for _, b := range base {
					out = append(out, route.Join(b, sub))
				}
				return out, true
			}
			if !hasRecv || name == "" {
				return nil, false
			}
			off = d
			continue
		}
		if !csIdent(code[k]) {
			return nil, false
		}
		e := k + 1
		for k >= 0 && csIdent(code[k]) {
			k--
		}
		name := string(code[k+1 : e])
		var best *csVar
		for i := range vars {
			v := &vars[i]
			if v.name == name && v.from <= off && off <= v.to && (best == nil || v.from > best.from) {
				best = v
			}
		}
		if best == nil {
			return nil, false
		}
		return best.prefixes, true
	}
	return nil, false
}

// methodSnippet finds a method named name (same file first) and returns its source.
func (rp *csRepo) methodSnippet(name string, prefer *csFile) string {
	files := append([]*csFile{prefer}, rp.files...)
	for _, cf := range files {
		for _, c := range cf.classes {
			cf.parseMembers(c, rp)
			for _, m := range c.members {
				if m.isMethod && m.name == name {
					return cf.snippet(m.start, m.end)
				}
			}
		}
	}
	// top-level statements: local functions (`static IResult GetProduct(int id) => …`)
	re := regexp.MustCompile(`[\w>?\]]\s+` + regexp.QuoteMeta(name) + `\s*(?:<[^<>()]*>)?\s*\(`)
	for _, cf := range files {
		for _, m := range re.FindAllIndex(cf.code, -1) {
			if cf.enclosing(m[0]) != nil {
				continue
			}
			w := m[0]
			for w > 0 && csIdent(cf.code[w-1]) {
				w--
			}
			if kw := string(cf.code[w : m[0]+1]); kw == "new" || kw == "return" || kw == "await" || kw == "else" {
				continue
			}
			p := csClose(cf.code, m[1]-1) + 1
			for p < len(cf.code) && csSpace(cf.code[p]) {
				p++
			}
			switch {
			case p < len(cf.code) && cf.code[p] == '{':
				return cf.snippet(m[0], csClose(cf.code, p))
			case p+1 < len(cf.code) && cf.code[p] == '=' && cf.code[p+1] == '>':
				return cf.snippet(m[0], cf.stmtEnd(p+2))
			}
		}
	}
	return ""
}

// ---- consumers ----

func (cf *csFile) consumers(rp *csRepo) []model.Item {
	code := cf.code
	var items []model.Item
	consumerClass := map[*csClass]bool{}
	add := func(it model.Item, off int) {
		c := cf.enclosing(off)
		if c != nil {
			consumerClass[c] = true
			if it.Handler == "" {
				it.Handler = c.name
			}
			if it.Key == "" {
				it.Key = c.name
			}
		}
		if it.Key == "" {
			it.Key = it.Source
		}
		if it.Key == "" {
			return
		}
		it.Kind, it.File, it.Line, it.Language = model.KindConsumer, cf.f.rel, cf.line(off), "csharp"
		if c != nil {
			it.Snippet = cf.snippet(c.declOff, c.end)
		} else {
			it.Snippet = braceBlock(cf.f.src, it.Line)
		}
		items = append(items, it)
	}
	kafka := bytes.Contains(code, []byte("ConsumerBuilder")) || bytes.Contains(code, []byte("ConsumerConfig")) ||
		bytes.Contains(code, []byte("Confluent.Kafka"))
	for _, m := range reCsCall.FindAllSubmatchIndex(code, -1) {
		name := string(code[m[2]:m[3]])
		args, _ := cf.callArgs(m[1] - 1)
		switch name {
		case "Subscribe":
			if !kafka || len(args) == 0 {
				continue
			}
			topics := cf.strings(rp, args[0])
			group := cf.nearestValue(rp, reCsGroupID, m[0])
			if len(topics) == 0 {
				topics = []string{""}
			}
			for _, t := range topics {
				add(model.Item{Key: group, Source: t, Framework: "confluent-kafka"}, m[0])
			}
		case "BasicConsume", "BasicConsumeAsync":
			q := ""
			for _, a := range args {
				if ar := cf.arg(rp, a); ar.ok && (ar.name == "queue" || ar.name == "") {
					q = ar.val
					break
				}
			}
			if q == "" {
				q = cf.firstCallString(rp, "QueueDeclare")
			}
			if q != "" {
				add(model.Item{Key: q, Source: q, Framework: "rabbitmq-client"}, m[0])
			}
		case "CreateProcessor", "CreateSessionProcessor":
			var vals []string
			for _, a := range args {
				if ar := cf.arg(rp, a); ar.ok && ar.name != "options" {
					vals = append(vals, ar.val)
				}
			}
			switch {
			case len(vals) >= 2:
				add(model.Item{Key: vals[1], Source: vals[0], Framework: "azure-servicebus"}, m[0])
			case len(vals) == 1:
				add(model.Item{Key: vals[0], Source: vals[0], Framework: "azure-servicebus"}, m[0])
			}
		}
	}
	// hosted services reading a queue through other clients (SQS, storage queues, Redis streams/lists)
	for _, c := range cf.classes {
		if c.open < 0 || consumerClass[c] || !csHasBase(c.bases, "BackgroundService") {
			continue
		}
		loc := reCsQueueRead.FindSubmatchIndex(code[c.open:c.end])
		if loc == nil {
			continue
		}
		src := ""
		if args, _ := cf.callArgs(c.open + loc[1] - 1); len(args) > 0 {
			if v, ok := cf.firstStringArg(rp, args); ok {
				src = v
			}
		}
		if src == "" {
			src = cf.nearestValue(rp, reCsQueueName, c.open+loc[0])
		}
		if i := strings.LastIndexByte(src, '/'); i >= 0 && strings.HasPrefix(src, "http") {
			src = src[i+1:]
		}
		items = append(items, model.Item{Kind: model.KindConsumer, Key: c.name, Source: src, File: cf.f.rel, Line: cf.line(c.declOff),
			Framework: "hosted-service", Handler: c.name + ".ExecuteAsync", Language: "csharp", Snippet: cf.snippet(c.declOff, c.end)})
	}
	return items
}

func csHasBase(bases []string, name string) bool {
	for _, b := range bases {
		if b == name || strings.HasSuffix(b, "."+name) {
			return true
		}
	}
	return false
}

// strings returns the string values of an argument: a literal/const, or every literal of a collection.
func (cf *csFile) strings(rp *csRepo, sp [2]int) []string {
	if v, ok := cf.eval(rp, sp[0], sp[1]); ok {
		return []string{v}
	}
	var out []string
	for k := sp[0]; k < sp[1]; k++ {
		if v, ok := cf.strs[k]; ok {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		for _, w := range reCsWord.FindAllIndex(cf.code[sp[0]:sp[1]], -1) {
			if v, ok := rp.lookupConst(string(cf.code[sp[0]+w[0] : sp[0]+w[1]])); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

func (cf *csFile) firstStringArg(rp *csRepo, args [][2]int) (string, bool) {
	for _, a := range args {
		if ar := cf.arg(rp, a); ar.ok {
			return ar.val, true
		}
	}
	return "", false
}

// nearestValue returns the string assigned/passed after the last match of re before off (else the first after).
func (cf *csFile) nearestValue(rp *csRepo, re *regexp.Regexp, off int) string {
	best, after := "", ""
	for _, m := range re.FindAllIndex(cf.code, -1) {
		var v string
		var ok bool
		if cf.code[m[1]-1] == '(' {
			if args, _ := cf.callArgs(m[1] - 1); len(args) > 0 {
				v, ok = cf.eval(rp, args[len(args)-1][0], args[len(args)-1][1])
				if !ok {
					v, ok = cf.firstStringArg(rp, args)
				}
			}
		} else {
			end := m[1]
			for end < len(cf.code) && cf.code[end] != ',' && cf.code[end] != ';' && cf.code[end] != '}' && cf.code[end] != '\n' {
				end++
			}
			v, ok = cf.eval(rp, m[1], end)
		}
		if !ok {
			continue
		}
		if m[0] < off {
			best = v
		} else if after == "" {
			after = v
		}
	}
	if best != "" {
		return best
	}
	return after
}

func (cf *csFile) firstCallString(rp *csRepo, name string) string {
	for _, m := range reCsCall.FindAllSubmatchIndex(cf.code, -1) {
		if n := string(cf.code[m[2]:m[3]]); n != name && n != name+"Async" {
			continue
		}
		args, _ := cf.callArgs(m[1] - 1)
		for _, a := range args {
			if ar := cf.arg(rp, a); ar.ok && (ar.name == "" || ar.name == "queue") {
				return ar.val
			}
		}
	}
	return ""
}

// ---- crons ----

func (cf *csFile) crons(rp *csRepo) []model.Item {
	code := cf.code
	hangfire := bytes.Contains(code, []byte("RecurringJob")) || bytes.Contains(code, []byte("Hangfire"))
	coravel := bytes.Contains(code, []byte("Coravel")) || bytes.Contains(code, []byte("IScheduler"))
	var items []model.Item
	add := func(key, sched, fw string, off, cl int) {
		if key == "" {
			return
		}
		items = append(items, model.Item{Kind: model.KindCron, Key: key, Source: sched, File: cf.f.rel, Line: cf.line(off),
			Framework: fw, Language: "csharp", Snippet: cf.snippet(off, cl)})
	}
	for _, m := range reCsCall.FindAllSubmatchIndex(code, -1) {
		name := string(code[m[2]:m[3]])
		open := m[1] - 1
		switch {
		case name == "AddOrUpdate" && hangfire:
			args, cl := cf.callArgs(open)
			lam := -1
			for i, a := range args {
				if bytes.Contains(code[a[0]:a[1]], []byte("=>")) {
					lam = i
					break
				}
			}
			if lam < 0 || lam+1 >= len(args) {
				continue
			}
			id := ""
			if lam > 0 {
				id, _ = cf.eval(rp, args[0][0], args[0][1])
			}
			if id == "" { // Hangfire's default id: Type.Method
				l := args[lam]
				if cm := reCsLastCall.FindSubmatch(code[l[0]:l[1]]); cm != nil {
					id = string(cm[1])
				}
				if g := reCsTypeArg.FindSubmatch(code[m[3]:open]); g != nil {
					id = string(g[1]) + "." + id
				}
			}
			add(id, cf.hangfireCron(rp, args[lam+1]), "hangfire", m[0], cl)
		case name == "WithCronSchedule":
			args, cl := cf.callArgs(open)
			if len(args) == 0 {
				continue
			}
			sched, _ := cf.eval(rp, args[0][0], args[0][1])
			add(cf.quartzKey(rp, m[0]), sched, "quartz", m[0], cl)
		case name == "Schedule" && coravel:
			_, cl := cf.callArgs(open)
			key := ""
			if g := reCsTypeArg.FindSubmatch(code[m[3]:open]); g != nil {
				key = string(g[1])
			}
			sched := ""
			// follow the chain: .Cron("…") / .EveryFiveMinutes() / .DailyAt(h, m)
			p := cl + 1
			for p < len(code) && sched == "" {
				cm := reCsChainCall.FindSubmatchIndex(code[p:])
				if cm == nil {
					break
				}
				method := string(code[p+cm[2] : p+cm[3]])
				args, c2 := cf.callArgs(p + cm[1] - 1)
				switch {
				case method == "Cron" && len(args) > 0:
					sched, _ = cf.eval(rp, args[0][0], args[0][1])
				case csCoravel[method] != "":
					sched = csCoravel[method]
				case method == "DailyAt" && len(args) == 2:
					sched = strings.TrimSpace(string(code[args[1][0]:args[1][1]])) + " " + strings.TrimSpace(string(code[args[0][0]:args[0][1]])) + " * * *"
				case method == "HourlyAt" && len(args) == 1:
					sched = strings.TrimSpace(string(code[args[0][0]:args[0][1]])) + " * * * *"
				}
				p = c2 + 1
			}
			if key == "" {
				if c := cf.enclosing(m[0]); c != nil {
					key = c.name + "@" + csItoa(cf.line(m[0]))
				}
			}
			if sched != "" {
				add(key, sched, "coravel", m[0], p)
			}
		}
	}
	return items
}

func csItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// hangfireCron turns a schedule argument into a cron expression: literal, const, Cron.Daily / Cron.Daily(3, 30).
func (cf *csFile) hangfireCron(rp *csRepo, sp [2]int) string {
	if v, ok := cf.eval(rp, sp[0], sp[1]); ok {
		return v
	}
	txt := strings.Join(strings.Fields(string(cf.code[sp[0]:sp[1]])), "")
	m := reCsCronHelp.FindStringSubmatch(txt)
	if m == nil {
		return txt
	}
	if m[1] == "MinuteInterval" {
		return "*/" + m[2] + " * * * *"
	}
	tmpl, ok := csHangfireCron[m[1]]
	if !ok {
		return txt
	}
	var args []string
	if m[2] != "" {
		args = strings.Split(m[2], ",")
	}
	// defaults: minute/hour 0, day 1; Weekly's first arg is a DayOfWeek
	for i := range 4 {
		v := "0"
		if (m[1] == "Monthly" && i == 0) || (m[1] == "Yearly" && i <= 1) {
			v = "1"
		}
		if i < len(args) {
			v = strings.TrimSpace(args[i])
			if d, ok := csDayOfWeek[strings.TrimPrefix(v, "DayOfWeek.")]; ok {
				v = d
			}
		}
		tmpl = strings.ReplaceAll(tmpl, "{"+csItoa(i)+"}", v)
	}
	if m[1] == "Weekly" && len(args) == 0 {
		tmpl = strings.Replace(tmpl, "* * 0", "* * 1", 1) // Hangfire's Weekly() defaults to Monday
	}
	return tmpl
}

var csDayOfWeek = map[string]string{"Sunday": "0", "Monday": "1", "Tuesday": "2", "Wednesday": "3", "Thursday": "4", "Friday": "5", "Saturday": "6"}

// quartzKey names the job of a WithCronSchedule call: ForJob("x") / ForJob(jobKey) / ScheduleJob<T> / WithIdentity("x").
func (cf *csFile) quartzKey(rp *csRepo, off int) string {
	code := cf.code
	from := bytes.LastIndexByte(code[:off], ';') + 1
	to := cf.stmtEnd(off)
	if to < off {
		to = off
	}
	region := code[from:to]
	jobKeys := map[string]string{}
	for _, m := range reCsJobKey.FindAllSubmatchIndex(code, -1) {
		if args, _ := cf.callArgs(m[1] - 1); len(args) > 0 {
			if v, ok := cf.eval(rp, args[0][0], args[0][1]); ok {
				jobKeys[string(code[m[2]:m[3]])] = v
			}
		}
	}
	var identity string
	for _, m := range reCsCall.FindAllSubmatchIndex(region, -1) {
		name := string(region[m[2]:m[3]])
		args, _ := cf.callArgs(from + m[1] - 1)
		if len(args) == 0 {
			continue
		}
		v, ok := cf.eval(rp, args[0][0], args[0][1])
		if !ok {
			v, ok = jobKeys[strings.TrimSpace(string(code[args[0][0]:args[0][1]]))]
		}
		switch {
		case name == "ForJob" && ok:
			return v
		case name == "WithIdentity" && ok && identity == "":
			identity = v
		}
	}
	if g := reCsGenericT.FindSubmatch(region); g != nil {
		return string(g[1])
	}
	return identity
}

// ---- dependencies ----

func (cf *csFile) deps() []model.Item {
	code := cf.code
	var items []model.Item
	for _, m := range reCsUsing.FindAllSubmatchIndex(code, -1) {
		ns := string(code[m[2]:m[3]])
		for _, d := range csDepNamespaces {
			if ns == d.ns || strings.HasPrefix(ns, d.ns+".") {
				items = append(items, depItem(d.kind, d.name, cf.f.rel, cf.line(m[2]), "csharp"))
				break
			}
		}
	}
	for _, m := range reCsDepCall.FindAllSubmatchIndex(code, -1) {
		name := string(code[m[2]:m[3]])
		if i := strings.IndexAny(name, " \t\n."); i >= 0 {
			name = name[:i]
		}
		if d, ok := csDepCalls[name]; ok {
			items = append(items, depItem(d[0], d[1], cf.f.rel, cf.line(m[2]), "csharp"))
		}
	}
	offs := make([]int, 0, len(cf.strs))
	for off, v := range cf.strs {
		if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			offs = append(offs, off)
		}
	}
	sort.Ints(offs)
	for _, off := range offs {
		host := hostOf(cf.strs[off])
		if host == "" {
			continue
		}
		before := code[max(0, off-200):off]
		if i := bytes.LastIndexByte(before, ';'); i >= 0 {
			before = before[i+1:]
		}
		switch {
		case strings.HasPrefix(host, "sqs.") && strings.HasSuffix(host, ".amazonaws.com"):
			items = append(items, depItem("queue", "sqs", cf.f.rel, cf.line(off), "csharp"))
		case reCsURLCtx.Match(before):
			items = append(items, depItem("http", host, cf.f.rel, cf.line(off), "csharp"))
		}
	}
	return items
}
