package extract

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

var jsRouteMethods = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE", "del": "DELETE", "patch": "PATCH", "head": "HEAD",
	"options": "OPTIONS", "all": "ANY", "any": "ANY",
}

var nestMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS", "All": "ANY",
}

// Receivers that are HTTP clients or plain objects, never routers.
var jsClientRecv = map[string]bool{
	"axios": true, "http": true, "https": true, "fetch": true, "request": true, "superagent": true, "ky": true, "got": true,
	"client": true, "api": true, "apiClient": true, "$http": true, "httpClient": true, "instance": true, "cache": true,
	"redis": true, "map": true, "params": true, "searchParams": true, "headers": true, "localStorage": true,
	"sessionStorage": true, "store": true, "config": true, "cookies": true, "res": true, "req": true, "ctx": true,
	"c": true, "context": true, "this": true, "Reflect": true, "formData": true, "url": true, "query": true, "agent": true,
	"supertest": true, "mock": true, "nock": true, "msw": true, "rest": true, "graphql": true,
}

var jsServerPkgs = []string{"express", "fastify", "koa", "@koa/router", "koa-router", "hono", "restify", "polka", "elysia",
	"@hapi/hapi", "h3", "itty-router", "@nestjs/common"}

// npm package → dependency.
var jsDeps = map[string][2]string{
	"pg": {"db", "postgres"}, "postgres": {"db", "postgres"}, "pg-promise": {"db", "postgres"}, "@neondatabase/serverless": {"db", "postgres"},
	"mysql": {"db", "mysql"}, "mysql2": {"db", "mysql"}, "mongodb": {"db", "mongodb"}, "mongoose": {"db", "mongodb"},
	"ioredis": {"cache", "redis"}, "redis": {"cache", "redis"}, "@upstash/redis": {"cache", "redis"},
	"kafkajs": {"queue", "kafka"}, "amqplib": {"queue", "rabbitmq"}, "amqp-connection-manager": {"queue", "rabbitmq"},
	"@aws-sdk/client-sqs": {"queue", "sqs"}, "sqs-consumer": {"queue", "sqs"}, "bullmq": {"queue", "redis"}, "bull": {"queue", "redis"},
	"@google-cloud/pubsub": {"queue", "pubsub"}, "stripe": {"http", "api.stripe.com"}, "iyzipay": {"http", "api.iyzipay.com"},
	"@prisma/client": {"db", "prisma"}, "@paypal/checkout-server-sdk": {"http", "api.paypal.com"},
}

var (
	reJSFuncDecl  = regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*[<(]`)
	reJSArrowDecl = regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=>`)
	reJSMethod    = regexp.MustCompile(`(?m)^[ \t]+(?:public\s+|private\s+|protected\s+)?(?:static\s+)?(?:async\s+)?([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?::\s*[^{;]+)?\{`)
)

type jsFuncRef struct {
	file *jsFile
	line int
}

type jsFile struct {
	f        srcFile
	toks     []jsTok
	imports  map[string]bool   // package / relative specifiers
	bindings map[string]string // local identifier → import specifier
	funcs    map[string]int    // function name → line
}

// jsMount is `app.use('/prefix', router)` where router is imported from another file.
type jsMount struct {
	prefix string
	file   string // resolved file (without extension)
}

func extractJS(files []srcFile, svc *serviceMap, hasNext bool) []model.Item {
	var parsed []*jsFile
	index := map[string][]jsFuncRef{}
	for _, f := range files {
		jf := &jsFile{f: f, toks: tokenizeJS(f.src), imports: map[string]bool{}, bindings: map[string]string{}, funcs: map[string]int{}}
		jf.scanImports()
		for _, re := range []*regexp.Regexp{reJSFuncDecl, reJSArrowDecl, reJSMethod} {
			for _, m := range re.FindAllSubmatchIndex(f.src, -1) {
				name := string(f.src[m[2]:m[3]])
				switch name {
				case "if", "for", "while", "switch", "catch", "function", "return", "constructor":
					continue
				}
				if _, ok := jf.funcs[name]; !ok {
					jf.funcs[name] = lineAt(f.src, m[2])
					index[name] = append(index[name], jsFuncRef{jf, jf.funcs[name]})
				}
			}
		}
		parsed = append(parsed, jf)
	}
	var items []model.Item
	var mounts []jsMount
	globalPrefix := map[string]string{} // service → Nest setGlobalPrefix
	for _, jf := range parsed {
		it, m, gp := jf.extract(index)
		items = append(items, it...)
		mounts = append(mounts, m...)
		if gp != "" {
			globalPrefix[svc.service(jf.f.rel)] = gp
		}
		if hasNext {
			items = append(items, jf.nextRoutes()...)
		}
	}
	// Apply cross-file mounts (express/koa/hono app.use('/api', ordersRouter)) and Nest global prefixes.
	for i := range items {
		it := &items[i]
		if it.Kind != model.KindEndpoint {
			continue
		}
		m, p := route.SplitKey(it.Key)
		noExt := strings.TrimSuffix(it.File, path.Ext(it.File))
		for _, mt := range mounts {
			if mt.file == noExt || mt.file+"/index" == noExt {
				p = route.Join(mt.prefix, p)
				break
			}
		}
		if it.Framework == "nestjs" {
			if gp := globalPrefix[svc.service(it.File)]; gp != "" {
				p = route.Join("/"+strings.Trim(gp, "/"), p)
			}
		}
		it.Key = m + " " + route.Normalize(p)
	}
	return items
}

func (jf *jsFile) scanImports() {
	t := jf.toks
	for i := 0; i < len(t); i++ {
		switch {
		case t[i].id("import"):
			// import X from 'p' | import { a, b as c } from 'p' | import * as X from 'p' | import 'p'
			j := i + 1
			var names []string
			for ; j < len(t) && j < i+200; j++ {
				if t[j].kind == tString {
					break
				}
				if t[j].kind == tIdent && !t[j].id("from") && !t[j].id("as") && !t[j].id("type") && !t[j].id("import") {
					names = append(names, t[j].val)
				}
				if t[j].is(";") {
					break
				}
			}
			if j < len(t) && t[j].kind == tString {
				jf.imports[t[j].val] = true
				for _, n := range names {
					jf.bindings[n] = t[j].val
				}
			}
		case t[i].id("require") && i+2 < len(t) && t[i+1].is("(") && t[i+2].kind == tString:
			spec := t[i+2].val
			jf.imports[spec] = true
			// const X = require('p') / const { a } = require('p')
			for k := i - 1; k >= 0 && k >= i-12; k-- {
				if t[k].id("const") || t[k].id("let") || t[k].id("var") {
					for m := k + 1; m < i; m++ {
						if t[m].kind == tIdent {
							jf.bindings[t[m].val] = spec
						}
					}
					break
				}
			}
		}
	}
}

func (jf *jsFile) imported(pkgs ...string) bool {
	for _, p := range pkgs {
		if jf.imports[p] {
			return true
		}
		for spec := range jf.imports {
			if strings.HasPrefix(spec, p+"/") {
				return true
			}
		}
	}
	return false
}

func (jf *jsFile) lang() string { return jf.f.lang }

// snippetFor returns the source of handler name (this file first, then any file) or the registration lines.
func (jf *jsFile) snippetFor(name string, line int, index map[string][]jsFuncRef) string {
	if name != "" {
		if l, ok := jf.funcs[name]; ok {
			return braceBlock(jf.f.src, l)
		}
		if refs := index[name]; len(refs) > 0 {
			return braceBlock(refs[0].file.f.src, refs[0].line)
		}
	}
	return braceBlock(jf.f.src, line)
}

// handlerName returns the last identifier at depth 0 in the argument list toks[from:to] (exclusive), skipping
// inline functions.
func handlerName(toks []jsTok, from, to int) string {
	depth := 0
	last := ""
	for k := from; k < to && k < len(toks); k++ {
		t := toks[k]
		if t.kind == tPunct {
			switch t.val {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case "=>":
				if depth == 0 {
					return ""
				}
			}
			continue
		}
		if depth == 0 && t.kind == tIdent {
			if t.val == "function" || t.val == "async" {
				return ""
			}
			last = t.val
		}
	}
	return last
}

func (jf *jsFile) extract(index map[string][]jsFuncRef) (items []model.Item, mounts []jsMount, globalPrefix string) {
	t := jf.toks
	f := jf.f
	isServer := jf.imported(jsServerPkgs...)
	routers := map[string]string{} // tracked router idents → framework
	nestPrefix, inNest := "", false
	kafkaGroup := ""
	isReactRouter := jf.imported("react-router", "react-router-dom", "@tanstack/react-router", "@remix-run/react")
	isNavigation := jf.imported("@react-navigation/native", "@react-navigation/stack", "@react-navigation/native-stack", "@react-navigation/bottom-tabs", "expo-router")
	cronIdents := map[string]bool{}
	for local, spec := range jf.bindings {
		if spec == "node-cron" || spec == "cron" || spec == "node-schedule" {
			cronIdents[local] = true
		}
	}
	framework := "node"
	for _, p := range jsServerPkgs {
		if jf.imported(p) {
			framework = strings.TrimPrefix(strings.TrimPrefix(p, "@koa/"), "@hapi/")
			if p == "@koa/router" || p == "koa-router" {
				framework = "koa"
			}
			break
		}
	}
	add := func(it model.Item) {
		it.File, it.Language = f.rel, jf.lang()
		items = append(items, it)
	}
	endpoint := func(method, p string, line int, handler, fw string) {
		add(model.Item{Kind: model.KindEndpoint, Key: route.Key(method, p), Line: line, Framework: fw, Handler: handler,
			Snippet: jf.snippetFor(handler, line, index)})
	}

	for i := 0; i < len(t); i++ {
		tk := t[i]
		// const app = express() / Router() / fastify() / new Hono() / new Koa() / new Router()
		if (tk.id("const") || tk.id("let") || tk.id("var")) && i+3 < len(t) && t[i+1].kind == tIdent && t[i+2].is("=") {
			j := i + 3
			if t[j].id("new") {
				j++
			}
			if j < len(t) && t[j].kind == tIdent {
				ctor := t[j].val
				if j+2 < len(t) && t[j+1].is(".") && t[j+2].kind == tIdent { // express.Router()
					ctor = t[j+2].val
				}
				switch ctor {
				case "express", "Router", "fastify", "Fastify", "Koa", "Hono", "Elysia", "polka", "createRouter", "AutoRouter":
					routers[t[i+1].val] = ctor
				}
			}
		}

		// X.method('/path', handler)  |  X.route('/path').get(h).post(h)  |  X.route({ method, url })  |  X.use('/p', r)
		if tk.kind == tIdent && i+4 < len(t) && t[i+1].is(".") && t[i+2].kind == tIdent && t[i+3].is("(") {
			recv, m := tk.val, t[i+2].val
			_, tracked := routers[recv]
			routerish := tracked || (isServer && !jsClientRecv[recv])
			closeIdx := matchClose(t, i+3)
			switch {
			case jsRouteMethods[m] != "" && routerish && t[i+4].kind == tString && i+5 < len(t) && t[i+5].is(","):
				p := t[i+4].val
				if strings.HasPrefix(p, "/") || p == "*" || (p == "" && tracked) {
					endpoint(jsRouteMethods[m], p, tk.line, handlerName(t, i+6, closeIdx), framework)
				}
			case m == "route" && routerish && t[i+4].kind == tString && strings.HasPrefix(t[i+4].val, "/"):
				p := t[i+4].val
				for k := closeIdx + 1; k+2 < len(t) && t[k].is(".") && t[k+1].kind == tIdent && t[k+2].is("("); {
					c := matchClose(t, k+2)
					if meth := jsRouteMethods[t[k+1].val]; meth != "" {
						endpoint(meth, p, t[k+1].line, handlerName(t, k+3, c), framework)
					}
					k = c + 1
				}
			case m == "route" && routerish && t[i+4].is("{"): // fastify.route({ method: 'POST', url: '/x', handler })
				obj := jf.objectProps(i+4, matchClose(t, i+4))
				p := obj["url"]
				if p == "" {
					p = obj["path"]
				}
				if strings.HasPrefix(p, "/") {
					methods := strings.Split(obj["method"], ",")
					for _, meth := range methods {
						endpoint(meth, p, tk.line, obj["handler"], framework)
					}
				}
			case (m == "use" || m == "register" || m == "route" || m == "mount") && i+6 < len(t) && t[i+4].kind == tString && t[i+5].is(","):
				// app.use('/api', ordersRouter) | app.route('/api', orders) (hono) → mount the imported file
				p := t[i+4].val
				if r := handlerName(t, i+6, closeIdx); r != "" && strings.HasPrefix(p, "/") {
					if spec, ok := jf.bindings[r]; ok && strings.HasPrefix(spec, ".") {
						mounts = append(mounts, jsMount{prefix: p, file: path.Clean(path.Join(path.Dir(f.rel), spec))})
					}
				}
			case m == "register" && i+6 < len(t) && t[i+4].kind == tIdent && t[i+5].is(","): // fastify.register(routes, { prefix: '/api' })
				if spec, ok := jf.bindings[t[i+4].val]; ok && strings.HasPrefix(spec, ".") && t[i+6].is("{") {
					if p := jf.objectProps(i+6, matchClose(t, i+6))["prefix"]; p != "" {
						mounts = append(mounts, jsMount{prefix: p, file: path.Clean(path.Join(path.Dir(f.rel), spec))})
					}
				}
			case m == "setGlobalPrefix" && t[i+4].kind == tString:
				globalPrefix = t[i+4].val
			case m == "consumer" && t[i+4].is("{"): // kafka.consumer({ groupId: 'g' })
				if g := jf.objectProps(i+4, matchClose(t, i+4))["groupId"]; g != "" {
					kafkaGroup = g
				}
			case m == "subscribe" && t[i+4].is("{") && jf.imported("kafkajs", "@confluentinc/kafka-javascript"): // consumer.subscribe({ topic | topics })
				obj := jf.objectProps(i+4, matchClose(t, i+4))
				for _, topic := range strings.Split(obj["topic"]+","+obj["topics"], ",") {
					if topic = strings.TrimSpace(topic); topic == "" {
						continue
					}
					key := topic
					if kafkaGroup != "" {
						key = kafkaGroup
					}
					add(model.Item{Kind: model.KindConsumer, Key: key, Source: topic, Line: tk.line, Framework: "kafkajs",
						Snippet: jf.consumerSnippet(i)})
				}
			case m == "consume" && t[i+4].kind == tString && jf.imported("amqplib", "amqp-connection-manager", "amqplib/callback_api"):
				q := t[i+4].val
				add(model.Item{Kind: model.KindConsumer, Key: q, Source: q, Line: tk.line, Framework: "amqplib",
					Handler: handlerName(t, i+6, closeIdx), Snippet: jf.snippetFor(handlerName(t, i+6, closeIdx), tk.line, index)})
			case m == "schedule" && cronIdents[recv] && t[i+4].kind == tString: // cron.schedule('*/5 * * * *', fn)
				h := handlerName(t, i+6, closeIdx)
				add(jf.cronItem(t[i+4].val, h, tk.line, "node-cron", index))
			case m == "create" && recv == "Consumer" && jf.imported("sqs-consumer") && t[i+4].is("{"):
				obj := jf.objectProps(i+4, matchClose(t, i+4))
				q := sqsName(obj["queueUrl"])
				add(model.Item{Kind: model.KindConsumer, Key: q, Source: q, Line: tk.line, Framework: "sqs-consumer",
					Handler: obj["handleMessage"], Snippet: jf.snippetFor(obj["handleMessage"], tk.line, index)})
			}
		}

		// new Worker('queue', handler) (bullmq) | new CronJob('expr', fn) | new ReceiveMessageCommand({ QueueUrl })
		if tk.id("new") && i+3 < len(t) && t[i+1].kind == tIdent && t[i+2].is("(") {
			closeIdx := matchClose(t, i+2)
			switch t[i+1].val {
			case "Worker":
				if jf.imported("bullmq", "bull") && t[i+3].kind == tString {
					q := t[i+3].val
					h := handlerName(t, i+5, closeIdx)
					add(model.Item{Kind: model.KindConsumer, Key: q, Source: q, Line: tk.line, Framework: "bullmq", Handler: h,
						Snippet: jf.snippetFor(h, tk.line, index)})
				}
			case "CronJob":
				if t[i+3].kind == tString {
					h := handlerName(t, i+5, closeIdx)
					add(jf.cronItem(t[i+3].val, h, tk.line, "cron", index))
				}
			case "ReceiveMessageCommand":
				if t[i+3].is("{") {
					q := sqsName(jf.objectProps(i+3, matchClose(t, i+3))["QueueUrl"])
					add(model.Item{Kind: model.KindConsumer, Key: q, Source: q, Line: tk.line, Framework: "aws-sdk-sqs",
						Snippet: braceBlock(f.src, tk.line)})
				}
			}
		}

		// NestJS: @Controller('orders') … @Get(':id') / @Post() / @EventPattern('topic') / @Cron('expr')
		if tk.is("@") && i+2 < len(t) && t[i+1].kind == tIdent && t[i+2].is("(") {
			dec := t[i+1].val
			arg := ""
			argIsObj := false
			if i+3 < len(t) {
				if t[i+3].kind == tString {
					arg = t[i+3].val
				} else if t[i+3].is("{") {
					argIsObj = true
					arg = jf.objectProps(i+3, matchClose(t, i+3))["path"]
				}
			}
			_ = argIsObj
			switch {
			case dec == "Controller":
				nestPrefix, inNest = "/"+strings.Trim(arg, "/"), true
			case nestMethods[dec] != "" && inNest:
				h := jf.nextMethodName(matchClose(t, i+2) + 1)
				add(model.Item{Kind: model.KindEndpoint, Key: route.Key(nestMethods[dec], route.Join(nestPrefix, "/"+strings.TrimLeft(arg, "/"))),
					Line: tk.line, Framework: "nestjs", Handler: h, Snippet: jf.snippetFor(h, tk.line, index)})
			case (dec == "EventPattern" || dec == "MessagePattern") && arg != "":
				h := jf.nextMethodName(matchClose(t, i+2) + 1)
				add(model.Item{Kind: model.KindConsumer, Key: arg, Source: arg, Line: tk.line, Framework: "nestjs-microservices",
					Handler: h, Snippet: jf.snippetFor(h, tk.line, index)})
			case dec == "Processor" && arg != "" && jf.imported("@nestjs/bull", "@nestjs/bullmq"):
				add(model.Item{Kind: model.KindConsumer, Key: arg, Source: arg, Line: tk.line, Framework: "nestjs-bull",
					Snippet: braceBlock(f.src, tk.line)})
			case dec == "Cron" && arg != "" && jf.imported("@nestjs/schedule"):
				h := jf.nextMethodName(matchClose(t, i+2) + 1)
				add(jf.cronItem(arg, h, tk.line, "nestjs-schedule", index))
			}
		}

		// <Route path="/x" …>  |  <Stack.Screen name="Home" …>
		if tk.is("<") && i+1 < len(t) && t[i+1].kind == tIdent {
			tag := t[i+1].val
			k := i + 2
			if k+1 < len(t) && t[k].is(".") && t[k+1].kind == tIdent {
				tag = t[k+1].val
				k += 2
			}
			want := ""
			switch {
			case tag == "Route" && isReactRouter:
				want = "path"
			case tag == "Screen" && isNavigation:
				want = "name"
			}
			if want != "" {
				for ; k+2 < len(t) && k < i+60 && !t[k].is(">"); k++ {
					if t[k].id(want) && t[k+1].is("=") && t[k+2].kind == tString {
						v := t[k+2].val
						if want == "path" {
							v = route.Normalize("/" + strings.TrimLeft(v, "/"))
						}
						add(model.Item{Kind: model.KindView, Key: v, Line: tk.line, Framework: map[string]string{"path": "react-router", "name": "react-navigation"}[want]})
						break
					}
				}
			}
		}
		// Route objects: { path: '/x', element | Component | lazy | children … } in react-router files.
		if isReactRouter && tk.id("path") && i+2 < len(t) && t[i+1].is(":") && t[i+2].kind == tString && i > 0 && (t[i-1].is("{") || t[i-1].is(",")) {
			add(model.Item{Kind: model.KindView, Key: route.Normalize("/" + strings.TrimLeft(t[i+2].val, "/")), Line: tk.line, Framework: "react-router"})
		}
	}
	items = append(items, jf.dependencies()...)
	return items, mounts, globalPrefix
}

// objectProps reads `{ key: 'v', key2: ['a', 'b'], handler: name }` between toks[open] and toks[close] at depth 1.
// Array values are joined with ",".
func (jf *jsFile) objectProps(open, close int) map[string]string {
	t := jf.toks
	out := map[string]string{}
	depth := 0
	for k := open; k <= close && k < len(t); k++ {
		if t[k].kind == tPunct {
			switch t[k].val {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
			continue
		}
		if depth != 1 || (t[k].kind != tIdent && t[k].kind != tString) || k+2 > close || !t[k+1].is(":") {
			continue
		}
		key := t[k].val
		v := t[k+2]
		switch {
		case v.kind == tString:
			out[key] = v.val
		case v.is("["):
			end := matchClose(t, k+2)
			var parts []string
			for m := k + 3; m < end; m++ {
				if t[m].kind == tString {
					parts = append(parts, t[m].val)
				}
			}
			out[key] = strings.Join(parts, ",")
		case v.kind == tIdent:
			// process.env.ORDERS_QUEUE_URL → "env:ORDERS_QUEUE_URL"; handler name otherwise.
			if v.val == "process" && k+6 <= close && t[k+3].is(".") && t[k+4].id("env") && t[k+5].is(".") {
				out[key] = "env:" + t[k+6].val
			} else if v.val != "async" && v.val != "function" {
				out[key] = v.val
			}
		}
	}
	return out
}

// nextMethodName returns the class method name following a decorator (skipping further decorators).
func (jf *jsFile) nextMethodName(from int) string {
	t := jf.toks
	for k := from; k < len(t) && k < from+80; k++ {
		if t[k].is("@") && k+2 < len(t) && t[k+2].is("(") {
			k = matchClose(t, k+2)
			continue
		}
		if t[k].kind == tIdent && k+1 < len(t) && t[k+1].is("(") {
			switch t[k].val {
			case "async", "public", "private", "protected", "static":
				continue
			}
			return t[k].val
		}
	}
	return ""
}

func (jf *jsFile) consumerSnippet(i int) string {
	// kafkajs: the handler is in consumer.run({ eachMessage }) — show from subscribe to the end of run().
	t := jf.toks
	for k := i; k < len(t) && k < i+400; k++ {
		if t[k].id("run") && k+1 < len(t) && t[k+1].is("(") {
			end := matchClose(t, k+1)
			return linesRange(jf.f.src, t[i].line, t[end].line)
		}
	}
	return braceBlock(jf.f.src, t[i].line)
}

func (jf *jsFile) cronItem(expr, handler string, line int, fw string, index map[string][]jsFuncRef) model.Item {
	name := handler
	if name == "" {
		name = strings.TrimSuffix(path.Base(jf.f.rel), path.Ext(jf.f.rel)) + "-cron-" + strconv.Itoa(line)
	}
	return model.Item{Kind: model.KindCron, Key: name, Source: expr, Line: line, Framework: fw, Handler: handler,
		Snippet: jf.snippetFor(handler, line, index)}
}

func sqsName(v string) string {
	switch {
	case strings.HasPrefix(v, "env:"):
		return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(v, "env:"), "_URL"), "_QUEUE"))
	case strings.Contains(v, "/"):
		return v[strings.LastIndexByte(v, '/')+1:]
	case v != "":
		return v
	}
	return "sqs"
}

func (jf *jsFile) dependencies() []model.Item {
	var out []model.Item
	specs := make([]string, 0, len(jf.imports))
	for s := range jf.imports {
		specs = append(specs, s)
	}
	sort.Strings(specs)
	for _, s := range specs {
		if d, ok := jsDeps[s]; ok {
			out = append(out, depItem(d[0], d[1], jf.f.rel, jf.importLine(s), jf.lang()))
		}
	}
	t := jf.toks
	for i := 0; i+2 < len(t); i++ {
		// fetch('https://host/…') | axios.post('https://…') | got('https://…')
		if t[i].kind != tIdent {
			continue
		}
		var arg jsTok
		switch {
		case (t[i].val == "fetch" || t[i].val == "got" || t[i].val == "ky") && t[i+1].is("("):
			arg = t[i+2]
		case (t[i].val == "axios" || t[i].val == "http" || t[i].val == "https") && t[i+1].is(".") && i+4 < len(t) && t[i+3].is("("):
			arg = t[i+4]
		default:
			continue
		}
		if arg.kind == tString || arg.kind == tTemplate {
			if h := hostOf(arg.val); h != "" {
				out = append(out, depItem("http", h, jf.f.rel, arg.line, jf.lang()))
			}
		}
	}
	return out
}

func (jf *jsFile) importLine(spec string) int {
	for _, tk := range jf.toks {
		if tk.kind == tString && tk.val == spec {
			return tk.line
		}
	}
	return 1
}

var nextHTTPMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}

// nextRoutes derives Next.js app/pages router endpoints and views from the file path.
func (jf *jsFile) nextRoutes() []model.Item {
	rel := jf.f.rel
	dir, base := path.Split(rel)
	stem := strings.TrimSuffix(base, path.Ext(base))
	segs := strings.Split(strings.Trim(dir, "/"), "/")
	// find the app/ or pages/ root (optionally under src/)
	root := -1
	kind := ""
	for i, s := range segs {
		if s == "app" || s == "pages" {
			root, kind = i, s
		}
	}
	if root < 0 && (strings.HasPrefix(rel, "app/") || strings.HasPrefix(rel, "pages/")) {
		return nil
	}
	if root < 0 {
		return nil
	}
	// Only treat it as Next.js when the root is at the module level: app/, src/app/, <pkg>/app/, <pkg>/src/app/.
	if root > 0 && segs[root-1] != "src" && root > 2 {
		return nil
	}
	var urlSegs []string
	for _, s := range segs[root+1:] {
		if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") { // route group
			continue
		}
		if strings.HasPrefix(s, "@") || strings.HasPrefix(s, "_") { // parallel routes / private folders
			return nil
		}
		urlSegs = append(urlSegs, s)
	}
	switch kind {
	case "app":
		if stem == "route" {
			p := route.Normalize("/" + strings.Join(urlSegs, "/"))
			var out []model.Item
			for _, m := range jf.exportedFuncs() {
				for _, hm := range nextHTTPMethods {
					if m == hm {
						out = append(out, model.Item{Kind: model.KindEndpoint, Key: route.Key(m, p), File: rel, Line: jf.funcs[m],
							Framework: "nextjs-app", Handler: m, Language: jf.lang(), Snippet: braceBlock(jf.f.src, max(1, jf.funcs[m]))})
					}
				}
			}
			return out
		}
		if stem == "page" {
			return []model.Item{{Kind: model.KindView, Key: route.Normalize("/" + strings.Join(urlSegs, "/")), File: rel, Line: 1,
				Framework: "nextjs-app", Language: jf.lang()}}
		}
	case "pages":
		if strings.HasPrefix(stem, "_") {
			return nil
		}
		parts := urlSegs
		if stem != "index" {
			parts = append(append([]string{}, urlSegs...), stem)
		}
		p := route.Normalize("/" + strings.Join(parts, "/"))
		if len(urlSegs) > 0 && urlSegs[0] == "api" {
			methods := jf.pagesAPIMethods()
			var out []model.Item
			for _, m := range methods {
				out = append(out, model.Item{Kind: model.KindEndpoint, Key: route.Key(m, p), File: rel, Line: 1,
					Framework: "nextjs-pages", Handler: "handler", Language: jf.lang(), Snippet: linesRange(jf.f.src, 1, 120)})
			}
			return out
		}
		if strings.Contains(string(jf.f.src), "export default") {
			return []model.Item{{Kind: model.KindView, Key: p, File: rel, Line: 1, Framework: "nextjs-pages", Language: jf.lang()}}
		}
	}
	return nil
}

// exportedFuncs lists `export (async) function NAME` and `export const NAME` names.
func (jf *jsFile) exportedFuncs() []string {
	t := jf.toks
	var out []string
	for i := 0; i+2 < len(t); i++ {
		if !t[i].id("export") {
			continue
		}
		j := i + 1
		if t[j].id("async") {
			j++
		}
		if j+1 < len(t) && (t[j].id("function") || t[j].id("const") || t[j].id("let")) && t[j+1].kind == tIdent {
			out = append(out, t[j+1].val)
			if _, ok := jf.funcs[t[j+1].val]; !ok {
				jf.funcs[t[j+1].val] = t[j+1].line
			}
		}
		// export { handler as GET, handler as POST }
		if t[j].is("{") {
			end := matchClose(t, j)
			for k := j + 1; k < end; k++ {
				if t[k].kind == tIdent && (k+1 >= end || t[k+1].is(",") || t[k+1].is("}")) {
					out = append(out, t[k].val)
				}
			}
		}
	}
	return out
}

// pagesAPIMethods finds `req.method === 'POST'` / `case 'GET':` checks; none → ANY.
func (jf *jsFile) pagesAPIMethods() []string {
	t := jf.toks
	seen := map[string]bool{}
	var out []string
	for i := 0; i+1 < len(t); i++ {
		if t[i].kind != tString {
			continue
		}
		m := t[i].val
		isM := false
		for _, hm := range nextHTTPMethods {
			if m == hm {
				isM = true
			}
		}
		if !isM || seen[m] {
			continue
		}
		if (i > 0 && (t[i-1].id("case") || t[i-1].is("="))) || t[i+1].is("=") {
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []string{"ANY"}
	}
	return out
}
