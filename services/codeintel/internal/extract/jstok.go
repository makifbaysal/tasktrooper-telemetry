package extract

// A small JS/TS tokenizer: identifiers, strings (quotes and template literals), numbers and punctuation.
// Comments are dropped; regex literals are skipped after operators. Strings never span a newline (except
// template literals), which bounds the damage of an apostrophe in JSX text.

type tokKind uint8

const (
	tIdent tokKind = iota
	tString
	tTemplate // template literal with ${…}; Val is the raw text
	tNumber
	tPunct
)

type jsTok struct {
	kind tokKind
	val  string
	line int
}

func (t jsTok) is(p string) bool { return t.kind == tPunct && t.val == p }
func (t jsTok) id(s string) bool { return t.kind == tIdent && t.val == s }

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// regexAllowedAfter: a '/' after these starts a regex literal, not a division.
func regexAllowedAfter(prev *jsTok) bool {
	if prev == nil {
		return true
	}
	if prev.kind == tPunct {
		switch prev.val {
		case ")", "]", "}":
			return false
		}
		return true
	}
	if prev.kind == tIdent {
		switch prev.val {
		case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "yield", "await":
			return true
		}
	}
	return false
}

func tokenizeJS(src []byte) []jsTok {
	toks := make([]jsTok, 0, len(src)/4)
	line := 1
	n := len(src)
	var prev *jsTok
	push := func(t jsTok) {
		toks = append(toks, t)
		prev = &toks[len(toks)-1]
	}
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 2
		case c == '/' && regexAllowedAfter(prev):
			// regex literal
			j := i + 1
			inClass := false
			for j < n && src[j] != '\n' {
				if src[j] == '\\' {
					j += 2
					continue
				}
				if src[j] == '[' {
					inClass = true
				} else if src[j] == ']' {
					inClass = false
				} else if src[j] == '/' && !inClass {
					break
				}
				j++
			}
			j++
			for j < n && isIdentPart(src[j]) { // flags
				j++
			}
			push(jsTok{kind: tString, val: "", line: line}) // opaque value
			i = min(j, n)
		case c == '"' || c == '\'':
			j := i + 1
			var b []byte
			for j < n && src[j] != c && src[j] != '\n' {
				if src[j] == '\\' && j+1 < n {
					b = append(b, src[j+1])
					j += 2
					continue
				}
				b = append(b, src[j])
				j++
			}
			push(jsTok{kind: tString, val: string(b), line: line})
			i = j + 1
		case c == '`':
			j := i + 1
			startLine := line
			depth := 0
			hasExpr := false
			for j < n {
				if src[j] == '\\' {
					j += 2
					continue
				}
				if src[j] == '\n' {
					line++
				}
				if depth == 0 && src[j] == '`' {
					break
				}
				if src[j] == '$' && j+1 < n && src[j+1] == '{' {
					depth++
					hasExpr = true
					j += 2
					continue
				}
				if depth > 0 && src[j] == '}' {
					depth--
				}
				j++
			}
			k := tString
			if hasExpr {
				k = tTemplate
			}
			push(jsTok{kind: k, val: string(src[i+1 : min(j, n)]), line: startLine})
			i = j + 1
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(src[j]) {
				j++
			}
			push(jsTok{kind: tIdent, val: string(src[i:j]), line: line})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && (isIdentPart(src[j]) || src[j] == '.') {
				j++
			}
			push(jsTok{kind: tNumber, val: string(src[i:j]), line: line})
			i = j
		case c == '=' && i+1 < n && src[i+1] == '>':
			push(jsTok{kind: tPunct, val: "=>", line: line})
			i += 2
		case c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.':
			push(jsTok{kind: tPunct, val: "...", line: line})
			i += 3
		case c == '?' && i+1 < n && src[i+1] == '.':
			push(jsTok{kind: tPunct, val: ".", line: line}) // optional chaining reads like a member access
			i += 2
		default:
			push(jsTok{kind: tPunct, val: string(c), line: line})
			i++
		}
	}
	return toks
}

// matchClose returns the index of the token closing the bracket at i ("(", "[", "{"), or len(toks)-1.
func matchClose(toks []jsTok, i int) int {
	open := toks[i].val
	var close string
	switch open {
	case "(":
		close = ")"
	case "[":
		close = "]"
	case "{":
		close = "}"
	default:
		return i
	}
	depth := 0
	for j := i; j < len(toks); j++ {
		if toks[j].kind != tPunct {
			continue
		}
		switch toks[j].val {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				if toks[j].val != close {
					return j
				}
				return j
			}
		}
	}
	return len(toks) - 1
}
