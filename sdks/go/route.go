package trooper

import "strings"

// NormalizeRoute converts brace-style path parameters to the colon style used by trooper.route:
//
//	/users/{id}          -> /users/:id      (net/http, chi)
//	/users/{id:[0-9]+}   -> /users/:id      (chi regexp)
//	/files/{path...}     -> /files/*path    (net/http wildcard)
//	/exact/{$}           -> /exact/         (net/http end anchor)
//
// Colon-style templates (gin, echo, fiber) and gRPC method names are returned unchanged.
func NormalizeRoute(route string) string {
	if !strings.Contains(route, "{") {
		return route
	}
	var b strings.Builder
	b.Grow(len(route))
	for i := 0; i < len(route); {
		c := route[i]
		if c != '{' {
			b.WriteByte(c)
			i++
			continue
		}
		// find matching close brace (regexps may contain nested braces, e.g. {id:[0-9]{3}})
		depth, j := 0, i
		for ; j < len(route); j++ {
			if route[j] == '{' {
				depth++
			} else if route[j] == '}' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if j >= len(route) { // unbalanced: keep the rest as is
			b.WriteString(route[i:])
			break
		}
		name := route[i+1 : j]
		if k := strings.IndexByte(name, ':'); k >= 0 {
			name = name[:k]
		}
		switch {
		case name == "$":
		case strings.HasSuffix(name, "..."):
			b.WriteString("*" + strings.TrimSuffix(name, "..."))
		default:
			b.WriteString(":" + name)
		}
		i = j + 1
	}
	return b.String()
}

// SanitizeSQL replaces literals in a SQL statement with '?' so that query text never carries
// values (PII). Quoted strings, numbers, hex/binary literals and dollar-quoted strings are replaced;
// identifiers, keywords and placeholders ($1, ?, :name) are kept.
func SanitizeSQL(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'':
			// single-quoted string; '' escapes
			j := i + 1
			for j < len(q) {
				if q[j] == '\\' && j+1 < len(q) {
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
			b.WriteByte('?')
			i = j + 1
		case c == '$' && i+1 < len(q) && (q[i+1] == '$' || isTagStart(q[i+1])) && dollarTag(q[i:]) != "":
			tag := dollarTag(q[i:])
			end := strings.Index(q[i+len(tag):], tag)
			b.WriteByte('?')
			if end < 0 {
				i = len(q)
			} else {
				i += len(tag) + end + len(tag)
			}
		case c >= '0' && c <= '9' && (i == 0 || !isIdent(q[i-1])):
			j := i
			for j < len(q) && (isIdent(q[j]) && q[j] != '$') {
				j++
			}
			b.WriteByte('?')
			i = j
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			// line comment: drop
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				i = len(q)
			} else {
				i += j
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isTagStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// dollarTag returns "$tag$" or "$$" at the start of s, or "".
func dollarTag(s string) string {
	for j := 1; j < len(s); j++ {
		if s[j] == '$' {
			return s[:j+1]
		}
		if !(isTagStart(s[j]) || s[j] >= '0' && s[j] <= '9') {
			return ""
		}
	}
	return ""
}
