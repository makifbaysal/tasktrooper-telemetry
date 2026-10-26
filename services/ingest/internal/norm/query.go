package norm

import (
	"strings"

	"github.com/cespare/xxhash/v2"
)

const maxQueryLen = 4096

// Query normalizes a database statement: string/number/hex literals become "?", "IN (?, ?, ?)" lists
// collapse to "IN (?)", whitespace collapses to single spaces. Identifiers, placeholders ($1, :name)
// and keywords are kept as written.
func Query(q string) string {
	if len(q) > maxQueryLen*2 {
		q = q[:maxQueryLen*2]
	}
	b := make([]byte, 0, len(q))
	space := false
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
			i++
			continue
		case c == '\'':
			// string literal with '' escapes
			j := i + 1
			for j < len(q) {
				if q[j] == '\\' {
					j += 2
					continue
				}
				if q[j] == '\'' {
					if j+1 < len(q) && q[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			b = appendTok(b, &space, "?")
			i = j + 1
			continue
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			// line comment
			for i < len(q) && q[i] != '\n' {
				i++
			}
			space = true
			continue
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				i = len(q)
			} else {
				i += end + 4
			}
			space = true
			continue
		case isDigit(c) && !prevIsIdent(b, space):
			// number (int, float, exponent, hex 0x..)
			j := i + 1
			if c == '0' && j < len(q) && (q[j] == 'x' || q[j] == 'X') {
				j++
				for j < len(q) && isHex(q[j]) {
					j++
				}
			} else {
				for j < len(q) && (isDigit(q[j]) || q[j] == '.' || q[j] == 'e' || q[j] == 'E' ||
					((q[j] == '+' || q[j] == '-') && (q[j-1] == 'e' || q[j-1] == 'E'))) {
					j++
				}
			}
			// negative literal: "= -5" → drop the sign we already wrote
			if n := len(b); n > 0 && b[n-1] == '-' && !space && n >= 2 && (b[n-2] == ' ' || b[n-2] == '(' || b[n-2] == ',' || b[n-2] == '=') {
				b = b[:n-1]
			}
			b = appendTok(b, &space, "?")
			i = j
			continue
		}
		if space && len(b) > 0 {
			b = append(b, ' ')
		}
		space = false
		b = append(b, c)
		i++
	}
	s := collapseInLists(b)
	if len(s) > maxQueryLen {
		s = s[:maxQueryLen]
	}
	return s
}

// QueryHash returns a stable 64-bit hash of an already normalized query (0 for empty).
func QueryHash(normalized string) uint64 {
	if normalized == "" {
		return 0
	}
	return xxhash.Sum64String(normalized)
}

func appendTok(b []byte, space *bool, tok string) []byte {
	if *space && len(b) > 0 {
		b = append(b, ' ')
	}
	*space = false
	return append(b, tok...)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// prevIsIdent reports whether the previous byte continues an identifier (t1, $1, :p2), so a digit is not a literal.
func prevIsIdent(b []byte, space bool) bool {
	if space || len(b) == 0 {
		return false
	}
	c := b[len(b)-1]
	return c == '$' || c == ':' || c == '@' || isIdentByte(c)
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || isDigit(c)
}

// collapseInLists rewrites "(?, ?, ?)" and "(?,?)" into "(?)" so batch sizes don't create new query hashes.
func collapseInLists(b []byte) string {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '(' && i+1 < len(b) && b[i+1] == '?' {
			j := i + 2
			for j < len(b) {
				k := j
				for k < len(b) && b[k] == ' ' {
					k++
				}
				if k < len(b) && b[k] == ',' {
					k++
					for k < len(b) && b[k] == ' ' {
						k++
					}
					if k < len(b) && b[k] == '?' {
						j = k + 1
						continue
					}
				}
				break
			}
			if j < len(b) && b[j] == ')' {
				out = append(out, "(?)"...)
				i = j
				continue
			}
		}
		out = append(out, b[i])
	}
	return string(out)
}
