// Package servicemap links the folders of a repository to the services the project's telemetry knows
// (.ai/code-intelligence.md § Folder → service mapping). Folders it cannot link are left "unmatched" for the user to
// resolve in the dashboard; user decisions ("confirmed", "ignored") are never changed here.
package servicemap

import (
	"sort"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/extract"
)

// Status of a folder mapping (repo_service_mappings.status).
const (
	Matched   = "matched"   // linked automatically to a service with telemetry
	Unmatched = "unmatched" // no telemetry service fits; the dashboard asks the user
	Confirmed = "confirmed" // the user picked the service
	Ignored   = "ignored"   // the user excluded the folder from the inventory
)

// Mapping is one folder of a repository and the service it is linked to.
type Mapping struct {
	Path     string
	Detected string
	Evidence string
	Kind     string
	Items    int
	Service  string // "" while unmatched / ignored
	Status   string
}

// Decided reports whether the user made the call (codeintel keeps those rows as they are).
func (m Mapping) Decided() bool { return m.Status == Confirmed || m.Status == Ignored }

// Overrides are the folder → service mappings the extractor must apply: user-confirmed folders and folders matched to
// a telemetry service under a different name than the code uses.
func Overrides(ms []Mapping) []extract.Override {
	var out []extract.Override
	for _, m := range ms {
		if (m.Status == Confirmed || m.Status == Matched) && m.Service != "" && m.Service != m.Detected {
			out = append(out, extract.Override{Path: m.Path, Name: m.Service})
		}
	}
	return out
}

// IgnoredPaths are the folders the user excluded.
func IgnoredPaths(ms []Mapping) []string {
	var out []string
	for _, m := range ms {
		if m.Status == Ignored {
			out = append(out, m.Path)
		}
	}
	return out
}

// Link maps every root to a service. prev holds the stored mappings: user decisions and earlier automatic matches are
// kept (a service that is quiet for a while does not lose its folder); only new or unmatched folders are matched
// against known, the service and app names with telemetry (nil or empty: none can be matched).
func Link(roots []extract.Root, prev []Mapping, known map[string]bool) []Mapping {
	byPath := map[string]Mapping{}
	for _, m := range prev {
		byPath[m.Path] = m
	}
	out := make([]Mapping, 0, len(roots))
	seen := map[string]bool{}
	for _, r := range roots {
		seen[r.Path] = true
		m := Mapping{Path: r.Path, Detected: r.Detected, Evidence: r.Evidence, Kind: r.Kind, Items: r.Items}
		old, had := byPath[r.Path]
		switch {
		case had && (old.Decided() || (old.Status == Matched && old.Service != "")):
			m.Service, m.Status = old.Service, old.Status
		default:
			if svc := Best(r.Detected, known); svc != "" {
				m.Service, m.Status = svc, Matched
			} else {
				m.Status = Unmatched
			}
		}
		out = append(out, m)
	}
	// user decisions for folders that no longer have items stay, so they apply again when the folder comes back
	for _, m := range prev {
		if !seen[m.Path] && m.Decided() {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// suffixes that commonly differ between a folder / package name and the service name it reports as.
var suffixes = []string{"service", "server", "backend", "frontend", "worker", "svc", "api", "app", "web"}

// norm lowercases and drops separators: "Orders_API" → "ordersapi".
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stem is norm without one common suffix: "orders-api" → "orders".
func stem(s string) string {
	n := norm(s)
	for _, suf := range suffixes {
		if len(n) > len(suf) && strings.HasSuffix(n, suf) {
			return strings.TrimSuffix(n, suf)
		}
	}
	return n
}

// tokens splits a name on separators and case: "tt-api" → [tt api].
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') })
}

// tail reports whether the tokens of a end with all tokens of b ("tt-api" ends with "api").
func tail(a, b []string) bool {
	if len(b) == 0 || len(b) >= len(a) {
		return false
	}
	for i := range b {
		if a[len(a)-len(b)+i] != b[i] {
			return false
		}
	}
	return true
}

// Best returns the telemetry service a detected name refers to: the exact name, else the only service with the same
// normalised name, else the only one with the same stem, else the only one whose name ends with the other's words
// (a prefix like "tt-api" for folder "api"). Ambiguous or no match → "".
func Best(detected string, known map[string]bool) string {
	if detected == "" || len(known) == 0 {
		return ""
	}
	if known[detected] {
		return detected
	}
	pick := func(key func(string) string) string {
		want, found := key(detected), ""
		if want == "" {
			return ""
		}
		for k := range known {
			if key(k) != want {
				continue
			}
			if found != "" {
				return "" // two candidates: let the user decide
			}
			found = k
		}
		return found
	}
	if s := pick(norm); s != "" {
		return s
	}
	if s := pick(stem); s != "" {
		return s
	}
	dt, found := tokens(detected), ""
	for k := range known {
		kt := tokens(k)
		if !tail(kt, dt) && !tail(dt, kt) {
			continue
		}
		if found != "" {
			return ""
		}
		found = k
	}
	return found
}

// Changed reports whether the service of any folder differs between two mapping sets (an incremental analysis then
// leaves stored items of untouched files under the old name, so a full scan is needed).
func Changed(prev, next []Mapping) bool {
	svc := func(m Mapping) string {
		if m.Status == Ignored {
			return "\x00ignored"
		}
		if m.Service != "" {
			return m.Service
		}
		return m.Detected
	}
	old := map[string]string{}
	for _, m := range prev {
		old[m.Path] = svc(m)
	}
	for _, m := range next {
		if o, ok := old[m.Path]; ok && o != svc(m) {
			return true
		}
	}
	return false
}
