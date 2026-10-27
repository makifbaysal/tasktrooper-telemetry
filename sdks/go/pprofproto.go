package trooper

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// Minimal decoder for the pprof profile.proto format written by runtime/pprof. It keeps only what
// the profiler needs to turn cumulative profiles (allocs, mutex, block) into per-period deltas:
// sample types, and per sample the function-name stack (leaf first), values and string labels.
// Field numbers: github.com/google/pprof/proto/profile.proto.

type pprofSample struct {
	stack  []string // leaf first
	values []int64
	labels map[string]string
}

type pprofProfile struct {
	sampleTypes []string // "type/unit"
	samples     []pprofSample
}

const maxProfileSize = 64 << 20 // decompressed

// decodePprof decodes a (possibly gzipped) pprof profile.
func decodePprof(data []byte) (*pprofProfile, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(zr, maxProfileSize+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxProfileSize {
			return nil, errors.New("profile too large")
		}
		data = raw
	}

	type rawSample struct {
		locs   []uint64
		values []int64
		labels [][2]int64 // key, str string index
	}
	type rawLocation struct{ funcs []uint64 } // function ids, leaf (innermost inlined) first
	var (
		strs      []string
		valTypes  [][2]int64
		samples   []rawSample
		locations = map[uint64]rawLocation{}
		functions = map[uint64]int64{} // id -> name string index
	)

	err := forEachField(data, func(num protowire.Number, typ protowire.Type, v uint64, b []byte) error {
		switch num {
		case 1: // sample_type
			var vt [2]int64
			if err := forEachField(b, func(n protowire.Number, _ protowire.Type, v uint64, _ []byte) error {
				if n == 1 || n == 2 {
					vt[n-1] = int64(v)
				}
				return nil
			}); err != nil {
				return err
			}
			valTypes = append(valTypes, vt)
		case 2: // sample
			var s rawSample
			if err := forEachField(b, func(n protowire.Number, t protowire.Type, v uint64, bb []byte) error {
				switch n {
				case 1:
					return appendVarints(t, v, bb, func(x uint64) { s.locs = append(s.locs, x) })
				case 2:
					return appendVarints(t, v, bb, func(x uint64) { s.values = append(s.values, int64(x)) })
				case 3:
					var kv [2]int64
					err := forEachField(bb, func(n protowire.Number, _ protowire.Type, v uint64, _ []byte) error {
						if n == 1 || n == 2 {
							kv[n-1] = int64(v)
						}
						return nil
					})
					if err == nil && kv[1] != 0 {
						s.labels = append(s.labels, kv)
					}
					return err
				}
				return nil
			}); err != nil {
				return err
			}
			samples = append(samples, s)
		case 4: // location
			var id uint64
			var loc rawLocation
			if err := forEachField(b, func(n protowire.Number, _ protowire.Type, v uint64, bb []byte) error {
				switch n {
				case 1:
					id = v
				case 4: // line
					return forEachField(bb, func(n protowire.Number, _ protowire.Type, v uint64, _ []byte) error {
						if n == 1 {
							loc.funcs = append(loc.funcs, v)
						}
						return nil
					})
				}
				return nil
			}); err != nil {
				return err
			}
			locations[id] = loc
		case 5: // function
			var id uint64
			var name int64
			if err := forEachField(b, func(n protowire.Number, _ protowire.Type, v uint64, _ []byte) error {
				switch n {
				case 1:
					id = v
				case 2:
					name = int64(v)
				}
				return nil
			}); err != nil {
				return err
			}
			functions[id] = name
		case 6: // string_table
			strs = append(strs, string(b))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	str := func(i int64) string {
		if i < 0 || int(i) >= len(strs) {
			return ""
		}
		return strs[i]
	}
	p := &pprofProfile{}
	for _, vt := range valTypes {
		p.sampleTypes = append(p.sampleTypes, str(vt[0])+"/"+str(vt[1]))
	}
	p.samples = make([]pprofSample, 0, len(samples))
	for _, rs := range samples {
		s := pprofSample{values: rs.values}
		for _, l := range rs.locs {
			for _, f := range locations[l].funcs {
				s.stack = append(s.stack, str(functions[f]))
			}
		}
		if len(rs.labels) > 0 {
			s.labels = make(map[string]string, len(rs.labels))
			for _, kv := range rs.labels {
				s.labels[str(kv[0])] = str(kv[1])
			}
		}
		p.samples = append(p.samples, s)
	}
	return p, nil
}

// sampleIndex returns the index of sample type "type/unit", or -1.
func (p *pprofProfile) sampleIndex(typeUnit string) int {
	for i, t := range p.sampleTypes {
		if t == typeUnit {
			return i
		}
	}
	return -1
}

// forEachField iterates the top-level fields of a protobuf message. For varint fields v holds the
// value; for length-delimited fields b holds the bytes. Fixed32/64 values are passed in v.
func forEachField(b []byte, fn func(num protowire.Number, typ protowire.Type, v uint64, b []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var (
			v  uint64
			bb []byte
		)
		switch typ {
		case protowire.VarintType:
			v, n = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			bb, n = protowire.ConsumeBytes(b)
		case protowire.Fixed32Type:
			var x uint32
			x, n = protowire.ConsumeFixed32(b)
			v = uint64(x)
		case protowire.Fixed64Type:
			v, n = protowire.ConsumeFixed64(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(num, typ, v, bb); err != nil {
			return err
		}
	}
	return nil
}

// appendVarints handles both packed and unpacked repeated varint fields.
func appendVarints(typ protowire.Type, v uint64, b []byte, add func(uint64)) error {
	if typ == protowire.VarintType {
		add(v)
		return nil
	}
	for len(b) > 0 {
		x, n := protowire.ConsumeVarint(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		add(x)
		b = b[n:]
	}
	return nil
}

// foldedFrame makes a function name safe for the folded format (no ';' or line breaks).
func foldedFrame(s string) string {
	if s == "" {
		return "[unknown]"
	}
	if strings.ContainsAny(s, ";\n\r") {
		s = strings.NewReplacer(";", ":", "\n", " ", "\r", " ").Replace(s)
	}
	return s
}

// foldedKey returns the folded stack (root first) of a leaf-first stack.
func foldedKey(stack []string) string {
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteString(foldedFrame(stack[i]))
		if i > 0 {
			b.WriteByte(';')
		}
	}
	return b.String()
}
