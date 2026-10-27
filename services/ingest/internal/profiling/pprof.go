package profiling

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/google/pprof/profile"
)

// Sample labels that tag a stack with its endpoint (Go SDK: pprof.Do / pprof.Labels).
const (
	LabelRoute  = "trooper.route"
	LabelMethod = "trooper.method"
)

// ErrNoSampleTypes means the pprof profile has none of the supported sample types.
var ErrNoSampleTypes = errors.New("pprof: no supported sample type (cpu, wall, alloc_*, inuse_*, goroutine, delay)")

// mapSampleType maps a pprof sample type to a contracts §24 type ("" = skipped).
func mapSampleType(typ, unit string, blockHint bool) string {
	switch strings.ToLower(typ) + "/" + strings.ToLower(unit) {
	case "cpu/nanoseconds":
		return "cpu"
	case "wall/nanoseconds":
		return "wall"
	case "alloc_space/bytes":
		return "alloc_space"
	case "alloc_objects/count":
		return "alloc_objects"
	case "inuse_space/bytes":
		return "inuse_space"
	case "inuse_objects/count":
		return "inuse_objects"
	case "goroutine/count", "goroutines/count":
		return "goroutines"
	case "delay/nanoseconds":
		if blockHint {
			return "block"
		}
		return "lock"
	}
	return "" // samples/count, contentions/count, …
}

func parsePprof(raw []byte, o Options) ([]Series, error) {
	p, err := profile.ParseUncompressed(raw)
	if err != nil {
		return nil, fmt.Errorf("pprof: %w", err)
	}
	blockHint := o.Type == "block"

	type col struct {
		idx   int
		agg   *aggregator
		scale int64 // samples/count × period (cpu fallback)
	}
	var cols []col
	seen := map[string]bool{}
	for i, st := range p.SampleType {
		if t := mapSampleType(st.Type, st.Unit, blockHint); t != "" && !seen[t] {
			seen[t] = true
			cols = append(cols, col{idx: i, agg: newAggregator(t, typeUnits[t]), scale: 1})
		}
	}
	// CPU profiles without cpu/nanoseconds (some converters): samples/count × period when the period is CPU time.
	if !seen["cpu"] && p.PeriodType != nil && strings.EqualFold(p.PeriodType.Type, "cpu") &&
		strings.EqualFold(p.PeriodType.Unit, "nanoseconds") && p.Period > 0 {
		for i, st := range p.SampleType {
			if strings.EqualFold(st.Type, "samples") && strings.EqualFold(st.Unit, "count") {
				cols = append(cols, col{idx: i, agg: newAggregator("cpu", "nanoseconds"), scale: p.Period})
				break
			}
		}
	}
	if len(cols) == 0 {
		return nil, ErrNoSampleTypes
	}

	names := newFrameNamer(o.Runtime)
	locFrames := make(map[uint64][]string, len(p.Location)) // location id → frames, root first
	var stack []string
	for _, s := range p.Sample {
		stack = stack[:0]
		for i := len(s.Location) - 1; i >= 0; i-- {
			loc := s.Location[i]
			fr, ok := locFrames[loc.ID]
			if !ok {
				fr = names.location(loc)
				locFrames[loc.ID] = fr
			}
			stack = append(stack, fr...)
		}
		route, method := firstLabel(s, LabelRoute), strings.ToUpper(firstLabel(s, LabelMethod))
		for _, c := range cols {
			if c.idx < len(s.Value) {
				c.agg.add(route, method, stack, s.Value[c.idx]*c.scale)
			}
		}
	}
	out := make([]Series, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.agg.series())
	}
	return out, nil
}

func firstLabel(s *profile.Sample, k string) string {
	if v := s.Label[k]; len(v) > 0 {
		return capString(v[0], MaxLabelLen)
	}
	return ""
}

// frameNamer names frames per runtime (contracts §24): Go pkg.(*T).Method, JVM com.acme.Foo.bar,
// Node "fn (path/to/file.js)" / "(anonymous) (file)". File and line are never kept otherwise.
type frameNamer struct{ node bool }

func newFrameNamer(runtime string) frameNamer { return frameNamer{node: runtime == "node"} }

// location returns the frames of a location root first (pprof lists inlined callees first).
func (n frameNamer) location(loc *profile.Location) []string {
	if len(loc.Line) == 0 {
		if loc.Mapping != nil && loc.Mapping.File != "" {
			return []string{"[" + path.Base(loc.Mapping.File) + "]"}
		}
		return []string{"[unknown]"}
	}
	out := make([]string, 0, len(loc.Line))
	for _, ln := range slices.Backward(loc.Line) {
		out = append(out, n.function(ln.Function))
	}
	return out
}

func (n frameNamer) function(f *profile.Function) string {
	if f == nil {
		return "[unknown]"
	}
	return n.name(f.Name, f.SystemName, f.Filename)
}

// name builds a frame name from a function's name, system name and file (shared by pprof and OTLP).
func (n frameNamer) name(name, systemName, filename string) string {
	if name == "" {
		name = systemName
	}
	if n.node {
		if name == "" {
			name = "(anonymous)"
		}
		if file := strings.TrimPrefix(filename, "file://"); file != "" {
			name += " (" + file + ")"
		}
	} else if name == "" {
		name = "[unknown]"
	}
	return cleanFrame(name)
}
