package gw

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var jsonUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// isJSON decides the payload format from Content-Type; def is used when the header is missing.
func isJSON(contentType string, def bool) bool {
	ct := strings.ToLower(contentType)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "application/json", "text/plain": // text/plain: navigator.sendBeacon with a string body
		return true
	case "application/x-protobuf", "application/protobuf", "application/vnd.google.protobuf", "application/octet-stream":
		return false
	}
	return def
}

// unmarshalOTLPJSON decodes OTLP/JSON. OTLP/JSON encodes trace/span/profile ids as hex while protojson expects
// base64 for bytes fields, so ids are rewritten first.
func unmarshalOTLPJSON(body []byte, m proto.Message) error {
	if needsIDFix(body) {
		fixed, err := fixHexIDs(body)
		if err != nil {
			return err
		}
		body = fixed
	}
	return jsonUnmarshal.Unmarshal(body, m)
}

var idKeys = [][]byte{[]byte(`"traceId"`), []byte(`"spanId"`), []byte(`"parentSpanId"`), []byte(`"trace_id"`), []byte(`"span_id"`), []byte(`"parent_span_id"`), []byte(`"profileId"`), []byte(`"profile_id"`)}

func needsIDFix(b []byte) bool {
	for _, k := range idKeys {
		if bytes.Contains(b, k) {
			return true
		}
	}
	return false
}

func fixHexIDs(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	walkIDs(v)
	return json.Marshal(v)
}

func walkIDs(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			switch k {
			case "traceId", "spanId", "parentSpanId", "trace_id", "span_id", "parent_span_id", "profileId", "profile_id":
				if s, ok := e.(string); ok && (len(s) == 32 || len(s) == 16) {
					if b, err := hex.DecodeString(s); err == nil {
						x[k] = base64.StdEncoding.EncodeToString(b)
					}
				}
			default:
				walkIDs(e)
			}
		}
	case []any:
		for _, e := range x {
			walkIDs(e)
		}
	}
}
