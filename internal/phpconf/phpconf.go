// Package phpconf evaluates PHP configuration files such as Magento's
// app/etc/env.php and app/etc/config.php without running PHP.
//
// Only the subset of PHP needed for "return [ ... ];" style config files is
// supported. Anything else yields an error so callers can fall back to php.
//
// Arrays decode to map[string]any when they contain any explicit key (integer
// keys are stringified) and to []any when purely positional. Scalars decode
// to string, int64, float64, bool or nil.
package phpconf

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Parse evaluates a PHP config file of the form "<?php ... return <expr>;".
func Parse(src []byte) (any, error) {
	p := &parser{lx: lexer{src: src}}
	// Skip a shebang line and the opening tag.
	if bytes.HasPrefix(src, []byte("#!")) {
		if i := bytes.IndexByte(src, '\n'); i >= 0 {
			p.lx.pos = i + 1
		}
	}
	rest := src[p.lx.pos:]
	trimmed := bytes.TrimLeft(rest, " \t\r\n")
	if len(trimmed) != len(rest) {
		// Whitespace before "<?php" would be output by PHP; tolerate it.
		p.lx.pos += len(rest) - len(trimmed)
	}
	switch {
	case bytes.HasPrefix(trimmed, []byte("<?php")):
		p.lx.pos += 5
	case bytes.HasPrefix(trimmed, []byte("<?")):
		return nil, p.lx.errAt(p.lx.pos, "short open tags are not supported")
	default:
		return nil, p.lx.errAt(p.lx.pos, "missing <?php open tag")
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p.file()
}

// ParseFile reads and evaluates a PHP config file.
func ParseFile(path string) (any, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := Parse(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

type parser struct {
	lx  lexer
	tok token
}

func (p *parser) advance() error {
	t, err := p.lx.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) errf(format string, args ...any) error {
	return p.lx.errAt(p.tok.pos, format, args...)
}

func (p *parser) isPunct(s string) bool { return p.tok.kind == tPunct && p.tok.s == s }

func (p *parser) isKeyword(s string) bool {
	return p.tok.kind == tIdent && strings.EqualFold(p.tok.s, s)
}

func (p *parser) expect(s string) error {
	if !p.isPunct(s) {
		return p.errf("expected %q, found %s", s, p.describe())
	}
	return p.advance()
}

func (p *parser) describe() string {
	switch p.tok.kind {
	case tEOF:
		return "end of file"
	case tString:
		return "string literal"
	case tInt, tFloat:
		return "number"
	case tVar:
		return "variable $" + p.tok.s
	case tCast:
		return "cast (" + p.tok.s + ")"
	case tClose:
		return "?>"
	}
	return strconv.Quote(p.tok.s)
}

// file parses statements until "return expr;".
func (p *parser) file() (any, error) {
	for {
		switch {
		case p.isPunct(";"):
			if err := p.advance(); err != nil {
				return nil, err
			}
		case p.isKeyword("declare"):
			if err := p.skipDeclare(); err != nil {
				return nil, err
			}
		case p.isKeyword("use"), p.isKeyword("namespace"):
			if err := p.skipUntilSemicolon(); err != nil {
				return nil, err
			}
		case p.isKeyword("return"):
			if err := p.advance(); err != nil {
				return nil, err
			}
			v, err := p.expr()
			if err != nil {
				return nil, err
			}
			if p.tok.kind == tClose || p.tok.kind == tEOF {
				return v, nil
			}
			if err := p.expect(";"); err != nil {
				return nil, err
			}
			return v, nil
		case p.tok.kind == tEOF, p.tok.kind == tClose:
			return nil, p.errf("no return statement")
		default:
			return nil, p.errf("unsupported statement starting with %s", p.describe())
		}
	}
}

func (p *parser) skipDeclare() error {
	if err := p.advance(); err != nil {
		return err
	}
	if err := p.expect("("); err != nil {
		return err
	}
	for !p.isPunct(")") {
		if p.tok.kind == tEOF {
			return p.errf("unterminated declare")
		}
		if err := p.advance(); err != nil {
			return err
		}
	}
	if err := p.advance(); err != nil {
		return err
	}
	if p.isPunct(";") {
		return p.advance()
	}
	return p.errf("unsupported declare block")
}

func (p *parser) skipUntilSemicolon() error {
	for !p.isPunct(";") {
		if p.tok.kind == tEOF {
			return p.errf("unexpected end of file")
		}
		if p.isPunct("(") || p.isPunct("[") {
			return p.errf("unsupported statement")
		}
		if err := p.advance(); err != nil {
			return err
		}
	}
	return p.advance()
}

// expr := coalesce [ "?:" expr | "?" expr ":" expr ]
func (p *parser) expr() (any, error) {
	v, err := p.coalesce()
	if err != nil {
		return nil, err
	}
	switch {
	case p.isPunct("?:"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		r, err := p.expr()
		if err != nil {
			return nil, err
		}
		if truthy(v) {
			return v, nil
		}
		return r, nil
	case p.isPunct("?"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		a, err := p.expr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		b, err := p.expr()
		if err != nil {
			return nil, err
		}
		if truthy(v) {
			return a, nil
		}
		return b, nil
	}
	return v, nil
}

// coalesce := concat [ "??" coalesce ]
func (p *parser) coalesce() (any, error) {
	v, err := p.concat()
	if err != nil {
		return nil, err
	}
	if p.isPunct("??") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		r, err := p.coalesce()
		if err != nil {
			return nil, err
		}
		if v == nil {
			return r, nil
		}
	}
	return v, nil
}

// concat := unary ( "." unary )*
func (p *parser) concat() (any, error) {
	v, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.isPunct(".") {
		pos := p.tok.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		ls, err := toString(v)
		if err != nil {
			return nil, p.lx.errAt(pos, "%v", err)
		}
		rs, err := toString(r)
		if err != nil {
			return nil, p.lx.errAt(pos, "%v", err)
		}
		v = ls + rs
	}
	return v, nil
}

func (p *parser) unary() (any, error) {
	switch {
	case p.isPunct("-"), p.isPunct("+"):
		neg := p.tok.s == "-"
		pos := p.tok.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		switch x := v.(type) {
		case int64:
			if neg {
				if x == math.MinInt64 {
					return -float64(x), nil
				}
				return -x, nil
			}
			return x, nil
		case float64:
			if neg {
				return -x, nil
			}
			return x, nil
		}
		return nil, p.lx.errAt(pos, "unary %s on non-number is not supported", p.lx.src[pos:pos+1])
	case p.isPunct("!"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		return !truthy(v), nil
	case p.tok.kind == tCast:
		typ := p.tok.s
		pos := p.tok.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		r, err := cast(typ, v)
		if err != nil {
			return nil, p.lx.errAt(pos, "%v", err)
		}
		return r, nil
	}
	return p.primary()
}

func (p *parser) primary() (any, error) {
	t := p.tok
	switch t.kind {
	case tString:
		return t.s, p.advance()
	case tInt:
		return t.i, p.advance()
	case tFloat:
		return t.f, p.advance()
	case tVar:
		return nil, p.errf("variables are not supported ($%s)", t.s)
	case tPunct:
		switch t.s {
		case "[":
			if err := p.advance(); err != nil {
				return nil, err
			}
			return p.array("]")
		case "(":
			if err := p.advance(); err != nil {
				return nil, err
			}
			v, err := p.expr()
			if err != nil {
				return nil, err
			}
			return v, p.expect(")")
		}
	case tIdent:
		return p.ident()
	}
	return nil, p.errf("unexpected %s", p.describe())
}

func (p *parser) ident() (any, error) {
	name := p.tok.s
	if err := p.advance(); err != nil {
		return nil, err
	}
	bare := strings.TrimPrefix(name, "\\")
	lower := strings.ToLower(bare)
	if p.isPunct("::") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind != tIdent || strings.Contains(p.tok.s, "\\") {
			return nil, p.errf("unsupported class member access %s::%s", bare, p.describe())
		}
		member := p.tok.s
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.isPunct("(") {
			return nil, p.errf("static method calls are not supported (%s::%s)", bare, member)
		}
		if strings.EqualFold(member, "class") {
			return bare, nil
		}
		// The actual constant value is unknown; its name is a stable stand-in.
		return bare + "::" + member, nil
	}
	if p.isPunct("(") {
		switch lower {
		case "array":
			if err := p.advance(); err != nil {
				return nil, err
			}
			return p.array(")")
		case "getenv":
			return p.getenv()
		}
		return nil, p.errf("function calls are not supported (%s)", bare)
	}
	switch lower {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	case "php_eol":
		return "\n", nil
	case "directory_separator":
		return "/", nil
	case "php_int_max":
		return int64(math.MaxInt64), nil
	case "php_int_min":
		return int64(math.MinInt64), nil
	}
	return nil, p.errf("unknown constant %s", bare)
}

func (p *parser) getenv() (any, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	if p.isPunct(")") {
		return nil, p.errf("getenv() without arguments is not supported")
	}
	arg, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.isPunct(",") { // getenv('X', true)
		if err := p.advance(); err != nil {
			return nil, err
		}
		if _, err := p.expr(); err != nil {
			return nil, err
		}
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	name, err := toString(arg)
	if err != nil {
		return nil, p.errf("getenv: %v", err)
	}
	if v, ok := os.LookupEnv(name); ok {
		return v, nil
	}
	return false, nil
}

// array parses array elements up to the closing token.
func (p *parser) array(closer string) (any, error) {
	type entry struct {
		key string
		val any
	}
	var entries []entry
	index := map[string]int{}
	hasKeys := false
	var next int64
	for !p.isPunct(closer) {
		v, err := p.expr()
		if err != nil {
			return nil, err
		}
		var key string
		if p.isPunct("=>") {
			pos := p.tok.pos
			if err := p.advance(); err != nil {
				return nil, err
			}
			val, err := p.expr()
			if err != nil {
				return nil, err
			}
			hasKeys = true
			k, isInt, err := arrayKey(v)
			if err != nil {
				return nil, p.lx.errAt(pos, "%v", err)
			}
			if isInt && k >= next {
				if k == math.MaxInt64 {
					next = k
				} else {
					next = k + 1
				}
			}
			if isInt {
				key = strconv.FormatInt(k, 10)
			} else {
				key = v.(string)
			}
			v = val
		} else {
			key = strconv.FormatInt(next, 10)
			next++
		}
		if i, ok := index[key]; ok {
			entries[i].val = v
		} else {
			index[key] = len(entries)
			entries = append(entries, entry{key, v})
		}
		if p.isPunct(",") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		if !p.isPunct(closer) {
			return nil, p.errf("expected \",\" or %q in array, found %s", closer, p.describe())
		}
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if !hasKeys {
		list := make([]any, len(entries))
		for i, e := range entries {
			list[i] = e.val
		}
		return list, nil
	}
	m := make(map[string]any, len(entries))
	for _, e := range entries {
		m[e.key] = e.val
	}
	return m, nil
}

// arrayKey applies PHP's array key coercion. It returns the int key and
// isInt=true, or isInt=false when the key is the (string) value itself.
func arrayKey(v any) (int64, bool, error) {
	switch x := v.(type) {
	case string:
		if n, ok := canonicalInt(x); ok {
			return n, true, nil
		}
		return 0, false, nil
	case int64:
		return x, true, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, true, nil
		}
		return int64(x), true, nil
	case bool:
		if x {
			return 1, true, nil
		}
		return 0, true, nil
	case nil:
		return 0, false, fmt.Errorf("null array keys are not supported")
	}
	return 0, false, fmt.Errorf("illegal array key type %T", v)
}

// canonicalInt reports whether s is a canonical decimal integer as PHP
// would treat it as an integer array key ("5", "-3", but not "05" or "+1").
func canonicalInt(s string) (int64, bool) {
	if s == "" || len(s) > 20 {
		return 0, false
	}
	d := s
	if d[0] == '-' {
		d = d[1:]
	}
	if d == "" || (d[0] == '0' && len(d) > 1) || (s[0] == '-' && d == "0") {
		return 0, false
	}
	for i := 0; i < len(d); i++ {
		if d[i] < '0' || d[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != "" && x != "0"
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func toString(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case bool:
		if x {
			return "1", nil
		}
		return "", nil
	case string:
		return x, nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return formatFloat(x), nil
	}
	return "", fmt.Errorf("cannot convert array to string")
}

// formatFloat mimics PHP's float to string conversion with the default
// precision ini setting of 14 significant digits.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NAN"
	case math.IsInf(f, 1):
		return "INF"
	case math.IsInf(f, -1):
		return "-INF"
	}
	s := strconv.FormatFloat(f, 'G', 14, 64)
	if strings.Contains(s, "E") {
		mant, exp, _ := strings.Cut(s, "E")
		if strings.Contains(mant, ".") {
			mant = strings.TrimRight(strings.TrimRight(mant, "0"), ".")
		}
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		sign := "+"
		if exp[0] == '-' || exp[0] == '+' {
			if exp[0] == '-' {
				sign = "-"
			}
			exp = exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		return mant + "E" + sign + exp
	}
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func cast(typ string, v any) (any, error) {
	switch typ {
	case "bool":
		return truthy(v), nil
	case "string":
		return toString(v)
	case "int":
		switch x := v.(type) {
		case nil:
			return int64(0), nil
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case int64:
			return x, nil
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return int64(0), nil
			}
			return int64(x), nil
		case string:
			return stringToInt(x), nil
		case []any:
			return boolInt(len(x) > 0), nil
		case map[string]any:
			return boolInt(len(x) > 0), nil
		}
	case "float":
		switch x := v.(type) {
		case nil:
			return 0.0, nil
		case bool:
			if x {
				return 1.0, nil
			}
			return 0.0, nil
		case int64:
			return float64(x), nil
		case float64:
			return x, nil
		case string:
			return stringToFloat(x), nil
		case []any:
			return float64(boolInt(len(x) > 0)), nil
		case map[string]any:
			return float64(boolInt(len(x) > 0)), nil
		}
	}
	return nil, fmt.Errorf("unsupported cast (%s) of %T", typ, v)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// numericPrefix returns the leading numeric part of s (after leading whitespace).
func numericPrefix(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t\n\r\v\f")
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	isFloat := false
	if i < len(s) && s[i] == '.' {
		j := i + 1
		fd := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			fd++
		}
		if digits+fd > 0 {
			i = j
			digits += fd
			isFloat = true
		}
	}
	if digits == 0 {
		return "", false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && s[j] >= '0' && s[j] <= '9' {
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			i = j
			isFloat = true
		}
	}
	return s[:i], isFloat
}

func stringToInt(s string) int64 {
	num, isFloat := numericPrefix(s)
	if num == "" {
		return 0
	}
	if !isFloat {
		if n, err := strconv.ParseInt(num, 10, 64); err == nil {
			return n
		}
	}
	f, _ := strconv.ParseFloat(num, 64)
	if math.IsNaN(f) || math.IsInf(f, 0) || f >= math.MaxInt64 || f < math.MinInt64 {
		return 0
	}
	return int64(f)
}

func stringToFloat(s string) float64 {
	num, _ := numericPrefix(s)
	if num == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(num, 64)
	return f
}
