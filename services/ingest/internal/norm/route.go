// Package norm holds the pure, allocation-conscious normalizers the writer applies to telemetry:
// route templates, SQL normalization, user-agent classification, error fingerprints and log patterns.
package norm

import "strings"

const maxRouteLen = 256

// Route normalizes a URL path into a route template: query/fragment dropped, empty segments collapsed,
// id-like segments (numbers, UUIDs, hex, ULIDs, long opaque tokens) replaced by ":id".
func Route(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	// Absolute URL → path only.
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			p = rest[j:]
		} else {
			p = "/"
		}
	}
	if p == "" || p == "/" {
		return "/"
	}
	if !routeNeedsRewrite(p) {
		if len(p) > maxRouteLen {
			return p[:maxRouteLen]
		}
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		if IsID(seg) {
			b.WriteString(":id")
		} else {
			b.WriteString(seg)
		}
		if b.Len() >= maxRouteLen {
			break
		}
	}
	if b.Len() == 0 {
		return "/"
	}
	s := b.String() // trailing slash dropped: "/a/" and "/a" are the same route
	if len(s) > maxRouteLen {
		s = s[:maxRouteLen]
	}
	return s
}

// routeNeedsRewrite reports whether p has empty segments, a trailing slash or id-like segments.
func routeNeedsRewrite(p string) bool {
	if p[0] != '/' || p[len(p)-1] == '/' || strings.Contains(p, "//") {
		return true
	}
	start := 1
	for i := 1; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if IsID(p[start:i]) {
				return true
			}
			start = i + 1
		}
	}
	return false
}

// IsID reports whether a path segment looks like an identifier rather than a static word.
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
		case c == '-' || c == '_' || c == '.' || c == '~' || c == ':' || c == '@' || c == '%' || c == '=':
			other++
		default:
			other++
		}
	}
	if digits == n {
		return true // 123
	}
	if n == 36 && isUUID(s) {
		return true
	}
	if letters == 0 && other == 0 && n >= 8 && digits > 0 {
		return true // hex: deadbeef01, sha1, mongo object id
	}
	if n == 26 && other == 0 && digits > 0 && isULID(s) {
		return true
	}
	// Long opaque tokens mixing digits and letters (base62/base64 ids, slugs with long numbers).
	if n >= 20 && digits >= 4 && digits+hexLetters+letters >= n-2 {
		return true
	}
	// Mostly numeric segment with a short prefix/suffix: "INV-2024-000123", "123.json" stays static-ish → id.
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

// isULID: 26 chars of Crockford base32 (no I, L, O, U).
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

// StripQueryValues removes the query string values from a URL (keys are kept) and the fragment.
// "https://x/a?b=1&c=2#f" → "https://x/a?b&c".
func StripQueryValues(u string) string {
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	q := strings.IndexByte(u, '?')
	if q < 0 {
		return u
	}
	var b strings.Builder
	b.Grow(len(u))
	b.WriteString(u[:q])
	sep := byte('?')
	for kv := range strings.SplitSeq(u[q+1:], "&") {
		if kv == "" {
			continue
		}
		if i := strings.IndexByte(kv, '='); i >= 0 {
			kv = kv[:i]
		}
		b.WriteByte(sep)
		b.WriteString(kv)
		sep = '&'
	}
	return b.String()
}

// ResourceURL turns a client network URL into a low-cardinality template: scheme + host kept,
// query dropped, path normalized like routes. "https://api.x.com/v1/orders/42?a=1" → "https://api.x.com/v1/orders/:id".
func ResourceURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	host := ""
	path := u
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			host, path = u[:i+3+j], rest[j:]
		} else {
			host, path = u, ""
		}
		if k := strings.LastIndexByte(host, '@'); k >= 0 { // drop userinfo
			host = host[:i+3] + host[k+1:]
		}
	}
	if path == "" {
		return host
	}
	return host + Route(path)
}
