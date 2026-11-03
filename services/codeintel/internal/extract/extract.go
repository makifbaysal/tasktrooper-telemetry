// Package extract builds the code inventory of a repository checkout: endpoints, consumers, crons, views and
// outgoing dependencies (contracts §13). Extractors are deterministic and pure Go: Go source through go/ast,
// TS/JS (and Vue single-file components) through a comment/string-aware tokenizer, Python, Java/Kotlin (JVM +
// Android), C# (.NET), Swift (iOS) and Dart (Flutter) through comment/string-blanking line scanners, Kubernetes
// CronJobs through YAML — every language a TaskTrooper SDK supports.
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
	// Overrides are folder → service mappings set in the dashboard; they win over everything the code says.
	Overrides []Override
	// Ignore lists folders whose items are left out of the inventory (dashboard "ignore this folder").
	Ignore []string
}

// Override maps a folder (slash separated, "." = repository root) to a service name.
type Override struct{ Path, Name string }

// Root is one folder of the repository that owns inventory items: how the code names it, the service its items
// get, and what kind of app it looks like. Roots cover the whole checkout, also in incremental analysis.
type Root struct {
	Path     string // folder the name was resolved from ("." = repository root)
	Detected string // name the code gives it (config, SDK init, module dir or repo name)
	Evidence string // EvidenceConfig | EvidenceSDK | EvidenceModule | EvidenceRepo
	Service  string // service its items get (Detected unless a dashboard override applies)
	Kind     string // backend | web | mobile | worker | library
	Items    int    // items (dependencies excluded)
}

// Result is the inventory of a checkout.
type Result struct {
	Items     []model.Item
	Languages []string // by file count, most used first
	Services  []string // sorted
	Roots     []Root   // sorted by path
	Files     int
}

// codeLangs are the languages counted in Result.Languages.
var codeLangs = map[string]bool{"go": true, "typescript": true, "javascript": true, "vue": true, "python": true,
	"java": true, "kotlin": true, "csharp": true, "swift": true, "dart": true}

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
	// JVM / Android, .NET, iOS, Flutter build and dependency output
	"Pods": true, "DerivedData": true, "Carthage": true, "generated": true, "intermediates": true,
}

// testDir reports whether a directory of rel is a test source root of the JVM, .NET, Swift or Dart layouts.
func testDir(rel string) bool {
	for _, seg := range strings.Split(path.Dir(rel), "/") {
		switch {
		case seg == "test", seg == "tests", seg == "Tests", seg == "androidTest", seg == "testFixtures", seg == "integration_test",
			strings.HasSuffix(seg, ".Tests"), strings.HasSuffix(seg, ".Test"), strings.HasSuffix(seg, "Tests"):
			return true
		}
	}
	return false
}

// testName reports whether a file name follows a JVM, .NET, Swift or Dart test naming convention.
func testName(base string) bool {
	stem := strings.TrimSuffix(base, path.Ext(base))
	return strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests") || strings.HasSuffix(stem, "Spec") ||
		strings.HasSuffix(stem, "IT") || strings.HasSuffix(stem, "_test")
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
	// manifests before the language suffixes: Package.swift is not Swift source
	case base == "pubspec.yaml", base == "pom.xml", base == "build.gradle", base == "build.gradle.kts", base == "Package.swift",
		strings.HasSuffix(base, ".csproj"):
		return "manifest"
	case strings.HasSuffix(base, ".vue"):
		return "vue"
	case strings.HasSuffix(base, ".java"), strings.HasSuffix(base, ".kt"), strings.HasSuffix(base, ".cs"),
		strings.HasSuffix(base, ".swift"), strings.HasSuffix(base, ".dart"):
		if testName(base) || testDir(rel) || strings.HasSuffix(base, ".g.dart") || strings.HasSuffix(base, ".freezed.dart") ||
			strings.HasSuffix(base, ".Designer.cs") || strings.HasSuffix(base, ".g.cs") {
			return ""
		}
		return map[string]string{".java": "java", ".kt": "kotlin", ".cs": "csharp", ".swift": "swift", ".dart": "dart"}[path.Ext(base)]
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
	svc := newServiceMap(files, opt.RepoName, opt.Overrides...)
	res := &Result{Files: len(files)}

	var items []model.Item
	counts := map[string]int{}
	var goFiles, jsFiles, pyFiles, jvmFiles, csFiles, swiftFiles, dartFiles []srcFile
	for _, f := range files {
		switch f.lang {
		case "go":
			goFiles = append(goFiles, f)
		case "typescript", "javascript", "vue":
			jsFiles = append(jsFiles, f)
		case "python":
			pyFiles = append(pyFiles, f)
		case "java", "kotlin":
			jvmFiles = append(jvmFiles, f)
		case "csharp":
			csFiles = append(csFiles, f)
		case "swift":
			swiftFiles = append(swiftFiles, f)
		case "dart":
			dartFiles = append(dartFiles, f)
		case "yaml":
			items = append(items, extractK8sCron(f)...)
		}
		if codeLangs[f.lang] {
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
	items = append(items, extractJVM(jvmFiles)...)
	items = append(items, extractDotnet(csFiles)...)
	items = append(items, extractSwift(swiftFiles)...)
	items = append(items, extractDart(dartFiles)...)

	roots := map[string]*Root{}
	kinds := map[string]map[string]int{}
	for i := range items {
		if items[i].Service != "" {
			continue // the extractor named it (e.g. TROOPER_SERVICE in a k8s CronJob)
		}
		d, r := svc.detectedName(items[i].File), svc.resolve(items[i].File)
		items[i].Service, items[i].Root = r.name, d.root
		rt := roots[d.root]
		if rt == nil {
			rt = &Root{Path: d.root, Detected: d.name, Evidence: d.evidence, Service: r.name}
			roots[d.root], kinds[d.root] = rt, map[string]int{}
		}
		if items[i].Kind != model.KindDependency {
			rt.Items++
			kinds[d.root][items[i].Kind]++
		}
	}
	items = dropIgnored(items, opt.Ignore)
	for p, rt := range roots {
		if rt.Items == 0 {
			continue // dependencies only: a library, nothing to link
		}
		rt.Kind = rootKind(files, p, kinds[p])
		res.Roots = append(res.Roots, *rt)
	}
	sort.Slice(res.Roots, func(i, j int) bool { return res.Roots[i].Path < res.Roots[j].Path })
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

// dropIgnored removes items of ignored folders.
func dropIgnored(items []model.Item, ignore []string) []model.Item {
	if len(ignore) == 0 {
		return items
	}
	kept := items[:0]
	for _, it := range items {
		skip := false
		for _, d := range ignore {
			if under(it.File, cleanDir(d)) {
				skip = true
				break
			}
		}
		if !skip {
			kept = append(kept, it)
		}
	}
	return kept
}

// rootKind guesses what a folder is from its manifests and the kinds of items in it.
func rootKind(files []srcFile, dir string, kinds map[string]int) string {
	mobile, web := false, false
	for _, f := range files {
		if f.lang != "manifest" || path.Dir(f.rel) != dir {
			continue
		}
		switch base := path.Base(f.rel); {
		case base == "pubspec.yaml", base == "Package.swift":
			mobile = true
		case strings.HasPrefix(base, "build.gradle") && bytes.Contains(f.src, []byte("com.android")):
			mobile = true
		case base == "package.json":
			switch {
			case bytes.Contains(f.src, []byte(`"react-native"`)), bytes.Contains(f.src, []byte(`"expo"`)):
				mobile = true
			case bytes.Contains(f.src, []byte(`"react"`)), bytes.Contains(f.src, []byte(`"vue"`)), bytes.Contains(f.src, []byte(`"@angular/core"`)),
				bytes.Contains(f.src, []byte(`"next"`)), bytes.Contains(f.src, []byte(`"svelte"`)):
				web = true
			}
		}
	}
	switch {
	case mobile:
		return "mobile"
	case web && kinds[model.KindView] > 0:
		return "web" // a web app with API routes (Next.js) is still the web app
	case kinds[model.KindEndpoint] > 0:
		return "backend"
	case web || kinds[model.KindView] > 0:
		return "web"
	case kinds[model.KindConsumer] > 0 || kinds[model.KindCron] > 0:
		return "worker"
	}
	return "library"
}
