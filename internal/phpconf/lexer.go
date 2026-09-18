package phpconf

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type tokKind int

const (
	tEOF    tokKind = iota
	tString         // decoded string literal
	tInt
	tFloat
	tIdent // identifier or namespaced name, e.g. PDO, \PDO, Foo\Bar
	tVar   // $name
	tCast  // (int), (string), ...
	tPunct // operators and punctuation
	tClose // ?> closing tag
)

type token struct {
	kind tokKind
	s    string // string value, identifier name, punctuation or cast type
	i    int64
	f    float64
	pos  int
}

// SyntaxError describes why a file could not be evaluated natively.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("phpconf: line %d: %s", e.Line, e.Msg)
}

type lexer struct {
	src []byte
	pos int
}

func (l *lexer) errAt(pos int, format string, args ...any) error {
	if pos > len(l.src) {
		pos = len(l.src)
	}
	return &SyntaxError{Line: 1 + strings.Count(string(l.src[:pos]), "\n"), Msg: fmt.Sprintf(format, args...)}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// skipSpace skips whitespace and comments.
func (l *lexer) skipSpace() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			l.pos++
		case c == '#' || (c == '/' && l.peekAt(1) == '/'):
			if c == '#' && l.peekAt(1) == '[' {
				return l.errAt(l.pos, "attributes are not supported")
			}
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				if l.src[l.pos] == '?' && l.peekAt(1) == '>' {
					break // "?>" ends a single-line comment
				}
				l.pos++
			}
		case c == '/' && l.peekAt(1) == '*':
			end := strings.Index(string(l.src[l.pos+2:]), "*/")
			if end < 0 {
				return l.errAt(l.pos, "unterminated comment")
			}
			l.pos += 2 + end + 2
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) peekAt(off int) byte {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

var castTypes = map[string]string{
	"int": "int", "integer": "int",
	"bool": "bool", "boolean": "bool",
	"float": "float", "double": "float", "real": "float",
	"string": "string", "binary": "string",
}

var puncts = []string{"??", "?:", "::", "=>", "?>", "[", "]", "(", ")", ",", ";", ".", "?", ":", "-", "+", "!", "="}

func (l *lexer) next() (token, error) {
	if err := l.skipSpace(); err != nil {
		return token{}, err
	}
	start := l.pos
	if l.pos >= len(l.src) {
		return token{kind: tEOF, pos: start}, nil
	}
	c := l.src[l.pos]
	switch {
	case c == '\'':
		s, err := l.singleQuoted()
		return token{kind: tString, s: s, pos: start}, err
	case c == '"':
		s, err := l.doubleQuoted()
		return token{kind: tString, s: s, pos: start}, err
	case c == '<' && strings.HasPrefix(string(l.src[l.pos:min(len(l.src), l.pos+3)]), "<<<"):
		s, err := l.heredoc()
		return token{kind: tString, s: s, pos: start}, err
	case c >= '0' && c <= '9':
		return l.number()
	case c == '$':
		l.pos++
		for l.pos < len(l.src) && isIdentChar(l.src[l.pos]) {
			l.pos++
		}
		return token{kind: tVar, s: string(l.src[start+1 : l.pos]), pos: start}, nil
	case isIdentStart(c) || c == '\\':
		for l.pos < len(l.src) && (isIdentChar(l.src[l.pos]) || l.src[l.pos] == '\\') {
			l.pos++
		}
		return token{kind: tIdent, s: string(l.src[start:l.pos]), pos: start}, nil
	case c == '(':
		// Possible cast: "(" ws* type ws* ")"
		p := l.pos + 1
		for p < len(l.src) && (l.src[p] == ' ' || l.src[p] == '\t') {
			p++
		}
		q := p
		for q < len(l.src) && isIdentChar(l.src[q]) {
			q++
		}
		r := q
		for r < len(l.src) && (l.src[r] == ' ' || l.src[r] == '\t') {
			r++
		}
		if q > p && r < len(l.src) && l.src[r] == ')' {
			name := strings.ToLower(string(l.src[p:q]))
			if t, ok := castTypes[name]; ok {
				l.pos = r + 1
				return token{kind: tCast, s: t, pos: start}, nil
			}
			if name == "array" || name == "object" || name == "unset" {
				return token{}, l.errAt(start, "unsupported cast (%s)", name)
			}
		}
	}
	for _, p := range puncts {
		if strings.HasPrefix(string(l.src[l.pos:min(len(l.src), l.pos+len(p))]), p) {
			l.pos += len(p)
			if p == "?>" {
				return token{kind: tClose, s: p, pos: start}, nil
			}
			return token{kind: tPunct, s: p, pos: start}, nil
		}
	}
	r, _ := utf8.DecodeRune(l.src[l.pos:])
	return token{}, l.errAt(start, "unexpected character %q", r)
}

func (l *lexer) singleQuoted() (string, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '\'':
			l.pos++
			return b.String(), nil
		case '\\':
			if n := l.peekAt(1); n == '\'' || n == '\\' {
				b.WriteByte(n)
				l.pos += 2
				continue
			}
		}
		b.WriteByte(c)
		l.pos++
	}
	return "", l.errAt(start, "unterminated string")
}

func (l *lexer) doubleQuoted() (string, error) {
	start := l.pos
	l.pos++
	end := -1
	for p := l.pos; p < len(l.src); p++ {
		if l.src[p] == '\\' {
			p++
			continue
		}
		if l.src[p] == '"' {
			end = p
			break
		}
	}
	if end < 0 {
		return "", l.errAt(start, "unterminated string")
	}
	s, err := l.unescape(l.src[l.pos:end], start, '"')
	l.pos = end + 1
	return s, err
}

// unescape decodes a double-quoted/heredoc body. quote is '"' for
// double-quoted strings (where \" is an escape) and 0 for heredocs.
func (l *lexer) unescape(body []byte, pos int, quote byte) (string, error) {
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '$' && i+1 < len(body) && (isIdentStart(body[i+1]) || body[i+1] == '{') {
			return "", l.errAt(pos, "variable interpolation in string is not supported")
		}
		if c == '{' && i+1 < len(body) && body[i+1] == '$' {
			return "", l.errAt(pos, "variable interpolation in string is not supported")
		}
		if c != '\\' || i+1 >= len(body) {
			b.WriteByte(c)
			continue
		}
		n := body[i+1]
		switch n {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'v':
			b.WriteByte('\v')
		case 'e':
			b.WriteByte(0x1b)
		case 'f':
			b.WriteByte('\f')
		case '\\':
			b.WriteByte('\\')
		case '$':
			b.WriteByte('$')
		case '"':
			if quote == '"' {
				b.WriteByte('"')
			} else {
				b.WriteString(`\"`)
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i + 1
			for j < len(body) && j < i+4 && body[j] >= '0' && body[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(string(body[i+1:j]), 8, 16)
			b.WriteByte(byte(v))
			i = j - 1
			continue
		case 'x':
			j := i + 2
			for j < len(body) && j < i+4 && isHex(body[j]) {
				j++
			}
			if j == i+2 {
				b.WriteString(`\x`)
			} else {
				v, _ := strconv.ParseUint(string(body[i+2:j]), 16, 8)
				b.WriteByte(byte(v))
			}
			i = j - 1
			continue
		case 'u':
			if i+2 < len(body) && body[i+2] == '{' {
				end := strings.IndexByte(string(body[i+3:]), '}')
				if end < 0 {
					return "", l.errAt(pos, "invalid \\u{} escape")
				}
				v, err := strconv.ParseUint(string(body[i+3:i+3+end]), 16, 32)
				if err != nil || v > utf8.MaxRune {
					return "", l.errAt(pos, "invalid \\u{} escape")
				}
				b.WriteRune(rune(v))
				i = i + 3 + end
				continue
			}
			b.WriteString(`\u`)
		default:
			b.WriteByte('\\')
			b.WriteByte(n)
		}
		i++
	}
	return b.String(), nil
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// heredoc lexes <<<ID ... ID and <<<'ID' ... ID (nowdoc), with PHP 7.3
// flexible closing-marker indentation.
func (l *lexer) heredoc() (string, error) {
	start := l.pos
	p := l.pos + 3
	for p < len(l.src) && (l.src[p] == ' ' || l.src[p] == '\t') {
		p++
	}
	nowdoc := false
	quote := byte(0)
	if p < len(l.src) && (l.src[p] == '\'' || l.src[p] == '"') {
		quote = l.src[p]
		nowdoc = quote == '\''
		p++
	}
	q := p
	for q < len(l.src) && isIdentChar(l.src[q]) {
		q++
	}
	if q == p {
		return "", l.errAt(start, "invalid heredoc")
	}
	id := string(l.src[p:q])
	if quote != 0 {
		if q >= len(l.src) || l.src[q] != quote {
			return "", l.errAt(start, "invalid heredoc")
		}
		q++
	}
	if q < len(l.src) && l.src[q] == '\r' {
		q++
	}
	if q >= len(l.src) || l.src[q] != '\n' {
		return "", l.errAt(start, "invalid heredoc")
	}
	q++
	// Scan lines for the closing marker.
	var lines []string
	lineStart := q
	for {
		lineEnd := strings.IndexByte(string(l.src[lineStart:]), '\n')
		var line string
		if lineEnd < 0 {
			line = string(l.src[lineStart:])
			lineEnd = len(l.src)
		} else {
			lineEnd += lineStart
			line = string(l.src[lineStart:lineEnd])
		}
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, id) {
			rest := trimmed[len(id):]
			if rest == "" || !isIdentChar(rest[0]) {
				indent := line[:len(line)-len(trimmed)]
				out := make([]string, len(lines))
				for i, ln := range lines {
					ln = strings.TrimSuffix(ln, "\r")
					if strings.TrimLeft(ln, " \t") == "" && len(ln) < len(indent) {
						ln = ""
					} else if !strings.HasPrefix(ln, indent) {
						return "", l.errAt(start, "invalid body indentation in heredoc")
					} else {
						ln = ln[len(indent):]
					}
					out[i] = ln
				}
				body := strings.Join(out, "\n")
				l.pos = lineStart + len(indent) + len(id)
				if nowdoc {
					return body, nil
				}
				return l.unescape([]byte(body), start, 0)
			}
		}
		lines = append(lines, line)
		if lineEnd >= len(l.src) {
			return "", l.errAt(start, "unterminated heredoc")
		}
		lineStart = lineEnd + 1
	}
}

func (l *lexer) number() (token, error) {
	start := l.pos
	p := l.pos
	isDigitOr := func(ok func(byte) bool) {
		for p < len(l.src) && (ok(l.src[p]) || (l.src[p] == '_' && p+1 < len(l.src) && ok(l.src[p+1]))) {
			p++
		}
	}
	dec := func(c byte) bool { return c >= '0' && c <= '9' }
	base := 10
	prefix := 0
	if l.src[p] == '0' && p+1 < len(l.src) {
		switch l.src[p+1] {
		case 'x', 'X':
			base, prefix = 16, 2
		case 'b', 'B':
			base, prefix = 2, 2
		case 'o', 'O':
			base, prefix = 8, 2
		}
	}
	if base != 10 {
		p += prefix
		switch base {
		case 16:
			isDigitOr(isHex)
		case 2:
			isDigitOr(func(c byte) bool { return c == '0' || c == '1' })
		case 8:
			isDigitOr(func(c byte) bool { return c >= '0' && c <= '7' })
		}
		l.pos = p
		if p == start+prefix {
			return token{}, l.errAt(start, "invalid number %q", l.src[start:p])
		}
		return intToken(strings.ReplaceAll(string(l.src[start+prefix:p]), "_", ""), base, start, l)
	}
	isDigitOr(dec)
	isFloat := false
	// Like PHP's lexer, a "." directly after digits belongs to the number
	// ("1." is a float), so concatenating a number needs a space ("1 . 'x'").
	if p < len(l.src) && l.src[p] == '.' {
		isFloat = true
		p++
		isDigitOr(dec)
	}
	if p < len(l.src) && (l.src[p] == 'e' || l.src[p] == 'E') {
		q := p + 1
		if q < len(l.src) && (l.src[q] == '+' || l.src[q] == '-') {
			q++
		}
		if q < len(l.src) && dec(l.src[q]) {
			isFloat = true
			p = q
			isDigitOr(dec)
		}
	}
	l.pos = p
	text := strings.ReplaceAll(string(l.src[start:p]), "_", "")
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !isRangeErr(err) {
			return token{}, l.errAt(start, "invalid number %q", text)
		}
		return token{kind: tFloat, f: f, pos: start}, nil
	}
	if len(text) > 1 && text[0] == '0' {
		return intToken(text[1:], 8, start, l)
	}
	return intToken(text, 10, start, l)
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

func intToken(digits string, base, start int, l *lexer) (token, error) {
	v, err := strconv.ParseInt(digits, base, 64)
	if err == nil {
		return token{kind: tInt, i: v, pos: start}, nil
	}
	if isRangeErr(err) {
		// PHP converts integer overflow to float.
		u, uerr := strconv.ParseUint(digits, base, 64)
		if uerr == nil {
			return token{kind: tFloat, f: float64(u), pos: start}, nil
		}
		if base == 10 {
			f, _ := strconv.ParseFloat(digits, 64)
			return token{kind: tFloat, f: f, pos: start}, nil
		}
		return token{kind: tFloat, f: math.Inf(1), pos: start}, nil
	}
	return token{}, l.errAt(start, "invalid number %q", digits)
}
