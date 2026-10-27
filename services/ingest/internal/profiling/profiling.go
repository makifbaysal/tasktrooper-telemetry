// Package profiling turns a continuous-profiling upload (contracts §24) — pprof (gzip'ed or not) or folded stacks —
// into aggregated stacks per profile type: identical (route, method, stack) samples are summed, stacks are capped at
// MaxDepth frames (root side cut to "[truncated]") and at MaxStacks per type (the smallest folded into "[other]").
// It is pure (no I/O) so ingest-gw (validation) and ingest-writer (rows) share it.
package profiling

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits (contracts §24, ADR-022).
const (
	MaxDepth     = 128   // frames per stack, "[truncated]" included
	MaxStacks    = 10000 // unique (route, method, stack) per profile and type before "[other]"
	MaxFrameLen  = 512   // bytes per frame name
	MaxLabelLen  = 256   // route / method
	TruncatedFrm = "[truncated]"
	OtherFrm     = "[other]"
)

// Formats.
const (
	FormatPprof  = "pprof"
	FormatFolded = "folded"
)

// Units per type.
var typeUnits = map[string]string{
	"cpu": "nanoseconds", "wall": "nanoseconds", "lock": "nanoseconds", "block": "nanoseconds",
	"alloc_space": "bytes", "inuse_space": "bytes",
	"alloc_objects": "count", "inuse_objects": "count", "goroutines": "count",
}

// ValidType reports whether t is one of the profile types of contracts §24.
func ValidType(t string) bool { _, ok := typeUnits[t]; return ok }

// DefaultUnit is the unit of a profile type ("" if unknown).
func DefaultUnit(t string) string { return typeUnits[t] }

// ErrTooLarge means the decompressed body exceeds the limit given to Parse.
var ErrTooLarge = errors.New("profile exceeds the decompressed size limit")

// Options describe an upload (the /v1/profiles query parameters that matter for parsing).
type Options struct {
	Format  string // pprof (default) | folded
	Type    string // folded: required; pprof: "block" maps delay/nanoseconds to block instead of lock
	Unit    string // folded: required (defaults to the type's unit)
	Runtime string // go | jvm | node | dotnet | python — frame naming
	MaxSize int64  // decompressed limit (0 = 32 MiB)
}

// Stack is one aggregated row.
type Stack struct {
	Route, Method string
	Frames        []string // root first
	Value         int64
}

// Series is the aggregated content of one profile type.
type Series struct {
	Type, Unit string
	Total      int64
	Stacks     []Stack
}

// IsGzip reports whether b starts with the gzip magic.
func IsGzip(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// Gunzip decompresses b when it is gzip'ed (bounded by max); otherwise returns b.
func Gunzip(b []byte, max int64) ([]byte, error) {
	if !IsGzip(b) {
		if int64(len(b)) > max {
			return nil, ErrTooLarge
		}
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var buf bytes.Buffer
	buf.Grow(int(min(max, int64(len(b))*6)))
	n, err := buf.ReadFrom(io.LimitReader(zr, max+1))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	if n > max {
		return nil, ErrTooLarge
	}
	return buf.Bytes(), nil
}

// Parse decodes body (gzip detected by magic) and aggregates it.
func Parse(body []byte, o Options) ([]Series, error) {
	if o.MaxSize <= 0 {
		o.MaxSize = 32 << 20
	}
	raw, err := Gunzip(body, o.MaxSize)
	if err != nil {
		return nil, err
	}
	// A gzip'ed pprof sent with Content-Encoding: gzip reaches the writer gzip'ed twice (gw forwards the wire bytes).
	if IsGzip(raw) {
		if raw, err = Gunzip(raw, o.MaxSize); err != nil {
			return nil, err
		}
	}
	switch o.Format {
	case "", FormatPprof:
		return parsePprof(raw, o)
	case FormatFolded:
		return parseFolded(raw, o)
	}
	return nil, fmt.Errorf("unknown format %q", o.Format)
}

// aggregator sums samples of one type.
type aggregator struct {
	typ, unit string
	idx       map[string]int
	stacks    []Stack
	total     int64
	key       []byte
}

func newAggregator(typ, unit string) *aggregator {
	return &aggregator{typ: typ, unit: unit, idx: map[string]int{}}
}

// add records value for frames (root first). frames is copied when new.
func (a *aggregator) add(route, method string, frames []string, value int64) {
	if value <= 0 || len(frames) == 0 {
		return
	}
	frames = capDepth(frames)
	k := a.key[:0]
	k = append(k, route...)
	k = append(k, 0)
	k = append(k, method...)
	for _, f := range frames {
		k = append(k, 0)
		k = append(k, f...)
	}
	a.key = k
	a.total += value
	if i, ok := a.idx[string(k)]; ok {
		a.stacks[i].Value += value
		return
	}
	a.idx[string(k)] = len(a.stacks)
	a.stacks = append(a.stacks, Stack{Route: route, Method: method, Frames: slices.Clone(frames), Value: value})
}

// capDepth keeps the leaf side: [truncated, last MaxDepth-1 frames].
func capDepth(frames []string) []string {
	if len(frames) <= MaxDepth {
		return frames
	}
	out := make([]string, MaxDepth)
	out[0] = TruncatedFrm
	copy(out[1:], frames[len(frames)-(MaxDepth-1):])
	return out
}

// series finalizes: stacks beyond MaxStacks (smallest values) are merged into "[other]" per (route, method).
func (a *aggregator) series() Series {
	s := Series{Type: a.typ, Unit: a.unit, Total: a.total, Stacks: a.stacks}
	if len(s.Stacks) <= MaxStacks {
		return s
	}
	slices.SortFunc(s.Stacks, func(x, y Stack) int {
		switch {
		case x.Value > y.Value:
			return -1
		case x.Value < y.Value:
			return 1
		}
		return 0
	})
	kept := s.Stacks[:MaxStacks]
	type rm struct{ route, method string }
	other := map[rm]int64{}
	var order []rm
	for _, st := range s.Stacks[MaxStacks:] {
		k := rm{st.Route, st.Method}
		if _, ok := other[k]; !ok {
			order = append(order, k)
		}
		other[k] += st.Value
	}
	out := make([]Stack, 0, MaxStacks+len(order))
	out = append(out, kept...)
	for _, k := range order {
		out = append(out, Stack{Route: k.route, Method: k.method, Frames: []string{OtherFrm}, Value: other[k]})
	}
	s.Stacks = out
	return s
}

// cleanFrame bounds a frame name and strips characters that would break the folded form.
func cleanFrame(s string) string {
	if strings.ContainsAny(s, ";\n\r") {
		s = strings.NewReplacer(";", ":", "\n", " ", "\r", " ").Replace(s)
	}
	return capString(s, MaxFrameLen)
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && i > n-4 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
