// Package ednlit parses a subset of Clojure EDN.
//
// The reference behavior is the Clojure 1.12.6 reader (clojure.lang.LispReader
// and clojure.lang.EdnReader), the version rig's resolver pins
// (resolver/deps.edn). ednlit rejects, with an explicit error, everything the
// JVM reader accepts that ednlit has no type for — big integers (N suffix),
// big decimals (M suffix), ratios, explicit radix — and everything out of
// scope for deps.edn-style files: character literals, tagged literals, and
// the reader macros other than quotes, sets and namespaced maps.
//
// Errors render as "ednlit: [name:]line:col: message" followed by the
// offending source line and a caret under the column.
package ednlit

import (
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Keyword struct{ NS, Name string }

// String renders the keyword. An empty NS with a name starting in "/" is the
// reader's form of "a namespace that is itself empty" (://a): it must render
// with the slash so the keyword round-trips.
func (k Keyword) String() string {
	if k.NS == "" && !strings.HasPrefix(k.Name, "/") {
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

// parseError is a parse failure at position pos of src. Error renders a
// one-line "ednlit: [name:]line:col: msg" header, then the offending source
// line with a caret under the column. Columns count runes, so caret
// alignment survives UTF-8; tabs in the line may misalign it.
type parseError struct {
	msg  string
	pos  int
	name string
	src  string
}

func (e *parseError) Error() string {
	line, col := lineCol(e.src, e.pos)
	hdr := "ednlit: "
	if e.name != "" {
		hdr += e.name + ":"
	}
	hdr += strconv.Itoa(line) + ":" + strconv.Itoa(col) + ": " + e.msg
	if snippet, caret := snippetAt(e.src, e.pos); snippet != "" {
		return hdr + "\n" + snippet + "\n" + caret
	}
	return hdr
}

// lineCol returns the 1-based line and rune column of pos in src.
func lineCol(src string, pos int) (line, col int) {
	line, col = 1, 1
	for i := 0; i < pos && i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			line, col = line+1, 1
		case c < 0x80:
			col++
		}
	}
	return
}

// snippetAt returns the source line containing pos and a caret line pointing
// at the column. It returns an empty snippet when the line is blank.
func snippetAt(src string, pos int) (snippet, caret string) {
	ls := 0
	for i := 0; i < pos && i < len(src); i++ {
		if src[i] == '\n' {
			ls = i + 1
		}
	}
	le := strings.IndexByte(src[ls:], '\n')
	if le < 0 {
		le = len(src)
	} else {
		le += ls
	}
	line := strings.TrimSuffix(src[ls:le], "\r")
	if line == "" {
		return "", ""
	}
	_, col := lineCol(src, pos)
	return line, strings.Repeat(" ", col-1) + "^"
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
	return &parseError{msg: msg, pos: pos, name: p.name, src: p.s}
}

// isWSAt reports whether the character at pos is whitespace or a comma,
// without consuming it and without treating ; as a comment. A position at or
// past the end is never whitespace.
func (p *parser) isWSAt(pos int) bool {
	if pos >= len(p.s) {
		return false
	}
	c := p.s[pos]
	if c == ',' {
		return true
	}
	if c < 0x80 {
		switch c {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return true
		}
		return false
	}
	r, _ := utf8.DecodeRuneInString(p.s[pos:])
	return isWSRune(r)
}

// isWSRune is the JVM's Character.isWhitespace.
func isWSRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	switch {
	case r >= 0x1C && r <= 0x1F,
		r == 0x85, r == 0xA0, r == 0x1680,
		r >= 0x2000 && r <= 0x200A,
		r == 0x2028, r == 0x2029, r == 0x3000:
		return true
	}
	return false
}

// skipWS skips whitespace and line comments.
func (p *parser) skipWS() {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch {
		case c == ';':
			for p.pos < len(p.s) && p.s[p.pos] != '\n' && p.s[p.pos] != '\r' {
				p.pos++
			}
			continue
		case !p.isWSAt(p.pos):
			return
		}
		if c >= 0x80 {
			_, size := utf8.DecodeRuneInString(p.s[p.pos:])
			p.pos += size
		} else {
			p.pos++
		}
	}
}

// peek returns the next non-whitespace character without consuming it.
func (p *parser) peek() (byte, bool) {
	p.skipWS()
	if p.pos >= len(p.s) {
		return 0, false
	}
	return p.s[p.pos], true
}

// skipDiscards consumes leading #_ discard forms. The discarded form is still
// read, so a syntax error in it is reported.
func (p *parser) skipDiscards() error {
	for {
		c, ok := p.peek()
		if !ok || c != '#' || p.pos+1 >= len(p.s) || p.s[p.pos+1] != '_' {
			return nil
		}
		p.pos += 2
		if _, err := p.value(); err != nil {
			return err
		}
	}
}

// value reads one form, at the current position.
func (p *parser) value() (any, error) {
	if err := p.skipDiscards(); err != nil {
		return nil, err
	}
	c, ok := p.peek()
	if !ok {
		return nil, p.fail("EOF while reading")
	}
	switch c {
	case '(':
		return p.list()
	case '[':
		return p.vector()
	case '{':
		return p.mapValue("")
	case '"':
		return p.string()
	case '#':
		return p.dispatch()
	case '\'':
		p.pos++
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		return Quoted{V: v}, nil
	case ')':
		return nil, p.failAt(p.pos, "unmatched delimiter: )")
	case ']':
		return nil, p.failAt(p.pos, "unmatched delimiter: ]")
	case '}':
		return nil, p.failAt(p.pos, "unmatched delimiter: }")
	case '\\':
		return nil, p.failAt(p.pos, "character literal not supported")
	case '@', '^', '`', '~':
		return nil, p.failAt(p.pos, "reader macro not supported: "+string(c))
	}
	return p.atom()
}

func (p *parser) list() (any, error) {
	p.pos++
	out := List{}
	for {
		if err := p.skipDiscards(); err != nil {
			return nil, err
		}
		c, ok := p.peek()
		if !ok {
			return nil, p.fail("EOF while reading")
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

func (p *parser) vector() (any, error) {
	p.pos++
	out := []any{}
	for {
		if err := p.skipDiscards(); err != nil {
			return nil, err
		}
		c, ok := p.peek()
		if !ok {
			return nil, p.fail("EOF while reading")
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

// mapValue reads a {…} map. When prefix is non-empty the map is a namespaced
// map: keyword and symbol keys without a namespace are qualified with
// prefix, and keys carrying the "_" namespace are unqualified.
func (p *parser) mapValue(prefix string) (any, error) {
	p.pos++
	out := Map{}
	seen := []any{}
	odd := "map literal must contain an even number of forms"
	if prefix != "" {
		odd = "namespaced map literal must contain an even number of forms"
	}
	for {
		if err := p.skipDiscards(); err != nil {
			return nil, err
		}
		c, ok := p.peek()
		if !ok {
			return nil, p.fail("EOF while reading")
		}
		if c == '}' {
			p.pos++
			return out, nil
		}
		kstart := p.pos
		k, err := p.value()
		if err != nil {
			return nil, err
		}
		if prefix != "" {
			switch t := k.(type) {
			case Keyword:
				switch t.NS {
				case "":
					t.NS = prefix
				case "_":
					t.NS = ""
				}
				k = t
			case Symbol:
				switch t.NS {
				case "":
					t.NS = prefix
				case "_":
					t.NS = ""
				}
				k = t
			}
		}
		c2, ok2 := p.peek()
		if !ok2 {
			return nil, p.fail("EOF while reading")
		}
		if c2 == '}' {
			return nil, p.failAt(p.pos, odd)
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		for _, s := range seen {
			if reflect.DeepEqual(s, k) {
				return nil, p.failAt(kstart, "duplicate key: "+keyStr(k))
			}
		}
		seen = append(seen, k)
		out = append(out, Pair{K: k, V: v})
	}
}

func (p *parser) set() (any, error) {
	p.pos++
	out := Set{}
	seen := []any{}
	for {
		if err := p.skipDiscards(); err != nil {
			return nil, err
		}
		c, ok := p.peek()
		if !ok {
			return nil, p.fail("EOF while reading")
		}
		if c == '}' {
			p.pos++
			return out, nil
		}
		vstart := p.pos
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		for _, s := range seen {
			if reflect.DeepEqual(s, v) {
				return nil, p.failAt(vstart, "duplicate key: "+keyStr(v))
			}
		}
		seen = append(seen, v)
		out = append(out, v)
	}
}

// dispatch reads a # form. Only #{ (sets), #: (namespaced maps) and ## (the
// symbolic values Inf, -Inf and NaN) are supported; #_ discards are handled
// before dispatch. Everything else is a tagged literal or a reader macro,
// which ednlit refuses.
func (p *parser) dispatch() (any, error) {
	hashPos := p.pos
	p.pos++
	if p.pos >= len(p.s) {
		return nil, p.failAt(hashPos, "EOF while reading character")
	}
	c := p.s[p.pos]
	switch c {
	case '{':
		return p.set()
	case ':':
		return p.namespacedMap()
	case '#':
		return p.symbolicValue(p.pos)
	}
	// A tag is read as a word and interpreted: a symbol tag has no reader
	// function here, anything else is not a tag at all.
	tagStart := p.pos
	if !(isLetterASCII(c) || c == '-') {
		return nil, p.failAt(tagStart, "reader tag must be a symbol")
	}
	p.pos++
	rest, _ := p.word()
	tag := string(c) + rest
	if isNumberish(tag) {
		if _, err := p.number(tag, tagStart); err != nil {
			return nil, err
		}
		return nil, p.failAt(tagStart, "reader tag must be a symbol")
	}
	switch tag {
	case "nil", "true", "false":
		return nil, p.failAt(tagStart, "reader tag must be a symbol")
	}
	v, err := p.symbolOrKeyword(tag, tagStart)
	if err != nil {
		return nil, err
	}
	if sym, ok := v.(Symbol); ok {
		return nil, p.failAt(tagStart, "no reader function for tag "+sym.String())
	}
	return nil, p.failAt(tagStart, "reader tag must be a symbol")
}

// symbolicValue reads a ## form: the double must be one of Inf, -Inf, NaN.
// pos is at the second '#'.
func (p *parser) symbolicValue(pos int) (any, error) {
	p.pos = pos + 1
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	if sym, ok := v.(Symbol); ok {
		switch sym.Name {
		case "Inf":
			return math.Inf(1), nil
		case "-Inf":
			return math.Inf(-1), nil
		case "NaN":
			return math.NaN(), nil
		}
		return nil, p.failAt(pos, "unknown symbolic value: ##"+sym.String())
	}
	return nil, p.failAt(pos, "invalid token: ##"+keyStr(v))
}

// namespacedMap reads a #:qualifier {…} form. dispatch has consumed the '#';
// p.pos is at the ':' of the qualifier.
func (p *parser) namespacedMap() (any, error) {
	qstart := p.pos
	p.pos++ // consume ':'
	auto := false
	if p.pos < len(p.s) && p.s[p.pos] == ':' {
		auto = true
		p.pos++
	}
	if p.pos >= len(p.s) {
		// "#:" or "#::" at EOF: the JVM still tries to read the missing
		// qualifier.
		return nil, p.failAt(len(p.s), "EOF while reading")
	}
	var sym any
	hasQual := false
	switch {
	case p.isWSAt(p.pos):
		// "#: {…}" or "#:: {…}".
		if !auto {
			return nil, p.failAt(qstart, "namespaced map must specify a namespace")
		}
		p.skipRawWS()
		if p.pos >= len(p.s) || p.s[p.pos] != '{' {
			return nil, p.failAt(qstart, "namespaced map must specify a namespace")
		}
	case p.s[p.pos] != '{':
		var err error
		sym, err = p.value()
		if err != nil {
			return nil, err
		}
		hasQual = true
		p.skipRawWS()
		if p.pos >= len(p.s) || p.s[p.pos] != '{' {
			return nil, p.failAt(qstart, "namespaced map must specify a map")
		}
	}
	var prefix string
	switch {
	case auto && !hasQual:
		// "#:: {…}" — no alias: the current namespace, "user" for a
		// deps.edn.
		prefix = "user"
	case auto && hasQual:
		s, ok := sym.(Symbol)
		if !ok || s.NS != "" {
			return nil, p.failAt(qstart, "namespaced map must specify a valid namespace: "+keyStr(sym))
		}
		// No aliases are resolvable here.
		return nil, p.failAt(qstart, "unknown auto-resolved namespace alias: "+s.String())
	case !auto && !hasQual:
		return nil, p.failAt(qstart, "namespaced map must specify a valid namespace: nil")
	default:
		s, ok := sym.(Symbol)
		if !ok || s.NS != "" {
			return nil, p.failAt(qstart, "namespaced map must specify a valid namespace: "+keyStr(sym))
		}
		prefix = s.Name
	}
	return p.mapValue(prefix)
}

// skipRawWS skips whitespace and commas, the JVM's per-character isWhitespace
// scan, without reading comments.
func (p *parser) skipRawWS() {
	for p.pos < len(p.s) && p.isWSAt(p.pos) {
		if p.s[p.pos] >= 0x80 {
			_, size := utf8.DecodeRuneInString(p.s[p.pos:])
			p.pos += size
		} else {
			p.pos++
		}
	}
}

// string reads a "…" string, including multi-line strings and escapes.
func (p *parser) string() (any, error) {
	p.pos++ // consume '"'
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c == '\\':
			p.pos++
			if p.pos >= len(p.s) {
				return nil, p.failAt(len(p.s), "EOF while reading string")
			}
			e := p.s[p.pos]
			switch e {
			case 't':
				b.WriteByte('\t')
				p.pos++
			case 'r':
				b.WriteByte('\r')
				p.pos++
			case 'n':
				b.WriteByte('\n')
				p.pos++
			case 'b':
				b.WriteByte('\b')
				p.pos++
			case 'f':
				b.WriteByte('\f')
				p.pos++
			case '"', '\\':
				b.WriteByte(e)
				p.pos++
			case 'u':
				r, err := p.unicodeEscape(p.pos - 1)
				if err != nil {
					return nil, err
				}
				b.WriteRune(r)
			default:
				if isDigit(e) {
					n, err := p.octalEscape(p.pos - 1)
					if err != nil {
						return nil, err
					}
					b.WriteByte(byte(n))
				} else {
					return nil, p.failAt(p.pos, "unsupported escape character: \\"+string(e))
				}
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	return nil, p.failAt(len(p.s), "EOF while reading string")
}

// unicodeEscape reads a \uXXXX escape. p.pos is at 'u'; the returned rune is
// a single 16-bit unit, and lone surrogates are accepted, as in the JVM.
func (p *parser) unicodeEscape(backslash int) (rune, error) {
	p.pos++ // consume 'u'
	if p.pos >= len(p.s) {
		return 0, p.failAt(len(p.s), "EOF while reading string")
	}
	first := p.s[p.pos]
	if !isHexDigit(first) {
		return 0, p.failAt(p.pos, "invalid unicode escape: \\u"+string(first))
	}
	uc := int(hexValue(first))
	p.pos++
	digits := 1
	for digits < 4 && p.pos < len(p.s) {
		c := p.s[p.pos]
		if p.isWSAt(p.pos) || isMacroChar(c) {
			break
		}
		if !isHexDigit(c) {
			return 0, p.failAt(p.pos, "invalid digit: "+string(c))
		}
		uc = uc*16 + int(hexValue(c))
		p.pos++
		digits++
	}
	if digits < 4 {
		return 0, p.failAt(backslash, "invalid character length: "+strconv.Itoa(digits)+", should be: 4")
	}
	return rune(uc), nil
}

// octalEscape reads a \NNN octal escape: at most three octal digits, value in
// [0, 377]. p.pos is at the first digit. A fourth digit stays literal, as in
// the JVM.
func (p *parser) octalEscape(backslash int) (int, error) {
	first := p.s[p.pos]
	if !isOctalDigit(first) {
		return 0, p.failAt(p.pos, "invalid digit: "+string(first))
	}
	uc := int(first - '0')
	p.pos++
	digits := 1
	for digits < 3 && p.pos < len(p.s) {
		c := p.s[p.pos]
		if p.isWSAt(p.pos) || isMacroChar(c) {
			break
		}
		if !isOctalDigit(c) {
			return 0, p.failAt(p.pos, "invalid digit: "+string(c))
		}
		uc = uc*8 + int(c-'0')
		p.pos++
		digits++
	}
	if uc > 0o377 {
		return 0, p.failAt(backslash, "octal escape sequence must be in range [0, 377]")
	}
	return uc, nil
}

// atom reads a bare word: a number, one of the literals nil/true/false, or a
// symbol/keyword.
func (p *parser) atom() (any, error) {
	start := p.pos
	if p.isNumberishAt(start) {
		return p.number(p.numberToken(), start)
	}
	word, _ := p.word()
	switch word {
	case "nil":
		return nil, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return p.symbolOrKeyword(word, start)
}

// isNumberishAt reports whether the token at pos enters the number syntax:
// it starts with a digit, or with a sign followed by a digit. "+a" and ".5"
// are symbols.
func (p *parser) isNumberishAt(pos int) bool {
	if pos >= len(p.s) {
		return false
	}
	c := p.s[pos]
	if isDigit(c) {
		return true
	}
	if c == '+' || c == '-' {
		return pos+1 < len(p.s) && isDigit(p.s[pos+1])
	}
	return false
}

// numberToken reads a numeric literal: like word, but it stops at any macro
// character, including '#', the quote character and '%', as in the JVM's
// readNumber.
func (p *parser) numberToken() string {
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if p.isWSAt(p.pos) || isMacroChar(c) {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos]
}

// symbolPat and arraySymbolPat are the Clojure 1.12.6 patterns, in RE2 form.
var (
	symbolRe      = regexp.MustCompile(`^:?(?:([^\d/].*/)?)(/|[^\d/][^/]*)$`)
	arraySymbolRe = regexp.MustCompile(`^([^\d/:].*)/([1-9])$`)
)

// symbolOrKeyword validates word as a symbol or keyword and splits it into
// namespace and name at the first "/".
func (p *parser) symbolOrKeyword(word string, start int) (any, error) {
	if m := symbolRe.FindStringSubmatch(word); m != nil {
		ns, name := m[1], m[2]
		if (ns != "" && strings.HasSuffix(ns, ":/")) ||
			strings.HasSuffix(name, ":") ||
			strings.Contains(word[1:], "::") {
			return nil, p.failAt(start, "invalid token: "+word)
		}
		if strings.HasPrefix(word, "::") {
			inner := word[2:]
			if i := strings.IndexByte(inner, '/'); i > 0 {
				// ::a/b would need an alias to resolve; there are none.
				return nil, p.failAt(start, "invalid token: "+word)
			}
			return Keyword{NS: "user", Name: inner}, nil
		}
		if strings.HasPrefix(word, ":") {
			ns2, name2 := splitFirst(word[1:])
			return Keyword{NS: ns2, Name: name2}, nil
		}
		ns2, name2 := splitFirst(word)
		return Symbol{NS: ns2, Name: name2}, nil
	}
	if m := arraySymbolRe.FindStringSubmatch(word); m != nil {
		// a/1 — array-index notation; only symbols take it, never keywords.
		return Symbol{NS: m[1], Name: m[2]}, nil
	}
	return nil, p.failAt(start, "invalid token: "+word)
}

// splitFirst splits s into namespace and name at the first "/", the way the
// JVM's Symbol.intern does. A leading slash leaves the namespace empty and
// the name keeps the slash, so ://a round-trips as the keyword with an empty
// namespace and name "/a".
func splitFirst(s string) (ns, name string) {
	i := strings.IndexByte(s, '/')
	if i == 0 {
		if len(s) == 1 {
			return "", "/"
		}
		return "", s[1:]
	}
	if i < 0 {
		return "", s
	}
	return s[:i], s[i+1:]
}

// number reads a numeric literal. It mirrors the Clojure reader's intPat,
// floatPat and ratioPat, except that ednlit has no BigInt/BigDecimal/Ratio
// types: the N and M suffixes, ratios and explicit radix are refused with an
// explicit error, and integers are bounded to int64.
func (p *parser) number(word string, start int) (any, error) {
	fail := func(msg string) *parseError { return p.failAt(start, msg) }
	s := word
	sign := ""
	if s[0] == '+' || s[0] == '-' {
		sign, s = string(s[0]), s[1:]
	}
	// Underscores are not JVM number syntax; Go's ParseFloat would accept
	// them.
	if strings.ContainsRune(s, '_') {
		return nil, fail("invalid number: " + word)
	}
	// 2r10 — explicit radix: valid JVM syntax, no corresponding type here.
	if matchRadix(s) {
		return nil, fail("number with explicit radix not supported")
	}
	// 123N — big integer.
	if strings.HasSuffix(s, "N") && validInt(s[:len(s)-1]) {
		return nil, fail("big integer literal not supported")
	}
	// 1.5M — big decimal. The base must be decimal float syntax; 0x10M is
	// not a float in the JVM either and falls through to "invalid number".
	if strings.HasSuffix(s, "M") && validDecimal(s[:len(s)-1]) {
		return nil, fail("big decimal literal not supported")
	}
	// 1/2 — ratio.
	if i := strings.IndexByte(s, '/'); i > 0 && i == len(s)-2 {
		if allDigits(s[:i]) && allDigits(s[i+1:]) {
			return nil, fail("ratio literal not supported")
		}
	}
	// 0x1F — hexadecimal.
	if len(s) >= 3 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		h := s[2:]
		if !allHex(h) {
			return nil, fail("invalid number: " + word)
		}
		if sign == "-" && h == "8000000000000000" {
			return int64(math.MinInt64), nil
		}
		n, err := strconv.ParseInt(h, 16, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return nil, fail("integer out of 64-bit range")
			}
			return nil, fail("invalid number: " + word)
		}
		if sign == "-" {
			return -n, nil
		}
		return n, nil
	}
	// 017 — octal: a leading zero marks the base, and the digits must be
	// octal (08 is not a number).
	if len(s) > 1 && s[0] == '0' && allDigits(s) {
		if !allOctal(s) {
			return nil, fail("invalid number: " + word)
		}
		n, err := strconv.ParseInt(s, 8, 64)
		if err != nil {
			return nil, fail("integer out of 64-bit range")
		}
		if sign == "-" {
			return -n, nil
		}
		return n, nil
	}
	// decimal integer.
	if allDigits(s) {
		w := s
		if sign != "" {
			w = sign + s
		}
		if w == "-9223372036854775808" {
			return int64(math.MinInt64), nil
		}
		n, err := strconv.ParseInt(w, 10, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return nil, fail("integer out of 64-bit range")
			}
			return nil, fail("invalid number: " + word)
		}
		return n, nil
	}
	// float: digits, optional ".digits", optional exponent.
	w := s
	if sign != "" {
		w = sign + s
	}
	if f, err := strconv.ParseFloat(w, 64); err == nil {
		return f, nil
	}
	return nil, fail("invalid number: " + word)
}

// matchRadix is the JVM's radix integer alternative: [1-9][0-9]?[rR][0-9A-Za-z]+.
func matchRadix(s string) bool {
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	if n < 1 || n > 2 || s[0] == '0' || n >= len(s) {
		return false
	}
	if s[n] != 'r' && s[n] != 'R' {
		return false
	}
	if n+1 >= len(s) {
		return false
	}
	for i := n + 1; i < len(s); i++ {
		if !isDigit(s[i]) && !isLetterASCII(s[i]) {
			return false
		}
	}
	return true
}

// validInt is the JVM's integer syntax minus the bare "0[0-9]+" fallthrough
// that matchNumber rejects: a zero, a decimal, a hex, or an octal integer.
func validInt(s string) bool {
	switch {
	case s == "0":
		return true
	case s[0] >= '1' && s[0] <= '9' && allDigits(s):
		return true
	case len(s) >= 3 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') && allHex(s[2:]):
		return true
	case len(s) >= 2 && s[0] == '0' && allOctal(s):
		return true
	}
	return false
}

// validDecimal is the JVM's floatPat base: decimal digits, an optional
// ".digits", and an optional exponent with at least one digit.
func validDecimal(s string) bool {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == 0 {
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}

// isNumberish reports whether a word enters the number syntax: it starts with
// a digit, or with a sign followed by a digit. "+a" and ".5" are symbols.
func isNumberish(w string) bool {
	switch w[0] {
	case '+', '-':
		return len(w) > 1 && isDigit(w[1])
	default:
		return isDigit(w[0])
	}
}

// word reads a run of bytes until whitespace or a terminating macro. '#',
// the quote character and '%' continue a word, as in the JVM.
func (p *parser) word() (string, int) {
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if p.isWSAt(p.pos) || isTerminatingMacro(c) {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos], start
}

// isTerminatingMacro is the JVM's full macro set minus '#', the quote
// character and '%', which continue a symbol or keyword.
func isTerminatingMacro(c byte) bool {
	switch c {
	case '(', ')', '[', ']', '{', '}', '"', '\\', ';', '@', '^', '`', '~':
		return true
	}
	return false
}

// isMacroChar is the JVM's full macro set, the stop set for string escapes.
func isMacroChar(c byte) bool {
	switch c {
	case '(', ')', '[', ']', '{', '}', '"', '\\', ';', '@', '^', '`', '~',
		'#', '\'', '%':
		return true
	}
	return false
}

// keyStr renders a value for error messages, the way the JVM renders it in
// "Duplicate key:" and friends: strings raw, collections via Format.
func keyStr(k any) string {
	switch t := k.(type) {
	case Keyword:
		return t.String()
	case Symbol:
		return t.String()
	case string:
		return t
	case nil:
		return "nil"
	case Quoted:
		return "(quote " + keyStr(t.V) + ")"
	default:
		return Format(t)
	}
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isOctalDigit(c byte) bool {
	return c >= '0' && c <= '7'
}

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

func isLetterASCII(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func allOctal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isOctalDigit(s[i]) {
			return false
		}
	}
	return true
}

func allHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}
