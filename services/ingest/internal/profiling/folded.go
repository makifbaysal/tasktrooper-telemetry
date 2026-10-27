package profiling

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// parseFolded reads "frame1;frame2;…;frameN <value>" lines (root first). A "# trooper.route=<r> trooper.method=<M>" line
// tags the following lines with an endpoint until the next "#" line; "#" alone clears the tag.
func parseFolded(raw []byte, o Options) ([]Series, error) {
	if !ValidType(o.Type) {
		return nil, fmt.Errorf("folded: unknown type %q", o.Type)
	}
	unit := o.Unit
	if unit == "" {
		unit = DefaultUnit(o.Type)
	}
	agg := newAggregator(o.Type, unit)
	var route, method string
	var frames []string
	for lineNo := 1; len(raw) > 0; lineNo++ {
		var line []byte
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i], raw[i+1:]
		} else {
			line, raw = raw, nil
		}
		line = bytes.TrimRight(line, "\r \t")
		if len(line) == 0 {
			continue
		}
		if line[0] == '#' {
			route, method = sectionLabels(string(line[1:]))
			continue
		}
		sp := bytes.LastIndexAny(line, " \t")
		if sp <= 0 {
			return nil, fmt.Errorf("folded line %d: missing value", lineNo)
		}
		v, err := strconv.ParseInt(string(line[sp+1:]), 10, 64)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("folded line %d: invalid value %q", lineNo, line[sp+1:])
		}
		frames = frames[:0]
		for f := range strings.SplitSeq(string(bytes.TrimRight(line[:sp], " \t")), ";") {
			if f = strings.TrimSpace(f); f != "" {
				frames = append(frames, cleanFrame(f))
			}
		}
		agg.add(route, method, frames, v)
	}
	return []Series{agg.series()}, nil
}

// sectionLabels parses the text after '#': whitespace-separated key=value pairs; unknown keys are ignored.
func sectionLabels(s string) (route, method string) {
	for f := range strings.FieldsSeq(s) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case LabelRoute:
			route = capString(v, MaxLabelLen)
		case LabelMethod:
			method = strings.ToUpper(capString(v, MaxLabelLen))
		}
	}
	return route, method
}
