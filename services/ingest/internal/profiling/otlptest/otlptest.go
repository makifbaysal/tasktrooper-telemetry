// Package otlptest builds OTLP profiles requests (dictionary layout of profiles/v1development) for tests.
package otlptest

import (
	colprofilespb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Builder fills a request-level ProfilesDictionary (index 0 of every table is the zero value, as the spec requires).
type Builder struct {
	D    *profilespb.ProfilesDictionary
	strs map[string]int32
	fns  map[string]int32
	Req  *colprofilespb.ExportProfilesServiceRequest
}

// New returns an empty request with a dictionary.
func New() *Builder {
	d := &profilespb.ProfilesDictionary{
		StringTable:    []string{""},
		MappingTable:   []*profilespb.Mapping{{}},
		LocationTable:  []*profilespb.Location{{}},
		FunctionTable:  []*profilespb.Function{{}},
		AttributeTable: []*profilespb.KeyValueAndUnit{{}},
		StackTable:     []*profilespb.Stack{{}},
		LinkTable:      []*profilespb.Link{{}},
	}
	return &Builder{D: d, strs: map[string]int32{"": 0}, fns: map[string]int32{},
		Req: &colprofilespb.ExportProfilesServiceRequest{Dictionary: d}}
}

// Str interns s.
func (b *Builder) Str(s string) int32 {
	if i, ok := b.strs[s]; ok {
		return i
	}
	i := int32(len(b.D.StringTable))
	b.D.StringTable = append(b.D.StringTable, s)
	b.strs[s] = i
	return i
}

// Func interns a function (file optional).
func (b *Builder) Func(name, file string) int32 {
	k := name + "\x00" + file
	if i, ok := b.fns[k]; ok {
		return i
	}
	i := int32(len(b.D.FunctionTable))
	b.D.FunctionTable = append(b.D.FunctionTable, &profilespb.Function{NameStrindex: b.Str(name), FilenameStrindex: b.Str(file)})
	b.fns[k] = i
	return i
}

// Loc adds a location whose lines are fns (innermost inlined callee first, the caller last).
func (b *Builder) Loc(fns ...string) int32 {
	loc := &profilespb.Location{}
	for _, f := range fns {
		loc.Lines = append(loc.Lines, &profilespb.Line{FunctionIndex: b.Func(f, "")})
	}
	b.D.LocationTable = append(b.D.LocationTable, loc)
	return int32(len(b.D.LocationTable) - 1)
}

// LocFile adds a single-line location with a function file (Node frame naming).
func (b *Builder) LocFile(fn, file string) int32 {
	b.D.LocationTable = append(b.D.LocationTable, &profilespb.Location{Lines: []*profilespb.Line{{FunctionIndex: b.Func(fn, file)}}})
	return int32(len(b.D.LocationTable) - 1)
}

// NativeLoc adds an unsymbolized location in a mapping of file.
func (b *Builder) NativeLoc(file string, addr uint64) int32 {
	b.D.MappingTable = append(b.D.MappingTable, &profilespb.Mapping{FilenameStrindex: b.Str(file)})
	b.D.LocationTable = append(b.D.LocationTable, &profilespb.Location{MappingIndex: int32(len(b.D.MappingTable) - 1), Address: addr})
	return int32(len(b.D.LocationTable) - 1)
}

// Stack adds a stack of locations, leaf first (wire order).
func (b *Builder) Stack(locs ...int32) int32 {
	b.D.StackTable = append(b.D.StackTable, &profilespb.Stack{LocationIndices: locs})
	return int32(len(b.D.StackTable) - 1)
}

// Attr adds a string attribute.
func (b *Builder) Attr(k, v string) int32 {
	b.D.AttributeTable = append(b.D.AttributeTable, &profilespb.KeyValueAndUnit{KeyStrindex: b.Str(k), Value: Str(v)})
	return int32(len(b.D.AttributeTable) - 1)
}

// VT is a value type.
func (b *Builder) VT(typ, unit string) *profilespb.ValueType {
	return &profilespb.ValueType{TypeStrindex: b.Str(typ), UnitStrindex: b.Str(unit)}
}

// Resource appends a ResourceProfiles with string attributes (key, value pairs) holding profiles.
func (b *Builder) Resource(attrs []string, profiles ...*profilespb.Profile) {
	res := &resourcepb.Resource{}
	for i := 0; i+1 < len(attrs); i += 2 {
		res.Attributes = append(res.Attributes, &commonpb.KeyValue{Key: attrs[i], Value: Str(attrs[i+1])})
	}
	b.Req.ResourceProfiles = append(b.Req.ResourceProfiles, &profilespb.ResourceProfiles{Resource: res,
		ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: profiles}}})
}

// ResourceStrindex is Resource with dictionary-referenced attributes (key_strindex / string_value_strindex), the form
// Collector 0.164 emits.
func (b *Builder) ResourceStrindex(attrs []string, profiles ...*profilespb.Profile) {
	res := &resourcepb.Resource{}
	for i := 0; i+1 < len(attrs); i += 2 {
		res.Attributes = append(res.Attributes, &commonpb.KeyValue{KeyStrindex: b.Str(attrs[i]),
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: b.Str(attrs[i+1])}}})
	}
	b.Req.ResourceProfiles = append(b.Req.ResourceProfiles, &profilespb.ResourceProfiles{Resource: res,
		ScopeProfiles: []*profilespb.ScopeProfiles{{Profiles: profiles}}})
}

// Str is a string AnyValue.
func Str(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
