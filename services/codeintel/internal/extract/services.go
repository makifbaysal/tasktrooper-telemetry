package extract

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// serviceMap resolves the service of a file (contracts §13):
//  0. a folder mapping set in the dashboard (Options.Overrides; longest path prefix wins);
//  1. `.trooper/config.yaml` `services: [{path, name}]` (longest path prefix wins);
//  2. the nearest directory with a TaskTrooper SDK init (`service:` / `serviceName` / `TROOPER_SERVICE`);
//  3. the nearest module root (go.mod, package.json, pyproject.toml, requirements.txt, setup.py, pom.xml,
//     build.gradle[.kts], *.csproj, Package.swift, pubspec.yaml) → its directory name;
//  4. the repository name.
type serviceMap struct {
	overrides []dirName // dashboard folder mappings, longest first
	explicit  []dirName // sorted by path length, longest first
	detected  map[string]string
	roots     map[string]bool
	repo      string
}

type dirName struct{ dir, name string }

// Evidence says how a folder's service name was found (Root.Evidence).
const (
	EvidenceOverride = "override" // dashboard folder mapping
	EvidenceConfig   = "config"   // .trooper/config.yaml
	EvidenceSDK      = "sdk"      // SDK init / TROOPER_SERVICE
	EvidenceModule   = "module"   // module root directory name
	EvidenceRepo     = "repo"     // repository name
)

// resolved is the service of a file and the folder it was resolved from.
type resolved struct{ name, root, evidence string }

// TrooperConfig is `.trooper/config.yaml`.
type TrooperConfig struct {
	Service  string `yaml:"service"`
	Services []struct {
		Path string `yaml:"path"`
		Name string `yaml:"name"`
	} `yaml:"services"`
}

var (
	reTrooperEnv  = regexp.MustCompile(`TROOPER_SERVICE["']?\s*[=:\s]\s*["']?([A-Za-z0-9_.-]+)`)
	reTrooperEnvK = regexp.MustCompile(`name:\s*["']?TROOPER_SERVICE["']?\s*\n\s*value:\s*["']?([A-Za-z0-9_.-]+)`)
	reServiceKey  = regexp.MustCompile(`\b(?:service|serviceName|service_name|Service|ServiceName)\s*[:=]\s*["']([A-Za-z0-9_.-]+)["']`)
)

func newServiceMap(files []srcFile, repo string, overrides ...Override) *serviceMap {
	m := &serviceMap{detected: map[string]string{}, roots: map[string]bool{}, repo: repo}
	for _, o := range overrides {
		if o.Name != "" {
			m.overrides = append(m.overrides, dirName{cleanDir(o.Path), o.Name})
		}
	}
	sort.SliceStable(m.overrides, func(i, j int) bool { return len(m.overrides[i].dir) > len(m.overrides[j].dir) })
	for _, f := range files {
		dir := path.Dir(f.rel)
		switch {
		case f.rel == ".trooper/config.yaml" || f.rel == ".trooper/config.yml":
			var c TrooperConfig
			if yaml.Unmarshal(f.src, &c) == nil {
				for _, s := range c.Services {
					if s.Name != "" {
						m.explicit = append(m.explicit, dirName{cleanDir(s.Path), s.Name})
					}
				}
				if c.Service != "" {
					m.explicit = append(m.explicit, dirName{".", c.Service})
				}
			}
		case f.lang == "manifest":
			m.roots[dir] = true
		}
		if name := detectSDKService(f); name != "" {
			if _, ok := m.detected[dir]; !ok {
				m.detected[dir] = name
			}
		}
	}
	sort.SliceStable(m.explicit, func(i, j int) bool { return len(m.explicit[i].dir) > len(m.explicit[j].dir) })
	return m
}

// detectSDKService finds a service name in an SDK init / TROOPER_SERVICE setting.
func detectSDKService(f srcFile) string {
	switch f.lang {
	case "go", "typescript", "javascript", "vue", "python", "java", "kotlin", "csharp", "swift", "dart", "config", "yaml":
	default:
		return ""
	}
	s := string(f.src)
	if m := reTrooperEnvK.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := reTrooperEnv.FindStringSubmatch(s); m != nil && !strings.HasPrefix(m[1], "process") && m[1] != "os" {
		return m[1]
	}
	if f.lang == "config" || f.lang == "yaml" {
		return ""
	}
	// Only files that use the TaskTrooper SDK: `trooper.Init(trooper.Config{Service: "x"})`, `init({ service: 'x' })`.
	low := strings.ToLower(s)
	if !strings.Contains(low, "trooper") {
		return ""
	}
	i := strings.Index(low, "trooper")
	window := s[i:min(len(s), i+4000)]
	if m := reServiceKey.FindStringSubmatch(window); m != nil {
		return m[1]
	}
	return ""
}

func (m *serviceMap) service(rel string) string { return m.resolve(rel).name }

// cleanDir normalises a folder path: slash separated, no leading/trailing slash, "." for the repository root.
func cleanDir(p string) string {
	p = strings.Trim(path.Clean("/"+strings.TrimSpace(p)), "/")
	if p == "" {
		return "."
	}
	return p
}

// under reports whether rel is inside dir ("." is the whole repository).
func under(rel, dir string) bool {
	return dir == "." || rel == dir || strings.HasPrefix(rel, dir+"/")
}

// detectedName is the name the code itself gives the folder of rel, ignoring dashboard overrides.
func (m *serviceMap) detectedName(rel string) resolved {
	for _, e := range m.explicit {
		if under(rel, e.dir) {
			return resolved{e.name, e.dir, EvidenceConfig}
		}
	}
	for dir := path.Dir(rel); ; dir = path.Dir(dir) {
		if n, ok := m.detected[dir]; ok {
			return resolved{n, dir, EvidenceSDK}
		}
		if dir == "." || dir == "/" {
			break
		}
	}
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if m.roots[dir] {
			return resolved{path.Base(dir), dir, EvidenceModule}
		}
	}
	return resolved{m.repo, ".", EvidenceRepo}
}

// resolve returns the service of rel with the folder and evidence it came from.
func (m *serviceMap) resolve(rel string) resolved {
	for _, e := range m.overrides {
		if under(rel, e.dir) {
			return resolved{e.name, e.dir, EvidenceOverride}
		}
	}
	return m.detectedName(rel)
}
