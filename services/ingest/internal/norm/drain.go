package norm

import "github.com/cespare/xxhash/v2"

const maxPatternTokens = 48

// PatternID is a fast, Drain-style log template id: the body is tokenized on whitespace and
// punctuation, variable tokens (anything with digits, hex/uuid ids, quoted values, key=value values)
// become "<*>", and the first maxPatternTokens template tokens are hashed. Identical templates
// map to the same id across services; the caller mixes in service if needed.
func PatternID(body string) uint64 {
	d := xxhash.New()
	n := 0
	i := 0
	for i < len(body) && n < maxPatternTokens {
		// skip separators
		for i < len(body) && isSep(body[i]) {
			i++
		}
		if i >= len(body) {
			break
		}
		start := i
		c := body[i]
		if c == '"' || c == '\'' {
			j := i + 1
			for j < len(body) && body[j] != c {
				j++
			}
			i = j + 1
			_, _ = d.WriteString("<*> ")
			n++
			continue
		}
		for i < len(body) && !isSep(body[i]) && body[i] != '=' {
			i++
		}
		tok := body[start:i]
		if i < len(body) && body[i] == '=' {
			// key=value: keep the key, mask a variable value
			_, _ = d.WriteString(tok)
			_, _ = d.WriteString("= ")
			i++
			vs := i
			if i < len(body) && (body[i] == '"' || body[i] == '\'') {
				q := body[i]
				i++
				for i < len(body) && body[i] != q {
					i++
				}
				i++
				_, _ = d.WriteString("<*> ")
			} else {
				for i < len(body) && !isSep(body[i]) {
					i++
				}
				v := body[vs:min(i, len(body))]
				if hasDigit(v) || IsID(v) {
					_, _ = d.WriteString("<*> ")
				} else {
					_, _ = d.WriteString(v)
					_, _ = d.WriteString(" ")
				}
			}
			n++
			continue
		}
		if hasDigit(tok) || IsID(tok) {
			_, _ = d.WriteString("<*> ")
		} else {
			_, _ = d.WriteString(tok)
			_, _ = d.WriteString(" ")
		}
		n++
	}
	h := d.Sum64()
	if h == 0 {
		h = 1
	}
	return h
}

func isSep(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', ';', '(', ')', '[', ']', '{', '}', '|', '<', '>':
		return true
	}
	return false
}
