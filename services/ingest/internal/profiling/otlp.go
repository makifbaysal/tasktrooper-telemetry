package profiling

import (
	"fmt"
	"math"
	"path"
	"strings"
	"time"

	colprofilespb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/protobuf/proto"
)

// OTLP profiles (contracts §24 "OTLP profiles"): an ExportProfilesServiceRequest (proto
// go.opentelemetry.io/proto/otlp/{collector/,}profiles/v1development v0.4.1 — the dictionary-based layout the
// Collector's pdata/pprofile and otlpexporter emit) is opened per ResourceProfiles → ScopeProfiles → Profile into the
// same Series as pprof: one Series per Profile (OTLP profiles carry a single sample type each).

// FormatOTLP is the tt.profiles format header of an OTLP request.
const FormatOTLP = "otlp"

// UnknownService is the service of a resource without service.name (OTel's default, suffixed with
// ":<process.executable.name>" when the resource has it).
const UnknownService = "unknown_service"

// maxOTLPWindow bounds a profile's time window (the /v1/profiles limit).
const maxOTLPWindow = time.Hour

// OTLPProfile is one converted Profile with its resource identity and window.
type OTLPProfile struct {
	Service, Env, Version, Host, Runtime string
	Start, End                           time.Time
	Series                               Series
}

// OTLPResult is the conversion of one request.
type OTLPResult struct {
	Profiles []OTLPProfile
	// Counters for the writer metrics.
	NoService   int // resources without service.name (converted under UnknownService)
	Unsupported int // profiles whose sample type has no contracts §24 mapping (skipped)
	Empty       int // profiles without a non-zero sample (skipped)
}

// DecodeOTLP gunzips (magic-detected, bounded by max) and unmarshals a tt.profiles OTLP value.
func DecodeOTLP(body []byte, max int64) (*colprofilespb.ExportProfilesServiceRequest, error) {
	if max <= 0 {
		max = 32 << 20
	}
	raw, err := Gunzip(body, max)
	if err != nil {
		return nil, err
	}
	req := &colprofilespb.ExportProfilesServiceRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		return nil, fmt.Errorf("otlp profiles: %w", err)
	}
	return req, nil
}

// RuntimeFromOTel maps process.runtime.name to a contracts §24 runtime ("" when unknown).
func RuntimeFromOTel(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case n == "":
		return ""
	case n == "go" || n == "gc" || n == "golang":
		return "go"
	case n == "java" || n == "jvm" || strings.Contains(n, "jdk") || strings.Contains(n, "java") || strings.Contains(n, "jre"):
		return "jvm" // the Java SDK reports "OpenJDK Runtime Environment"
	case n == "node" || n == "nodejs" || n == "node.js":
		return "node"
	case n == "dotnet" || strings.HasPrefix(n, ".net"): // ".NET", ".NET Framework"
		return "dotnet"
	case n == "cpython" || n == "python" || n == "pypy":
		return "python"
	}
	return ""
}

// otlpConv holds the request-level dictionary and per-runtime location caches. Index 0 of every table is looked up like
// any other: the spec reserves it for the zero value (which resolves to nothing), but Collector 0.164's pprof conversion
// stores real stacks / locations there.
type otlpConv struct {
	d       *profilespb.ProfilesDictionary
	strs    []string
	locs    [2][][]string // [node?][location index] → frames, root first (nil = not computed)
	stack   []string
	routeK  int32 // string index of trooper.route (-1 = absent)
	methodK int32
}

func (c *otlpConv) str(i int32) string {
	if i < 0 || int(i) >= len(c.strs) {
		return ""
	}
	return c.strs[i]
}

// ConvertOTLP opens a request. now stands in for a missing profile time.
func ConvertOTLP(req *colprofilespb.ExportProfilesServiceRequest, now time.Time) OTLPResult {
	var res OTLPResult
	d := req.GetDictionary()
	c := &otlpConv{d: d, strs: d.GetStringTable(), routeK: -1, methodK: -1}
	for i, s := range c.strs {
		switch s {
		case LabelRoute:
			c.routeK = int32(i)
		case LabelMethod:
			c.methodK = int32(i)
		}
	}
	for _, rp := range req.GetResourceProfiles() {
		id := c.resourceIdentity(rp.GetResource().GetAttributes())
		if id.noService {
			res.NoService++
		}
		// pprof-converted CPU profiles come as a (samples, count) and a (cpu, nanoseconds) Profile: the count fallback is
		// only used when the resource has no CPU-time profile for the same timestamp.
		cpuAt := map[uint64]bool{}
		for _, sp := range rp.GetScopeProfiles() {
			for _, p := range sp.GetProfiles() {
				if st := p.GetSampleType(); mapSampleType(c.str(st.GetTypeStrindex()), c.str(st.GetUnitStrindex()), false) == "cpu" {
					cpuAt[p.GetTimeUnixNano()] = true
				}
			}
		}
		for _, sp := range rp.GetScopeProfiles() {
			for _, p := range sp.GetProfiles() {
				st := p.GetSampleType()
				typ, scale := mapSampleType(c.str(st.GetTypeStrindex()), c.str(st.GetUnitStrindex()), false), int64(1)
				if typ == "" && isSamplesCount(c.str(st.GetTypeStrindex()), c.str(st.GetUnitStrindex())) && p.GetPeriod() > 0 &&
					strings.EqualFold(c.str(p.GetPeriodType().GetTypeStrindex()), "cpu") &&
					strings.EqualFold(c.str(p.GetPeriodType().GetUnitStrindex()), "nanoseconds") && !cpuAt[p.GetTimeUnixNano()] {
					typ, scale = "cpu", p.GetPeriod()
				}
				if typ == "" {
					res.Unsupported++
					continue
				}
				s := c.profile(p, typ, scale, id.runtime)
				if len(s.Stacks) == 0 {
					res.Empty++
					continue
				}
				start, end := window(p, now)
				res.Profiles = append(res.Profiles, OTLPProfile{Service: id.service, Env: id.env, Version: id.version, Host: id.host,
					Runtime: id.runtime, Start: start, End: end, Series: s})
			}
		}
	}
	return res
}

func isSamplesCount(typ, unit string) bool {
	return strings.EqualFold(typ, "samples") && strings.EqualFold(unit, "count")
}

type identity struct {
	service, env, version, host, runtime string
	noService                            bool
}

func (c *otlpConv) resourceIdentity(attrs []*commonpb.KeyValue) identity {
	var id identity
	var exe, envOld string
	for _, kv := range attrs {
		v := c.strValue(kv.GetValue())
		switch c.key(kv) {
		case "service.name":
			id.service = v
		case "deployment.environment.name":
			id.env = v
		case "deployment.environment":
			envOld = v
		case "service.version":
			id.version = v
		case "host.name":
			id.host = v
		case "process.runtime.name":
			id.runtime = RuntimeFromOTel(v)
		case "process.executable.name":
			exe = v
		}
	}
	if id.env == "" {
		id.env = envOld
	}
	if strings.TrimSpace(id.service) == "" {
		id.noService = true
		id.service = UnknownService
		if exe != "" {
			id.service += ":" + exe
		}
	}
	id.service = capString(strings.TrimSpace(id.service), MaxLabelLen)
	id.env, id.version, id.host = capString(id.env, MaxLabelLen), capString(id.version, MaxLabelLen), capString(id.host, MaxLabelLen)
	return id
}

// window is [time_unix_nano, + duration_nano]; missing parts come from the sample timestamps (else now), ≤ 1 hour.
func window(p *profilespb.Profile, now time.Time) (time.Time, time.Time) {
	startNs, dur := p.GetTimeUnixNano(), p.GetDurationNano()
	if startNs == 0 || dur == 0 {
		var lo, hi uint64
		for _, s := range p.GetSamples() {
			for _, ts := range s.GetTimestampsUnixNano() {
				if lo == 0 || ts < lo {
					lo = ts
				}
				hi = max(hi, ts)
			}
		}
		if startNs == 0 {
			startNs = lo
		}
		if dur == 0 && hi > startNs {
			dur = hi - startNs
		}
	}
	if startNs == 0 || startNs > math.MaxInt64 {
		return now.UTC(), now.UTC()
	}
	start := time.Unix(0, int64(startNs)).UTC()
	d := time.Duration(min(dur, uint64(maxOTLPWindow)))
	return start, start.Add(d)
}

// profile aggregates one Profile's samples.
func (c *otlpConv) profile(p *profilespb.Profile, typ string, scale int64, runtime string) Series {
	agg := newAggregator(typ, typeUnits[typ])
	node := runtime == "node"
	namer := newFrameNamer(runtime)
	pRoute, pMethod := c.labels(p.GetAttributeIndices(), "", "")
	stacks := c.d.GetStackTable()
	for _, s := range p.GetSamples() {
		var v int64
		if vals := s.GetValues(); len(vals) > 0 {
			for _, x := range vals {
				v += x
			}
		} else {
			v = int64(len(s.GetTimestampsUnixNano()))
		}
		if v <= 0 {
			continue
		}
		si := s.GetStackIndex()
		if si < 0 || int(si) >= len(stacks) {
			continue
		}
		c.stack = c.stack[:0]
		li := stacks[si].GetLocationIndices()
		for i := len(li) - 1; i >= 0; i-- { // leaf first on the wire → root first
			c.stack = append(c.stack, c.location(li[i], node, namer)...)
		}
		route, method := c.labels(s.GetAttributeIndices(), pRoute, pMethod)
		agg.add(route, method, c.stack, v*scale)
	}
	return agg.series()
}

// labels reads trooper.route / trooper.method from attribute_table entries (defaults when absent).
func (c *otlpConv) labels(idx []int32, route, method string) (string, string) {
	if c.routeK < 0 && c.methodK < 0 {
		return route, method
	}
	at := c.d.GetAttributeTable()
	for _, i := range idx {
		if i < 0 || int(i) >= len(at) {
			continue
		}
		kv := at[i]
		switch kv.GetKeyStrindex() {
		case c.routeK:
			route = capString(c.strValue(kv.GetValue()), MaxLabelLen)
		case c.methodK:
			method = strings.ToUpper(capString(c.strValue(kv.GetValue()), MaxLabelLen))
		}
	}
	return route, method
}

// location returns the frames of a location root first (lines list inlined callees first, the caller last).
func (c *otlpConv) location(i int32, node bool, namer frameNamer) []string {
	locs := c.d.GetLocationTable()
	if i < 0 || int(i) >= len(locs) {
		return []string{"[unknown]"}
	}
	k := 0
	if node {
		k = 1
	}
	if c.locs[k] == nil {
		c.locs[k] = make([][]string, len(locs))
	}
	if fr := c.locs[k][i]; fr != nil {
		return fr
	}
	loc := locs[i]
	var fr []string
	if lines := loc.GetLines(); len(lines) == 0 {
		fr = []string{"[unknown]"}
		if ms := c.d.GetMappingTable(); loc.GetMappingIndex() >= 0 && int(loc.GetMappingIndex()) < len(ms) {
			if f := c.str(ms[loc.GetMappingIndex()].GetFilenameStrindex()); f != "" {
				fr = []string{cleanFrame("[" + path.Base(f) + "]")}
			}
		}
	} else {
		fns := c.d.GetFunctionTable()
		fr = make([]string, 0, len(lines))
		for j := len(lines) - 1; j >= 0; j-- {
			fi := lines[j].GetFunctionIndex()
			if fi < 0 || int(fi) >= len(fns) {
				fr = append(fr, namer.name("", "", ""))
				continue
			}
			f := fns[fi]
			fr = append(fr, namer.name(c.str(f.GetNameStrindex()), c.str(f.GetSystemNameStrindex()), c.str(f.GetFilenameStrindex())))
		}
	}
	c.locs[k][i] = fr
	return fr
}

// key is a resource attribute's key: inline or, in profiles (OTLP ≥ 1.10, Collector ≥ 0.14x), key_strindex into the
// dictionary's string_table.
func (c *otlpConv) key(kv *commonpb.KeyValue) string {
	if k := kv.GetKey(); k != "" {
		return k
	}
	return c.str(kv.GetKeyStrindex())
}

// strValue is a string AnyValue, inline or as string_value_strindex.
func (c *otlpConv) strValue(v *commonpb.AnyValue) string {
	if x, ok := v.GetValue().(*commonpb.AnyValue_StringValueStrindex); ok {
		return c.str(x.StringValueStrindex)
	}
	return v.GetStringValue()
}
