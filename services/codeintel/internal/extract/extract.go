// Package extract builds the code inventory of a repository checkout: endpoints, consumers, crons, views and
// outgoing dependencies (contracts §13). Extractors are deterministic and pure Go: Go source through go/ast,
// TS/JS through a comment/string-aware tokenizer, Python through a line scanner, Kubernetes CronJobs through YAML.
package extract

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// Limits keep a hostile or huge repository from exhausting the analyzer.
const (
	MaxFiles    = 50000
	MaxFileSize = 1 << 20
	maxSnippet  = 6000
)

// Options narrow an extraction.
type Options struct {
	// Only, when non-nil, keeps items whose file is in the set (incremental analysis). Every file is still
	// read so cross-file facts (router mounts, URL includes, service mapping) stay correct.
	Only map[string]bool
	// RepoName names the service of files outside any module/service directory (default: root dir name).
	RepoName string
}

// Result is the inventory of a checkout.
type Result struct {
	Items     []model.Item
	Languages []string // by file count, most used first
	Services  []string // sorted
	Files     int
}

// srcFile is one source file loaded for extraction.
type srcFile struct {
	rel  string // slash separated, relative to root
	lang string
	src  []byte
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".next": true, "out": true,
	"coverage": true, "__pycache__": true, ".venv": true, "venv": true, "env": true, ".tox": true, "target": true,
	"bin": true, "obj": true, ".idea": true, ".vscode": true, "__tests__": true, "__mocks__": true, "testdata": true,
	"fixtures": true, ".turbo": true, ".cache": true, "site-packages": true,
}

func langOf(rel string) string {
	base := path.Base(rel)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return ""
	case strings.HasSuffix(base, ".go"):
		return "go"
	case strings.HasSuffix(base, ".d.ts"), strings.Contains(base, ".test."), strings.Contains(base, ".spec."),
		strings.Contains(base, ".stories."), strings.HasSuffix(base, ".min.js"):
		return ""
	case strings.HasSuffix(base, ".ts"), strings.HasSuffix(base, ".tsx"), strings.HasSuffix(base, ".mts"), strings.HasSuffix(base, ".cts"):
		return "typescript"
	case strings.HasSuffix(base, ".js"), strings.HasSuffix(base, ".jsx"), strings.HasSuffix(base, ".mjs"), strings.HasSuffix(base, ".cjs"):
		return "javascript"
	case strings.HasSuffix(base, ".py"):
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "conftest.py" {
			return ""
		}
		return "python"
	case strings.HasSuffix(base, ".yaml"), strings.HasSuffix(base, ".yml"):
		return "yaml"
	case base == "Dockerfile", strings.HasPrefix(base, ".env"), strings.HasSuffix(base, ".dockerfile"):
		return "config"
	case base == "go.mod", base == "package.json", base == "pyproject.toml", base == "requirements.txt", base == "setup.py":
		return "manifest"
	}
	return ""
}

// Repo extracts the inventory of the checkout at root.
func Repo(root string, opt Options) (*Result, error) {
	if opt.RepoName == "" {
		opt.RepoName = filepath.Base(filepath.Clean(root))
	}
	var files []srcFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (skipDirs[name] || (strings.HasPrefix(name, ".") && name != ".trooper" && name != ".github")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || len(files) >= MaxFiles {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		lang := langOf(rel)
		if lang == "" && !strings.HasPrefix(rel, ".trooper/") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > MaxFileSize {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 { // binary
			return nil
		}
		files = append(files, srcFile{rel: rel, lang: lang, src: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return FromFiles(files, opt), nil
}

// FromFiles runs every extractor over files already in memory.
func FromFiles(files []srcFile, opt Options) *Result {
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	svc := newServiceMap(files, opt.RepoName)
	res := &Result{Files: len(files)}

	var items []model.Item
	counts := map[string]int{}
	goFiles, jsFiles, pyFiles := []srcFile{}, []srcFile{}, []srcFile{}
	for _, f := range files {
		switch f.lang {
		case "go":
			goFiles = append(goFiles, f)
		case "typescript", "javascript":
			jsFiles = append(jsFiles, f)
		case "python":
			pyFiles = append(pyFiles, f)
		case "yaml":
			items = append(items, extractK8sCron(f)...)
		}
		if f.lang == "go" || f.lang == "typescript" || f.lang == "javascript" || f.lang == "python" {
			counts[f.lang]++
		}
	}
	items = append(items, extractGo(goFiles)...)
	hasNext := false
	for _, f := range files {
		if path.Base(f.rel) == "package.json" && bytes.Contains(f.src, []byte(`"next"`)) {
			hasNext = true
		}
	}
	items = append(items, extractJS(jsFiles, svc, hasNext)...)
	items = append(items, extractPython(pyFiles)...)

	for i := range items {
		if items[i].Service == "" {
			items[i].Service = svc.service(items[i].File)
		}
	}
	items = dedupe(items)
	if opt.Only != nil {
		kept := items[:0]
		for _, it := range items {
			if opt.Only[it.File] {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	services := map[string]bool{}
	for i := range items {
		if len(items[i].Snippet) > maxSnippet {
			items[i].Snippet = items[i].Snippet[:maxSnippet]
		}
		items[i].ComputeHash()
		services[items[i].Service] = true
	}
	res.Items = items
	for s := range services {
		res.Services = append(res.Services, s)
	}
	sort.Strings(res.Services)
	for l := range counts {
		res.Languages = append(res.Languages, l)
	}
	sort.Slice(res.Languages, func(i, j int) bool {
		a, b := res.Languages[i], res.Languages[j]
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		return a < b
	})
	return res
}

// dedupe keeps the first occurrence (by file, line) of each item identity; dependencies keep every file in meta.
func dedupe(items []model.Item) []model.Item {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].File != items[j].File {
			return items[i].File < items[j].File
		}
		return items[i].Line < items[j].Line
	})
	seen := map[string]int{}
	out := items[:0]
	for _, it := range items {
		id := it.Identity()
		if j, ok := seen[id]; ok {
			if it.Kind == model.KindDependency {
				n, _ := out[j].Meta["usages"].(int)
				out[j].Meta["usages"] = n + 1
			}
			if it.Kind == model.KindConsumer && it.Source != "" && !containsCSV(out[j].Source, it.Source) {
				out[j].Source += "," + it.Source // one process reading several topics
			}
			continue
		}
		if it.Meta == nil {
			it.Meta = map[string]any{}
		}
		if it.Kind == model.KindDependency {
			it.Meta["usages"] = 1
		}
		seen[id] = len(out)
		out = append(out, it)
	}
	return out
}

// lineAt returns the 1-based line of byte offset off.
func lineAt(src []byte, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return bytes.Count(src[:off], []byte{'\n'}) + 1
}

// lines returns lines [from, to] (1-based, inclusive) of src.
func linesRange(src []byte, from, to int) string {
	ls := strings.Split(string(src), "\n")
	if from < 1 {
		from = 1
	}
	if to > len(ls) {
		to = len(ls)
	}
	if from > to {
		return ""
	}
	return strings.Join(ls[from-1:to], "\n")
}

// braceBlock returns the source from line `from` through the line where the first '{' opened at or after it
// closes (max 120 lines). Strings and comments are not tracked; good enough for snippets.
func braceBlock(src []byte, from int) string {
	ls := strings.Split(string(src), "\n")
	if from < 1 || from > len(ls) {
		return ""
	}
	depth, opened := 0, false
	end := from
	for i := from - 1; i < len(ls) && i < from-1+120; i++ {
		for _, c := range ls[i] {
			switch c {
			case '{':
				depth++
				opened = true
			case '}':
				depth--
			}
		}
		end = i + 1
		if opened && depth <= 0 {
			break
		}
		if !opened && i > from+3 { // a one-liner registration
			break
		}
	}
	return strings.Join(ls[from-1:end], "\n")
}

// hostOf returns the host of an absolute http(s) URL literal, or "".
func hostOf(u string) string {
	rest, ok := strings.CutPrefix(u, "https://")
	if !ok {
		if rest, ok = strings.CutPrefix(u, "http://"); !ok {
			return ""
		}
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	if rest == "" || strings.ContainsAny(rest, "${}<> ") || rest == "localhost" || strings.HasPrefix(rest, "localhost:") ||
		strings.HasPrefix(rest, "127.0.0.1") || strings.HasPrefix(rest, "0.0.0.0") {
		return ""
	}
	return strings.ToLower(rest)
}

// depItem builds a dependency item "<kind> · <name>".
func depItem(kind, name, file string, line int, lang string) model.Item {
	return model.Item{Kind: model.KindDependency, Key: kind + " · " + name, File: file, Line: line, Language: lang,
		Meta: map[string]any{"depKind": kind, "name": name}}
}

func containsCSV(list, v string) bool {
	for _, s := range strings.Split(list, ",") {
		if s == v {
			return true
		}
	}
	return false
}
