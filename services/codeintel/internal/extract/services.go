package extract

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// serviceMap resolves the service of a file (contracts §13):
//  1. `.trooper/config.yaml` `services: [{path, name}]` (longest path prefix wins);
//  2. the nearest directory with a TaskTrooper SDK init (`service:` / `serviceName` / `TROOPER_SERVICE`);
//  3. the nearest module root (go.mod, package.json, pyproject.toml, requirements.txt, setup.py) → its directory name;
//  4. the repository name.
type serviceMap struct {
	explicit []dirName // sorted by path length, longest first
	detected map[string]string
	roots    map[string]bool
	repo     string
}

type dirName struct{ dir, name string }

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

func newServiceMap(files []srcFile, repo string) *serviceMap {
	m := &serviceMap{detected: map[string]string{}, roots: map[string]bool{}, repo: repo}
	for _, f := range files {
		dir := path.Dir(f.rel)
		switch {
		case f.rel == ".trooper/config.yaml" || f.rel == ".trooper/config.yml":
			var c TrooperConfig
			if yaml.Unmarshal(f.src, &c) == nil {
				for _, s := range c.Services {
					if s.Name != "" {
						m.explicit = append(m.explicit, dirName{strings.Trim(path.Clean(s.Path), "/"), s.Name})
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
	case "go", "typescript", "javascript", "python", "config", "yaml":
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

func (m *serviceMap) service(rel string) string {
	for _, e := range m.explicit {
		if e.dir == "." || rel == e.dir || strings.HasPrefix(rel, e.dir+"/") {
			return e.name
		}
	}
	for dir := path.Dir(rel); ; dir = path.Dir(dir) {
		if n, ok := m.detected[dir]; ok {
			return n
		}
		if dir == "." || dir == "/" {
			break
		}
	}
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if m.roots[dir] {
			return path.Base(dir)
		}
	}
	return m.repo
}
