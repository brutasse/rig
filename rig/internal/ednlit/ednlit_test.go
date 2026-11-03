package ednlit

import (
	"math"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`nil`, nil},
		{`null`, Symbol{Name: "null"}},
		{`true`, true},
		{`false`, false},
		{`42`, int64(42)},
		{`-7`, int64(-7)},
		{`+3`, int64(3)},
		{`0x1F`, int64(31)},
		{`-0x1F`, int64(-31)},
		{`+0x10`, int64(16)},
		{`017`, int64(15)},
		{`00`, int64(0)},
		{`000`, int64(0)},
		{`-9223372036854775808`, int64(-9223372036854775808)},
		{`-0x8000000000000000`, int64(-9223372036854775808)},
		{`1.5`, 1.5},
		{`-0.25`, -0.25},
		{`1e3`, 1000.0},
		{`1.e5`, 100000.0},
		{`0e5`, 0.0},
		{`5e-3`, 0.005},
		{`##Inf`, math.Inf(1)},
		{`##-Inf`, math.Inf(-1)},
		{`:a`, Keyword{Name: "a"}},
		{`:a/b`, Keyword{NS: "a", Name: "b"}},
		{`:1`, Keyword{Name: "1"}},
		{`:1a`, Keyword{Name: "1a"}},
		{`:1/a`, Keyword{NS: "1", Name: "a"}},
		{`:'quoted-kw`, Keyword{Name: "'quoted-kw"}},
		{`:-/a`, Keyword{NS: "-", Name: "a"}},
		{`:a/b/c`, Keyword{NS: "a", Name: "b/c"}},
		{`://a`, Keyword{NS: "", Name: "/a"}},
		{`::foo`, Keyword{NS: "user", Name: "foo"}},
		{`:kaocha.filter/focus`, Keyword{NS: "kaocha.filter", Name: "focus"}},
		{`/`, Symbol{Name: "/"}},
		{`:-`, Keyword{Name: "-"}},
		{`a:-`, Symbol{Name: "a:-"}},
		{`.5`, Symbol{Name: ".5"}},
		{`a//b`, Symbol{NS: "a", Name: "/b"}},
		{`a/b/c`, Symbol{NS: "a", Name: "b/c"}},
		{`a/1`, Symbol{NS: "a", Name: "1"}},
		{`abc:def/ghi:jkl.mno`, Symbol{NS: "abc:def", Name: "ghi:jkl.mno"}},
		{`"hi"`, "hi"},
		{`"a\nb"`, "a\nb"},
		{`"tab\there"`, "tab\there"},
		{`"quote\"back\\slash"`, `quote"back\slash`},
		{`"uni:é"`, "uni:é"},
		{`[1 2 3]`, []any{int64(1), int64(2), int64(3)}},
		{`[]`, []any{}},
		{`'[:unit]`, Quoted{V: []any{Keyword{Name: "unit"}}}},
		{`'a.b/c`, Quoted{V: Symbol{NS: "a.b", Name: "c"}}},
		{`(quote a)`, List{Symbol{Name: "quote"}, Symbol{Name: "a"}}},
		{`{}`, Map{}},
		{`{:a 1 :b/c "x"}`, Map{{K: Keyword{Name: "a"}, V: int64(1)}, {K: Keyword{NS: "b", Name: "c"}, V: "x"}}},
		{`{"s" 2}`, Map{{K: "s", V: int64(2)}}},
		{`#{:a :b}`, Set{Keyword{Name: "a"}, Keyword{Name: "b"}}},
		{`"  "`, "  "},
		{`  :a  `, Keyword{Name: "a"}},
		{`org.clojure/clojure`, Symbol{NS: "org.clojure", Name: "clojure"}},
		{`sym`, Symbol{Name: "sym"}},
		{`{:kaocha.filter/focus '[:unit]}`, Map{{K: Keyword{NS: "kaocha.filter", Name: "focus"}, V: Quoted{V: []any{Keyword{Name: "unit"}}}}}},
		{`[[1] [2 3]]`, []any{[]any{int64(1)}, []any{int64(2), int64(3)}}},
		// line comments
		{`1 ; the answer`, int64(1)},
		{"[1 ; first\n2 ; second\n3]", []any{int64(1), int64(2), int64(3)}},
		{"{:a 1\n;; whole line\n:b 2}", Map{{K: Keyword{Name: "a"}, V: int64(1)}, {K: Keyword{Name: "b"}, V: int64(2)}}},
		{"{1 2 ; } , { delims\n3 4}", Map{{K: int64(1), V: int64(2)}, {K: int64(3), V: int64(4)}}},
		{"{:a 1 ; tail\n:b 2}", Map{{K: Keyword{Name: "a"}, V: int64(1)}, {K: Keyword{Name: "b"}, V: int64(2)}}},
		{`{1 2} ; trailing, no newline`, Map{{K: int64(1), V: int64(2)}}},
		{`"not ; a comment"`, "not ; a comment"},
		// commas are whitespace
		{`[1, 2, 3]`, []any{int64(1), int64(2), int64(3)}},
		{`[1,2,]`, []any{int64(1), int64(2)}},
		{`{:a 1, :b 2}`, Map{{K: Keyword{Name: "a"}, V: int64(1)}, {K: Keyword{Name: "b"}, V: int64(2)}}},
		{`{a b, c d}`, Map{{K: Symbol{Name: "a"}, V: Symbol{Name: "b"}}, {K: Symbol{Name: "c"}, V: Symbol{Name: "d"}}}},
		{`{:a [1, 2], :b #{3,}}`, Map{{K: Keyword{Name: "a"}, V: []any{int64(1), int64(2)}}, {K: Keyword{Name: "b"}, V: Set{int64(3)}}}},
		// namespaced maps (the `#:qualifier {…}` reader macro)
		{`#:mvn{:version "1.0"}`, Map{{K: Keyword{NS: "mvn", Name: "version"}, V: "1.0"}}},
		{`#:mvn{:version "1.0" :classifier "sources"}`, Map{{K: Keyword{NS: "mvn", Name: "version"}, V: "1.0"}, {K: Keyword{NS: "mvn", Name: "classifier"}, V: "sources"}}},
		{`#:git{:url "git://x" :sha "abc"}`, Map{{K: Keyword{NS: "git", Name: "url"}, V: "git://x"}, {K: Keyword{NS: "git", Name: "sha"}, V: "abc"}}},
		{`#:local{:root "modules/core"}`, Map{{K: Keyword{NS: "local", Name: "root"}, V: "modules/core"}}},
		{`#:mvn {:version "1.0"}`, Map{{K: Keyword{NS: "mvn", Name: "version"}, V: "1.0"}}},
		{`#:: {:a 1}`, Map{{K: Keyword{NS: "user", Name: "a"}, V: int64(1)}}},
		{`#:a{:b 1}`, Map{{K: Keyword{NS: "a", Name: "b"}, V: int64(1)}}},
		{`#:a{:x/y 1}`, Map{{K: Keyword{NS: "x", Name: "y"}, V: int64(1)}}},
		{`#:a{b/c 1}`, Map{{K: Symbol{NS: "b", Name: "c"}, V: int64(1)}}},
		{`#:a{b 1 c 2}`, Map{{K: Symbol{NS: "a", Name: "b"}, V: int64(1)}, {K: Symbol{NS: "a", Name: "c"}, V: int64(2)}}},
		{`#:a{_/b 1}`, Map{{K: Symbol{Name: "b"}, V: int64(1)}}},
		{`{:deps {a/b #:mvn{:version "1.11.1"}}}`, Map{{K: Keyword{Name: "deps"}, V: Map{{K: Symbol{NS: "a", Name: "b"}, V: Map{{K: Keyword{NS: "mvn", Name: "version"}, V: "1.11.1"}}}}}}},
		// map and set keys can be any value
		{`{'a 1}`, Map{{K: Quoted{V: Symbol{Name: "a"}}, V: int64(1)}}},
		{`{[1 2] 3}`, Map{{K: []any{int64(1), int64(2)}, V: int64(3)}}},
		{`{#{} 1}`, Map{{K: Set{}, V: int64(1)}}},
		// string escapes
		{`"a\0003"`, "a\x003"},
		{`"a\0777"`, "a\0777"},
		{`"a\03%"`, "a\x03%"},
		{`"a\u00e9"`, "aé"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []string{
		``,
		`[1`,
		`(1`,
		`{:a}`,
		`{:a 1 :b}`,
		`"unterminated`,
		`"bad \q escape"`,
		`"a\/"`,
		`"a\8"`,
		`"a\03b"`,
		`"a\777"`,
		`"a\u"`,
		`"a\uZ"`,
		`"a\u00e"`,
		`#`,
		`# 1`,
		`#5 1`,
		`#x 1`,
		`#= 1`,
		`#(1 2)`,
		`#"re"`,
		`#@`,
		`#?`,
		`##`,
		`##5`,
		`##a/b`,
		`##"x"`,
		`#::`,
		`#:`,
		`#: {:a 1}`,
		`#:a`,
		`#:a ;c
{x 1}`,
		`#:mvn`,
		`#:mvn [1]`,
		`#:a 1 {x 1}`,
		`#:1.5{x 1}`,
		`#:"a"{x 1}`,
		`#:(a){x 1}`,
		`#::x {:a 1}`,
		`#::a/b{:x 1}`,
		`#:::foo{x 1}`,
		`#:: 1 {x 1}`,
		`{:a 1 2}`,
		`[1] trailing`,
		`'`,
		`:`,
		`,`,
		`;`,
		`@`,
		`^a`,
		`` + "`" + ``,
		`~`,
		`\a`,
		`a@b`,
		`1#x`,
		`1a`,
		`08`,
		`008`,
		`0x`,
		`1e`,
		`1_000`,
		`1.5_5`,
		`1N`,
		`0N`,
		`0x10N`,
		`017N`,
		`08N`,
		`1M`,
		`12M`,
		`1.5M`,
		`0x10M`,
		`2r`,
		`2r10`,
		`1/2`,
		`1/2/3`,
		`9223372036854775808`,
		`a:`,
		`a::b`,
		`//`,
		`//a`,
		`::`,
		`::/`,
		`::a/b`,
		`:1/2`,
		`:a/1`,
		`:a/b/`,
		`a/b/`,
		`a/10`,
		`a/0`,
		`{:a 1 :a 2}`,
		`#{1 1}`,
		`#:a{:a 1 :a 2}`,
		`{:a #_}`,
		`[:a #_]`,
		`#_`,
	}
	for _, in := range cases {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) expected error", in)
		}
	}
}

func TestParseExactErrors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`(1
2 3`, "ednlit: 2:4: EOF while reading\n2 3\n   ^"},
		{`{:a 1 ; x`, "ednlit: 1:10: EOF while reading\n{:a 1 ; x\n         ^"},
		{`"a\777"`, "ednlit: 1:3: octal escape sequence must be in range [0, 377]\n\"a\\777\"\n  ^"},
		{`"a\u00e"`, "ednlit: 1:3: invalid character length: 3, should be: 4\n\"a\\u00e\"\n  ^"},
		{`"a\8"`, "ednlit: 1:4: invalid digit: 8\n\"a\\8\"\n   ^"},
		{`"a\/"`, "ednlit: 1:4: unsupported escape character: \\/\n\"a\\/\"\n   ^"},
		{`{:a 1 :a 2}`, "ednlit: 1:7: duplicate key: :a\n{:a 1 :a 2}\n      ^"},
		{`#x 1`, "ednlit: 1:2: no reader function for tag x\n#x 1\n ^"},
		{`# 1`, "ednlit: 1:2: reader tag must be a symbol\n# 1\n ^"},
		{`#::x {:a 1}`, "ednlit: 1:2: unknown auto-resolved namespace alias: x\n#::x {:a 1}\n ^"},
		{`#:{:a 1}`, "ednlit: 1:2: namespaced map must specify a valid namespace: nil\n#:{:a 1}\n ^"},
		{`#: {:a 1}`, "ednlit: 1:2: namespaced map must specify a namespace\n#: {:a 1}\n ^"},
		{`{` + "`" + ` 1}`, "ednlit: 1:2: reader macro not supported: " + "`" + "\n{" + "`" + " 1}\n ^"},
		{`\a`, "ednlit: 1:1: character literal not supported\n\\a\n^"},
		{`a@b`, "ednlit: 1:2: trailing input\na@b\n ^"},
		{`#_`, "ednlit: 1:3: EOF while reading\n#_\n  ^"},
		{`1N`, "ednlit: 1:1: big integer literal not supported\n1N\n^"},
		{`1/2`, "ednlit: 1:1: ratio literal not supported\n1/2\n^"},
		{`2r10`, "ednlit: 1:1: number with explicit radix not supported\n2r10\n^"},
		{`9223372036854775808`, "ednlit: 1:1: integer out of 64-bit range\n9223372036854775808\n^"},
		{`a:`, "ednlit: 1:1: invalid token: a:\na:\n^"},
		{`::`, "ednlit: 1:1: invalid token: ::\n::\n^"},
		{`##5`, "ednlit: 1:2: invalid token: ##5\n##5\n ^"},
		{`##a/b`, "ednlit: 1:2: unknown symbolic value: ##a/b\n##a/b\n ^"},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) expected error", c.in)
			continue
		}
		if got := err.Error(); got != c.want {
			t.Errorf("Parse(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestParseNaN(t *testing.T) {
	got, err := Parse(`##NaN`)
	if err != nil {
		t.Fatalf("Parse(##NaN) error: %v", err)
	}
	f, ok := got.(float64)
	if !ok || !math.IsNaN(f) {
		t.Errorf("Parse(##NaN) = %#v, want NaN", got)
	}
}

func TestParseNamedError(t *testing.T) {
	_, err := Parse("{:a 1 ; x")
	if err == nil {
		t.Fatal("Parse expected error")
	}
	if got, want := err.Error(), "ednlit: 1:10: EOF while reading\n{:a 1 ; x\n         ^"; got != want {
		t.Errorf("unamed error = %q, want %q", got, want)
	}
	_, err = ParseNamed("modules/m/deps.edn", "{:a 1 ; x")
	if err == nil {
		t.Fatal("ParseNamed expected error")
	}
	if got, want := err.Error(), "ednlit: modules/m/deps.edn:1:10: EOF while reading\n{:a 1 ; x\n         ^"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []any{
		nil,
		true,
		false,
		int64(42),
		1.5,
		"plain",
		`has "quotes" and \back\slash and newline
`,
		Keyword{NS: "a.b", Name: "c"},
		Symbol{NS: "a.b", Name: "c"},
		[]any{int64(1), Keyword{Name: "x"}},
		List{Symbol{Name: "quote"}, Keyword{Name: "a"}},
		Map{{K: Keyword{NS: "k", Name: "v"}, V: []any{true}}},
		Set{int64(1), int64(2)},
		Quoted{V: []any{Keyword{Name: "unit"}}},
	}
	for _, in := range cases {
		s := Format(in)
		got, err := Parse(s)
		if err != nil {
			t.Errorf("round-trip parse of %q failed: %v", s, err)
			continue
		}
		if !reflect.DeepEqual(got, in) {
			t.Errorf("round-trip %q: got %#v, want %#v", s, got, in)
		}
	}
}
