package norm

import (
	"strings"

	"github.com/cespare/xxhash/v2"
)

const maxFrames = 5

// ErrorFingerprint groups errors into issues: a stable 64-bit hash over service, error type and either the
// top stack frames (line/column numbers, addresses and bundle hashes masked) or, without a stack, the
// masked message. Never returns 0 (0 means "no error" in ClickHouse).
func ErrorFingerprint(service, typ, message, stack string) uint64 {
	d := xxhash.New()
	_, _ = d.WriteString(service)
	_, _ = d.Write([]byte{0})
	_, _ = d.WriteString(typ)
	_, _ = d.Write([]byte{0})
	if n := writeFrames(d, stack, typ, message); n == 0 {
		_, _ = d.WriteString(maskTokens(message, 256))
	}
	h := d.Sum64()
	if h == 0 {
		h = 1
	}
	return h
}

// writeFrames hashes up to maxFrames normalized frame lines and returns how many it wrote.
func writeFrames(d *xxhash.Digest, stack, typ, message string) int {
	if stack == "" {
		return 0
	}
	var fallback []string
	n := 0
	for line := range strings.SplitSeq(stack, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if (typ != "" && strings.HasPrefix(line, typ)) || (message != "" && strings.Contains(line, message)) {
			continue // header line "TypeError: msg" varies with the message
		}
		if !looksLikeFrame(line) {
			if len(fallback) < maxFrames {
				fallback = append(fallback, line)
			}
			continue
		}
		_, _ = d.WriteString(maskFrame(line))
		_, _ = d.Write([]byte{'\n'})
		n++
		if n == maxFrames {
			return n
		}
	}
	if n == 0 {
		for _, l := range fallback {
			_, _ = d.WriteString(maskFrame(l))
			_, _ = d.Write([]byte{'\n'})
			n++
		}
	}
	return n
}

func looksLikeFrame(l string) bool {
	return strings.HasPrefix(l, "at ") || // JS (V8), Java, .NET
		strings.HasPrefix(l, "File \"") || // Python
		strings.HasPrefix(l, "#") || // Dart, PHP
		strings.Contains(l, ".go:") || // Go file line
		strings.Contains(l, "@") || // Firefox/Safari fn@url:line:col
		strings.Contains(l, ".swift:") || strings.Contains(l, ".kt:") || strings.Contains(l, ".java:")
}

// maskFrame removes the volatile parts of a frame: digits (line/col, addresses, build hashes) and query strings.
func maskFrame(l string) string {
	if i := strings.IndexByte(l, '?'); i >= 0 {
		// "app.js?v=123:10:5" → "app.js:10:5" (masked below)
		j := i
		for j < len(l) && l[j] != ':' && l[j] != ')' && l[j] != ' ' {
			j++
		}
		l = l[:i] + l[j:]
	}
	b := make([]byte, 0, len(l))
	inNum := false
	for i := 0; i < len(l); i++ {
		c := l[i]
		if isDigit(c) || (inNum && isHex(c)) || (c == 'x' && i > 0 && l[i-1] == '0') {
			if !inNum {
				b = append(b, '?')
				inNum = true
			}
			continue
		}
		inNum = false
		b = append(b, c)
	}
	return string(b)
}

// maskTokens masks whitespace-separated tokens that contain digits or look like ids/quoted values,
// keeping at most limit bytes of input.
func maskTokens(s string, limit int) string {
	if len(s) > limit {
		s = s[:limit]
	}
	var b strings.Builder
	b.Grow(len(s))
	first := true
	for tok := range strings.FieldsSeq(s) {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		b.WriteString(maskToken(tok))
	}
	return b.String()
}

func maskToken(tok string) string {
	if len(tok) >= 2 && (tok[0] == '"' || tok[0] == '\'' || tok[0] == '`') {
		return "<*>"
	}
	if i := strings.IndexByte(tok, '='); i > 0 && i < len(tok)-1 {
		if v := tok[i+1:]; hasDigit(v) || IsID(v) {
			return tok[:i+1] + "<*>"
		}
		return tok
	}
	if hasDigit(tok) || IsID(strings.Trim(tok, ",.;:()[]{}")) {
		return "<*>"
	}
	return tok
}

func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			return true
		}
	}
	return false
}
