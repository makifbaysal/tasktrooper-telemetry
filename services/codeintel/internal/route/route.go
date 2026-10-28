// Package route normalizes route templates found in code and observed in telemetry so they can be compared.
//
// Code routes keep their parameter names as ":name" ("/orders/{id}" → "/orders/:id", Flask "<int:id>" → ":id",
// Next.js "[id]" → ":id"). Observed routes come from the writer (services/ingest/internal/norm.Route): an SDK
// http.route template is stored as-is, otherwise id-like path segments become ":id". Canon reduces both to the
// same shape: every parameter or id-like segment becomes ":".
package route

import "strings"

const maxRouteLen = 256 // same cap as the writer

// Normalize turns a framework route pattern into "/a/:param/b". Query strings, duplicate and trailing slashes
// are dropped like the writer does.
func Normalize(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "/"
	}
	var b strings.Builder
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "{$}" {
			continue
		}
		b.WriteByte('/')
		b.WriteString(param(seg))
	}
	if b.Len() == 0 {
		return "/"
	}
	s := b.String()
	if len(s) > maxRouteLen {
		s = s[:maxRouteLen]
	}
	return s
}

// param rewrites one segment when it is a parameter in any supported syntax.
func param(seg string) string {
	switch {
	case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"): // Go 1.22 / FastAPI / ASP.NET: {id}, {path...}, {id:int}
		n := strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
		n = strings.TrimSuffix(n, "...")
		if i := strings.IndexByte(n, ':'); i >= 0 {
			n = n[:i]
		}
		return ":" + clean(n)
	case strings.HasPrefix(seg, "<") && strings.HasSuffix(seg, ">"): // Flask / Django: <int:id>, <slug>
		n := strings.TrimSuffix(strings.TrimPrefix(seg, "<"), ">")
		if i := strings.LastIndexByte(n, ':'); i >= 0 {
			n = n[i+1:]
		}
		return ":" + clean(n)
	case strings.HasPrefix(seg, "[") && strings.HasSuffix(seg, "]"): // Next.js: [id], [...slug], [[...slug]]
		n := strings.Trim(seg, "[]")
		n = strings.TrimPrefix(n, "...")
		return ":" + clean(n)
	case strings.HasPrefix(seg, ":"): // express / gin / chi-like ":id", optional ":id?"
		n := seg[1:]
		if i := strings.IndexAny(n, "(?"); i >= 0 { // ":id(\\d+)", ":id?"
			n = n[:i]
		}
		return ":" + clean(n)
	case strings.HasPrefix(seg, "*"): // gin / echo "*filepath", fiber "*"
		if len(seg) == 1 {
			return ":wildcard"
		}
		return ":" + clean(seg[1:])
	}
	return seg
}

func clean(n string) string {
	// chi regexp params {id:[0-9]+} were cut at ':' above; drop anything non-identifier.
	var b strings.Builder
	for _, c := range n {
		if c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "param"
	}
	return b.String()
}

// Canon reduces a route (code or observed) to its comparable form: parameters and id-like segments → ":".
func Canon(r string) string {
	r = Normalize(r)
	if r == "/" {
		return r
	}
	var b strings.Builder
	for seg := range strings.SplitSeq(r, "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		if strings.HasPrefix(seg, ":") || IsID(seg) {
			b.WriteByte(':')
		} else {
			b.WriteString(strings.ToLower(seg))
		}
	}
	return b.String()
}

// SplitKey splits an endpoint key "METHOD /route" into its parts. A key without a method returns "ANY".
func SplitKey(key string) (method, path string) {
	if m, p, ok := strings.Cut(key, " "); ok && strings.HasPrefix(p, "/") {
		return strings.ToUpper(m), p
	}
	return "ANY", key
}

// Key builds an endpoint key from a method and a raw pattern.
func Key(method, pattern string) string {
	m := strings.ToUpper(strings.TrimSpace(method))
	if m == "" || m == "ALL" || m == "*" || m == "USE" {
		m = "ANY"
	}
	return m + " " + Normalize(pattern)
}

// MethodMatches compares a code method with an observed one. ANY / GRPC in code match everything.
func MethodMatches(code, observed string) bool {
	code, observed = strings.ToUpper(code), strings.ToUpper(observed)
	return code == "ANY" || code == "GRPC" || observed == "" || code == observed
}

// Join joins a prefix and a sub path ("/api", "orders/:id" → "/api/orders/:id").
func Join(prefix, p string) string {
	if prefix == "" {
		return p
	}
	if p == "" || p == "/" {
		return prefix
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(p, "/")
}

// IsID reports whether a path segment looks like an identifier rather than a static word.
// Copy of services/ingest/internal/norm.IsID (internal package of another module) — keep in sync.
func IsID(s string) bool {
	n := len(s)
	if n == 0 {
		return false
	}
	var digits, hexLetters, letters, other int
	for i := 0; i < n; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
			hexLetters++
		case (c >= 'g' && c <= 'z') || (c >= 'G' && c <= 'Z'):
			letters++
		default:
			other++
		}
	}
	if digits == n {
		return true
	}
	if n == 36 && isUUID(s) {
		return true
	}
	if letters == 0 && other == 0 && n >= 8 && digits > 0 {
		return true
	}
	if n == 26 && other == 0 && digits > 0 && isULID(s) {
		return true
	}
	if n >= 20 && digits >= 4 && digits+hexLetters+letters >= n-2 {
		return true
	}
	if digits >= 6 && digits*2 >= n {
		return true
	}
	return false
}

func isUUID(s string) bool {
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !isHex(c) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isULID(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U':
		default:
			return false
		}
	}
	return true
}

// FromRegex converts a Django re_path style regex to a template: anchors dropped, named groups
// "(?P<pk>[0-9]+)" / "(?<pk>…)" → ":pk", other groups → ":param".
func FromRegex(re string) string {
	re = strings.TrimPrefix(re, "^")
	re = strings.TrimSuffix(re, "$")
	var b strings.Builder
	for i := 0; i < len(re); i++ {
		c := re[i]
		if c == '\\' && i+1 < len(re) {
			i++
			b.WriteByte(re[i])
			continue
		}
		if c != '(' {
			if c != '?' && c != '+' && c != '*' {
				b.WriteByte(c)
			}
			continue
		}
		depth, j := 0, i
		for ; j < len(re); j++ {
			if re[j] == '\\' {
				j++
				continue
			}
			if re[j] == '(' {
				depth++
			} else if re[j] == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		grp := re[i:min(j+1, len(re))]
		name := "param"
		for _, pre := range []string{"(?P<", "(?<"} {
			if strings.HasPrefix(grp, pre) {
				if k := strings.IndexByte(grp, '>'); k > len(pre) {
					name = grp[len(pre):k]
				}
			}
		}
		b.WriteString(":" + name)
		i = j
	}
	return Normalize(b.String())
}

// Observed turns a route from telemetry into an inventory template: parameters in any syntax → ":name",
// id-like raw segments → ":id" (the writer's rule).
func Observed(r string) string {
	r = Normalize(r)
	if r == "/" {
		return r
	}
	var b strings.Builder
	for seg := range strings.SplitSeq(r, "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		if !strings.HasPrefix(seg, ":") && IsID(seg) {
			seg = ":id"
		}
		b.WriteString(seg)
	}
	return b.String()
}
