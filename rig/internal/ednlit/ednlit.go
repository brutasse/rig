package ednlit

import (
	"fmt"
	"strconv"
	"strings"
)

type Keyword struct{ NS, Name string }

func (k Keyword) String() string {
	if k.NS == "" {
		return ":" + k.Name
	}
	return ":" + k.NS + "/" + k.Name
}

type Symbol struct{ NS, Name string }

func (s Symbol) String() string {
	if s.NS == "" {
		return s.Name
	}
	return s.NS + "/" + s.Name
}

type Quoted struct{ V any }

type List []any

type Pair struct{ K, V any }

type Map []Pair

type Set []any

type parseError struct {
	msg  string
	pos  int
	name string
}

func (e *parseError) Error() string {
	if e.name != "" {
		return fmt.Sprintf("ednlit: %s at %d in %s", e.msg, e.pos, e.name)
	}
	return fmt.Sprintf("ednlit: %s at %d", e.msg, e.pos)
}

func Parse(s string) (any, error) {
	return parse("", s)
}

// ParseNamed is Parse, with name included in any error message.
func ParseNamed(name, s string) (any, error) {
	return parse(name, s)
}

func parse(name, s string) (any, error) {
	p := &parser{name: name, s: s}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.s) {
		return nil, p.fail("trailing input")
	}
	return v, nil
}

type parser struct {
	name string
	s    string
	pos  int
}

func (p *parser) fail(msg string) *parseError {
	return p.failAt(p.pos, msg)
}

func (p *parser) failAt(pos int, msg string) *parseError {
	return &parseError{msg: msg, pos: pos, name: p.name}
}

func (p *parser) skipWS() {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		if c == ';' {
			// line comment
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		return
	}
}

func (p *parser) peek() (byte, bool) {
	p.skipWS()
	if p.pos >= len(p.s) {
		return 0, false
	}
	return p.s[p.pos], true
}

func (p *parser) value() (any, error) {
	c, ok := p.peek()
	if !ok {
		return nil, p.fail("unexpected end of input")
	}
	switch c {
	case '"':
		return p.string()
	case ':':
		return p.keyword()
	case '[':
		return p.vector()
	case '(':
		return p.list()
	case '{':
		return p.mapValue()
	case '#':
		return p.dispatch()
	case '\'':
		p.pos++
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		return Quoted{V: v}, nil
	}
	return p.atom()
}

func (p *parser) vector() (any, error) {
	p.pos++
	out := []any{}
	for {
		c, ok := p.peek()
		if !ok {
			return nil, p.fail("unclosed vector")
		}
		if c == ']' {
			p.pos++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (p *parser) list() (any, error) {
	p.pos++
	out := List{}
	for {
		c, ok := p.peek()
		if !ok {
			return nil, p.fail("unclosed list")
		}
		if c == ')' {
			p.pos++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (p *parser) mapValue() (any, error) {
	p.pos++
	out := Map{}
	for {
		c, ok := p.peek()
		if !ok {
			return nil, p.fail("unclosed map")
		}
		if c == '}' {
			p.pos++
			return out, nil
		}
		k, err := p.key()
		if err != nil {
			return nil, err
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, Pair{K: k, V: v})
	}
}

func (p *parser) key() (any, error) {
	c, ok := p.peek()
	if !ok {
		return nil, p.fail("unexpected end of input in map")
	}
	switch c {
	case '"':
		return p.string()
	case ':':
		return p.keyword()
	case '[', '(', '{', '#', '\'':
		return nil, p.fail("collection as map key")
	}
	return p.atom()
}

func (p *parser) dispatch() (any, error) {
	p.pos++
	if c, ok := p.peek(); ok && c == '{' {
		p.pos++
		out := Set{}
		for {
			c, ok := p.peek()
			if !ok {
				return nil, p.fail("unclosed set")
			}
			if c == '}' {
				p.pos++
				return out, nil
			}
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return nil, p.failAt(p.pos-1, "unsupported dispatch")
}

func (p *parser) string() (any, error) {
	p.pos++
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch c {
		case '"':
			p.pos++
			return b.String(), nil
		case '\\':
			p.pos++
			if p.pos >= len(p.s) {
				return nil, p.fail("unterminated escape")
			}
			e := p.s[p.pos]
			switch e {
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
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'u':
				if p.pos+4 >= len(p.s) {
					return nil, p.fail("bad unicode escape")
				}
				n, err := strconv.ParseUint(p.s[p.pos+1:p.pos+5], 16, 32)
				if err != nil {
					return nil, p.fail("bad unicode escape")
				}
				b.WriteRune(rune(n))
				p.pos += 4
			default:
				return nil, p.fail("bad escape")
			}
			p.pos++
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	return nil, p.fail("unterminated string")
}

func (p *parser) keyword() (any, error) {
	p.pos++
	if p.pos < len(p.s) && p.s[p.pos] == '\'' {
		p.pos++
	}
	word, pos := p.word()
	if word == "" {
		return nil, p.failAt(pos, "empty keyword")
	}
	ns, name := splitName(word)
	return Keyword{NS: ns, Name: name}, nil
}

func (p *parser) word() (string, int) {
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
			c == '[' || c == ']' || c == '(' || c == ')' ||
			c == '{' || c == '}' || c == '"' || c == ';' || c == ',' {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos], start
}

func (p *parser) atom() (any, error) {
	word, start := p.word()
	if word == "" {
		return nil, p.failAt(start, "empty atom")
	}
	if strings.ContainsRune(word, '/') && isKeywordLike(word) {
		ns, name := splitName(word)
		return Symbol{NS: ns, Name: name}, nil
	}
	switch word {
	case "nil", "null":
		return nil, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if n, err := strconv.ParseInt(strings.ReplaceAll(word, "_", ""), 10, 64); err == nil {
		return n, nil
	}
	if !strings.ContainsAny(word, "/") {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(word, "_", ""), 64); err == nil {
			return f, nil
		}
	}
	ns, name := splitName(word)
	return Symbol{NS: ns, Name: name}, nil
}

func isKeywordLike(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i > 0 && i < len(s)-1
		}
	}
	return false
}

func splitName(s string) (ns, name string) {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return "", s
	}
	return s[:i], s[i+1:]
}
