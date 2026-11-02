package extract

// Vue single-file components, vue-router / Angular router route trees and Nuxt file-based routing.

import (
	"bytes"
	"path"
	"regexp"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

var reVueScriptTS = regexp.MustCompile(`(?i)\blang\s*=\s*["']?tsx?\b`)

// vueScript keeps only the contents of the <script> / <script setup> blocks of a Vue SFC; everything else
// (template, style, custom blocks, HTML comments) is reduced to its newlines so line numbers stay the same.
// The language is typescript when any script block has lang="ts" / "tsx", javascript otherwise.
func vueScript(src []byte) ([]byte, string) {
	out := make([]byte, 0, len(src))
	lang := "javascript"
	newlines := func(b []byte) {
		for range bytes.Count(b, []byte{'\n'}) {
			out = append(out, '\n')
		}
	}
	for i := 0; i < len(src); {
		rest := src[i:]
		if bytes.HasPrefix(rest, []byte("<!--")) {
			end := bytes.Index(rest[4:], []byte("-->"))
			if end < 0 {
				newlines(rest)
				break
			}
			newlines(rest[:4+end+3])
			i += 4 + end + 3
			continue
		}
		if len(rest) > 7 && bytes.EqualFold(rest[:7], []byte("<script")) && strings.IndexByte(" \t\r\n>", rest[7]) >= 0 {
			gt := bytes.IndexByte(rest, '>')
			if gt < 0 {
				newlines(rest)
				break
			}
			attrs := rest[7:gt]
			newlines(rest[:gt+1])
			i += gt + 1
			if bytes.HasSuffix(bytes.TrimSpace(attrs), []byte("/")) { // <script src="x" />
				continue
			}
			if reVueScriptTS.Match(attrs) {
				lang = "typescript"
			}
			end := indexFold(src[i:], "</script")
			if end < 0 {
				end = len(src) - i
			}
			out = append(out, src[i:i+end]...)
			i += end
			continue
		}
		if src[i] == '\n' {
			out = append(out, '\n')
		}
		i++
	}
	return out, lang
}

func indexFold(b []byte, s string) int {
	n := len(s)
	for i := 0; i+n <= len(b); i++ {
		if bytes.EqualFold(b[i:i+n], []byte(s)) {
			return i
		}
	}
	return -1
}

// nuxtRoots returns the directories holding a nuxt.config.* (the Nuxt app roots, "." for the repo root).
func nuxtRoots(files []srcFile) []string {
	var roots []string
	for _, f := range files {
		switch path.Base(f.rel) {
		case "nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs", "nuxt.config.mts":
			roots = append(roots, path.Dir(f.rel))
		}
	}
	return roots
}

// underNuxt reports whether rel lies inside one of the Nuxt app roots.
func underNuxt(rel string, roots []string) bool {
	for _, r := range roots {
		if r == "." || strings.HasPrefix(rel, r+"/") {
			return true
		}
	}
	return false
}

// nuxtRoutes derives the view of a Nuxt page from its path: <root>/pages/orders/[id].vue → /orders/:id,
// index → parent path, route groups "(x)" are dropped. Nuxt 4's <root>/app/pages is supported too.
func (jf *jsFile) nuxtRoutes(roots []string) []model.Item {
	rel := jf.f.rel
	switch path.Ext(rel) {
	case ".vue", ".tsx", ".jsx":
	default:
		return nil
	}
	rest := ""
	best := -1
	for _, r := range roots {
		pre := r + "/"
		if r == "." {
			pre = ""
		}
		for _, pages := range []string{pre + "pages/", pre + "app/pages/"} {
			if strings.HasPrefix(rel, pages) && len(pages) > best {
				rest, best = strings.TrimPrefix(rel, pages), len(pages)
			}
		}
	}
	if best < 0 {
		return nil
	}
	segs := strings.Split(strings.TrimSuffix(rest, path.Ext(rest)), "/")
	var url []string
	for i, s := range segs {
		if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
			continue
		}
		if i == len(segs)-1 && s == "index" {
			continue
		}
		url = append(url, s)
	}
	return []model.Item{{Kind: model.KindView, Key: route.Normalize("/" + strings.Join(url, "/")), File: rel, Line: 1,
		Framework: "nuxt", Language: jf.lang()}}
}

// routeObj is one `{ path, component, children, loadChildren, redirect }` record of a route tree.
type routeObj struct {
	path      string
	hasPath   bool
	line      int
	children  int    // token index of the children array, -1 if none
	childRef  string // children: namedRoutes
	lazy      string // loadChildren: () => import('./x') specifier
	redirect  bool
	component bool
}

func (jf *jsFile) routeObject(open, close int) routeObj {
	t := jf.toks
	o := routeObj{children: -1, line: t[open].line}
	for k := open + 1; k < close; k++ {
		if t[k].is("(") || t[k].is("[") || t[k].is("{") {
			k = matchClose(t, k)
			continue
		}
		if (t[k].kind != tIdent && t[k].kind != tString) || k+2 >= close || !t[k+1].is(":") {
			continue
		}
		v := k + 2
		switch t[k].val {
		case "path":
			if t[v].kind == tString {
				o.path, o.hasPath, o.line = t[v].val, true, t[v].line
			}
		case "children":
			if t[v].is("[") {
				o.children = v
			} else if t[v].kind == tIdent {
				o.childRef = t[v].val
			}
		case "loadChildren":
			if t[v].kind == tString { // legacy './admin/admin.module#AdminModule'
				o.lazy, _, _ = strings.Cut(t[v].val, "#")
				break
			}
			for m := v; m+2 < close; m++ {
				if t[m].is(",") {
					break
				}
				if t[m].id("import") && t[m+1].is("(") && t[m+2].kind == tString {
					o.lazy = t[m+2].val
					break
				}
				if t[m].is("{") || t[m].is("[") {
					m = matchClose(t, m)
				}
			}
		case "redirect", "redirectTo":
			o.redirect = true
		case "component", "components", "loadComponent":
			o.component = true
		}
	}
	return o
}

// isRouteArray reports whether toks[open] ("[") starts an array of route records.
func (jf *jsFile) isRouteArray(open int) bool {
	t := jf.toks
	if open+1 >= len(t) || !t[open+1].is("{") {
		return false
	}
	close := matchClose(t, open)
	for k := open + 1; k < close; k++ {
		if t[k].is("{") {
			c := matchClose(t, k)
			o := jf.routeObject(k, c)
			if o.hasPath && (o.component || o.children >= 0 || o.childRef != "" || o.lazy != "" || o.redirect) {
				return true
			}
			k = c
		}
	}
	return false
}

// routeTrees emits the views of vue-router / Angular router route arrays with nested children joined to their
// parent. Arrays referenced by name (`children: adminRoutes`, `...adminRoutes`) are walked under their parent.
// Angular `loadChildren: () => import('./x')` records a lazy mount: the views of ./x get the parent path.
func (jf *jsFile) routeTrees(fw string) (items []model.Item, lazy []jsMount) {
	t := jf.toks
	named := map[string]int{}
	namedAt := map[int]string{}
	referenced := map[string]bool{}
	for i := 0; i+2 < len(t); i++ {
		if (t[i].id("const") || t[i].id("let") || t[i].id("var")) && t[i+1].kind == tIdent {
			for j := i + 2; j+1 < len(t) && j < i+30; j++ {
				if t[j].is(";") || t[j].is("(") {
					break
				}
				if t[j].is("=") {
					if t[j+1].is("[") {
						named[t[i+1].val] = j + 1
						namedAt[j+1] = t[i+1].val
					}
					break
				}
			}
		}
		if (t[i].id("children") && t[i+1].is(":") && t[i+2].kind == tIdent) || (t[i].is("...") && t[i+1].kind == tIdent) {
			if t[i].is("...") {
				referenced[t[i+1].val] = true
			} else {
				referenced[t[i+2].val] = true
			}
		}
	}
	var walk func(open int, prefix string, depth int)
	emit := func(p string, line int) {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			if s == "**" {
				segs[i] = "*"
			}
		}
		items = append(items, model.Item{Kind: model.KindView, Key: route.Normalize("/" + strings.TrimLeft(strings.Join(segs, "/"), "/")),
			Line: line, Framework: fw})
	}
	walk = func(open int, prefix string, depth int) {
		if depth > 20 || open >= len(t) || !t[open].is("[") {
			return
		}
		close := matchClose(t, open)
		for k := open + 1; k < close; k++ {
			switch {
			case t[k].is("...") && k+1 < close && t[k+1].kind == tIdent:
				if at, ok := named[t[k+1].val]; ok {
					walk(at, prefix, depth+1)
				}
			case t[k].is("{"):
				c := matchClose(t, k)
				o := jf.routeObject(k, c)
				full := prefix
				if o.hasPath {
					switch {
					case fw == "vue-router" && strings.HasPrefix(o.path, "/"):
						full = o.path
					default:
						full = route.Join(prefix, o.path)
						if full == "" {
							full = "/"
						}
					}
				}
				hasChildren := o.children >= 0 || o.childRef != ""
				switch {
				case o.children >= 0:
					walk(o.children, full, depth+1)
				case o.childRef != "":
					if at, ok := named[o.childRef]; ok {
						walk(at, full, depth+1)
					}
				}
				switch {
				case !o.hasPath || o.redirect:
				case o.lazy != "":
					if strings.HasPrefix(o.lazy, ".") {
						lazy = append(lazy, jsMount{prefix: route.Normalize("/" + strings.TrimLeft(full, "/")),
							file: path.Clean(path.Join(path.Dir(jf.f.rel), o.lazy))})
					}
				case !hasChildren || (fw == "vue-router" && o.component):
					emit(full, o.line)
				}
				k = c
			}
		}
	}
	for i := 0; i < len(t); i++ {
		if !t[i].is("[") {
			continue
		}
		if name, ok := namedAt[i]; ok && referenced[name] {
			i = matchClose(t, i)
			continue
		}
		if jf.isRouteArray(i) {
			walk(i, "", 0)
			i = matchClose(t, i)
		}
	}
	return items, lazy
}

// applyLazyMounts prefixes the Angular router views of lazily loaded route files with the path they are mounted
// under (transitively: a lazy file can itself lazy-load another one).
func applyLazyMounts(items []model.Item, lazy map[string][]jsMount) {
	prefix := map[string]string{} // target file (no extension) → prefix
	for range 5 {
		for src, ms := range lazy {
			for _, m := range ms {
				prefix[m.file] = route.Join(prefix[strings.TrimSuffix(src, path.Ext(src))], m.prefix)
			}
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind != model.KindView || it.Framework != "angular-router" {
			continue
		}
		noExt := strings.TrimSuffix(it.File, path.Ext(it.File))
		p, ok := prefix[noExt]
		if !ok {
			p, ok = prefix[path.Dir(noExt)] // import('./admin') → admin/index.ts
			ok = ok && path.Base(noExt) == "index"
		}
		if ok {
			it.Key = route.Normalize(route.Join(p, it.Key))
		}
	}
}
