// Package flatten turns decoded OTLP / RUM payloads into ClickHouse rows that match
// deploy/clickhouse/001_init.sql column for column. It is pure (no I/O) so it can be unit-tested and benchmarked.
package flatten

import (
	"encoding/base64"
	"encoding/hex"
	"math"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Attribute map caps (cardinality and row-size discipline).
const (
	MaxAttrKeys     = 64
	MaxAttrValueLen = 1024
	maxStackLen     = 16 << 10
)

// attrIndex is a small read-only view over OTLP key/values. Linear scan beats a map for the
// typical 5–30 attributes per span and allocates nothing.
type attrIndex []*commonpb.KeyValue

func (a attrIndex) get(key string) *commonpb.AnyValue {
	for _, kv := range a {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return nil
}

func (a attrIndex) str(key string) string {
	v := a.get(key)
	if v == nil {
		return ""
	}
	if s, ok := v.GetValue().(*commonpb.AnyValue_StringValue); ok {
		return s.StringValue
	}
	return anyString(v)
}

// first returns the first non-empty string among keys.
func (a attrIndex) first(keys ...string) string {
	for _, k := range keys {
		if s := a.str(k); s != "" {
			return s
		}
	}
	return ""
}

func (a attrIndex) int(key string) (int64, bool) {
	v := a.get(key)
	if v == nil {
		return 0, false
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_IntValue:
		return x.IntValue, true
	case *commonpb.AnyValue_DoubleValue:
		return int64(x.DoubleValue), true
	case *commonpb.AnyValue_StringValue:
		n, err := strconv.ParseInt(strings.TrimSpace(x.StringValue), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (a attrIndex) float(key string) (float64, bool) {
	v := a.get(key)
	if v == nil {
		return 0, false
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue, true
	case *commonpb.AnyValue_IntValue:
		return float64(x.IntValue), true
	case *commonpb.AnyValue_StringValue:
		f, err := strconv.ParseFloat(strings.TrimSpace(x.StringValue), 64)
		return f, err == nil
	}
	return 0, false
}

func (a attrIndex) bool(key string) bool {
	v := a.get(key)
	if v == nil {
		return false
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_StringValue:
		b, _ := strconv.ParseBool(x.StringValue)
		return b
	case *commonpb.AnyValue_IntValue:
		return x.IntValue != 0
	}
	return false
}

// anyString renders an AnyValue as a string (arrays/maps as compact JSON-ish text).
func anyString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case nil:
		return ""
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range x.ArrayValue.GetValues() {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONValue(&b, e)
		}
		b.WriteByte(']')
		return b.String()
	case *commonpb.AnyValue_KvlistValue:
		var b strings.Builder
		b.WriteByte('{')
		for i, kv := range x.KvlistValue.GetValues() {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Quote(kv.GetKey()))
			b.WriteByte(':')
			writeJSONValue(&b, kv.GetValue())
		}
		b.WriteByte('}')
		return b.String()
	}
	return ""
}

func writeJSONValue(b *strings.Builder, v *commonpb.AnyValue) {
	switch v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue, *commonpb.AnyValue_BytesValue:
		b.WriteString(strconv.Quote(anyString(v)))
	case nil:
		b.WriteString("null")
	default:
		b.WriteString(anyString(v))
	}
}

// attrMap converts key/values into a capped map with room for extra keys.
func attrMap(kvs []*commonpb.KeyValue, extra int) map[string]string {
	n := min(len(kvs), MaxAttrKeys)
	m := make(map[string]string, n+extra)
	for _, kv := range kvs {
		if len(m) >= MaxAttrKeys {
			break
		}
		k := kv.GetKey()
		if k == "" {
			continue
		}
		m[k] = capValue(anyString(kv.GetValue()), MaxAttrValueLen)
	}
	return m
}

// putAttr sets k=v respecting the caps (an existing key is always overwritten).
func putAttr(m map[string]string, k, v string, limit int) {
	if v == "" {
		return
	}
	if _, ok := m[k]; !ok && len(m) >= MaxAttrKeys {
		return
	}
	m[k] = capValue(v, limit)
}

// capValue truncates s to at most limit bytes without splitting a UTF-8 sequence.
func capValue(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := limit
	for i > 0 && i > limit-4 && s[i]&0xC0 == 0x80 {
		i--
	}
	return s[:i]
}

var (
	zeroTrace = make([]byte, 16)
	zeroSpan  = make([]byte, 8)
)

// fixedID returns b if it has exactly n bytes, a zero id if empty, else a left-padded/truncated copy.
func fixedID(b []byte, n int) []byte {
	if len(b) == n {
		return b
	}
	if len(b) == 0 {
		if n == 16 {
			return zeroTrace
		}
		return zeroSpan
	}
	out := make([]byte, n)
	if len(b) > n {
		copy(out, b[len(b)-n:])
	} else {
		copy(out[n-len(b):], b)
	}
	return out
}

// hexID decodes a hex trace/span id from RUM payloads ("" or invalid → zero id).
func hexID(s string, n int) []byte {
	if len(s) != n*2 {
		return fixedID(nil, n)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return fixedID(nil, n)
	}
	return b
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
