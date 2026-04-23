package zenoh

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// parseJSON5 parses a single JSON5 value from s and returns the Go native
// form (map[string]any, []any, string, int64, float64, bool, nil).
//
// Supported JSON5 features: line (//) and block (/* */) comments, unquoted
// identifier keys, single-quoted strings, trailing commas in objects and
// arrays, hex integer literals (0x...), and leading / trailing decimal points.
// NaN / Infinity / unicode line separators are not accepted — use plain
// numbers instead.
func parseJSON5(s string) (any, error) {
	p := &json5Parser{src: s}
	p.skip()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.pos != len(p.src) {
		return nil, p.errf("unexpected trailing input")
	}
	return v, nil
}

type json5Parser struct {
	src string
	pos int
}

func (p *json5Parser) errf(format string, args ...any) error {
	line, col := p.lineCol()
	return fmt.Errorf("json5: line %d col %d: %s", line, col, fmt.Sprintf(format, args...))
}

func (p *json5Parser) lineCol() (int, int) {
	line, col := 1, 1
	for i := 0; i < p.pos && i < len(p.src); i++ {
		if p.src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// skip advances past whitespace and comments.
func (p *json5Parser) skip() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/':
			p.pos += 2
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '*':
			p.pos += 2
			for p.pos+1 < len(p.src) && !(p.src[p.pos] == '*' && p.src[p.pos+1] == '/') {
				p.pos++
			}
			if p.pos+1 >= len(p.src) {
				return
			}
			p.pos += 2
		default:
			return
		}
	}
}

func (p *json5Parser) peek() byte {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *json5Parser) value() (any, error) {
	if p.pos >= len(p.src) {
		return nil, p.errf("unexpected end of input")
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"' || c == '\'':
		return p.string()
	case c == 't' || c == 'f':
		return p.bool()
	case c == 'n':
		return p.null()
	case c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, p.errf("unexpected character %q", c)
	}
}

func (p *json5Parser) object() (map[string]any, error) {
	p.pos++ // '{'
	out := map[string]any{}
	p.skip()
	if p.peek() == '}' {
		p.pos++
		return out, nil
	}
	for {
		p.skip()
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		p.skip()
		if p.peek() != ':' {
			return nil, p.errf("expected ':' after key %q", key)
		}
		p.pos++
		p.skip()
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		out[key] = val
		p.skip()
		switch p.peek() {
		case ',':
			p.pos++
			p.skip()
			if p.peek() == '}' {
				p.pos++
				return out, nil
			}
		case '}':
			p.pos++
			return out, nil
		default:
			return nil, p.errf("expected ',' or '}' in object")
		}
	}
}

func (p *json5Parser) array() ([]any, error) {
	p.pos++ // '['
	var out []any
	p.skip()
	if p.peek() == ']' {
		p.pos++
		return out, nil
	}
	for {
		p.skip()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skip()
		switch p.peek() {
		case ',':
			p.pos++
			p.skip()
			if p.peek() == ']' {
				p.pos++
				return out, nil
			}
		case ']':
			p.pos++
			return out, nil
		default:
			return nil, p.errf("expected ',' or ']' in array")
		}
	}
}

func (p *json5Parser) key() (string, error) {
	c := p.peek()
	if c == '"' || c == '\'' {
		return p.string()
	}
	// unquoted identifier: letters, digits (not first), '_', '$'.
	start := p.pos
	for p.pos < len(p.src) {
		r, w := utf8.DecodeRuneInString(p.src[p.pos:])
		if unicode.IsLetter(r) || r == '_' || r == '$' ||
			(p.pos > start && unicode.IsDigit(r)) {
			p.pos += w
			continue
		}
		break
	}
	if p.pos == start {
		return "", p.errf("expected object key")
	}
	return p.src[start:p.pos], nil
}

func (p *json5Parser) string() (string, error) {
	quote := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == quote {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.src) {
				return "", p.errf("unterminated escape")
			}
			esc := p.src[p.pos]
			p.pos++
			switch esc {
			case '"', '\'', '\\', '/':
				b.WriteByte(esc)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '0':
				b.WriteByte(0)
			case '\n': // line continuation
			case 'u':
				if p.pos+4 > len(p.src) {
					return "", p.errf("truncated \\u escape")
				}
				n, err := strconv.ParseUint(p.src[p.pos:p.pos+4], 16, 32)
				if err != nil {
					return "", p.errf("bad \\u escape: %v", err)
				}
				b.WriteRune(rune(n))
				p.pos += 4
			case 'x':
				if p.pos+2 > len(p.src) {
					return "", p.errf("truncated \\x escape")
				}
				n, err := strconv.ParseUint(p.src[p.pos:p.pos+2], 16, 8)
				if err != nil {
					return "", p.errf("bad \\x escape: %v", err)
				}
				b.WriteByte(byte(n))
				p.pos += 2
			default:
				return "", p.errf("unknown escape \\%c", esc)
			}
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", p.errf("unterminated string")
}

func (p *json5Parser) bool() (bool, error) {
	switch {
	case strings.HasPrefix(p.src[p.pos:], "true"):
		p.pos += 4
		return true, nil
	case strings.HasPrefix(p.src[p.pos:], "false"):
		p.pos += 5
		return false, nil
	}
	return false, p.errf("invalid literal")
}

func (p *json5Parser) null() (any, error) {
	if strings.HasPrefix(p.src[p.pos:], "null") {
		p.pos += 4
		return nil, nil
	}
	return nil, p.errf("invalid literal")
}

func (p *json5Parser) number() (any, error) {
	start := p.pos
	// optional sign
	if c := p.peek(); c == '+' || c == '-' {
		p.pos++
	}
	// hex
	if p.pos+1 < len(p.src) && p.src[p.pos] == '0' &&
		(p.src[p.pos+1] == 'x' || p.src[p.pos+1] == 'X') {
		p.pos += 2
		hexStart := p.pos
		for p.pos < len(p.src) && isHexDigit(p.src[p.pos]) {
			p.pos++
		}
		if p.pos == hexStart {
			return nil, p.errf("empty hex literal")
		}
		v, err := strconv.ParseInt(p.src[start:p.pos], 0, 64)
		if err != nil {
			return nil, p.errf("hex parse: %v", err)
		}
		return v, nil
	}
	isFloat := false
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c >= '0' && c <= '9':
			p.pos++
		case c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-':
			isFloat = true
			p.pos++
		default:
			goto done
		}
	}
done:
	lit := p.src[start:p.pos]
	if lit == "" || lit == "+" || lit == "-" {
		return nil, p.errf("empty number")
	}
	if isFloat {
		v, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return nil, p.errf("float parse: %v", err)
		}
		return v, nil
	}
	v, err := strconv.ParseInt(lit, 10, 64)
	if err != nil {
		// fall back to float for huge integers
		f, ferr := strconv.ParseFloat(lit, 64)
		if ferr == nil {
			return f, nil
		}
		return nil, p.errf("int parse: %v", err)
	}
	return v, nil
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
