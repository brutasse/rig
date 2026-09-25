package ednlit

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`nil`, nil},
		{`null`, nil},
		{`true`, true},
		{`false`, false},
		{`42`, int64(42)},
		{`-7`, int64(-7)},
		{`+3`, int64(3)},
		{`1_000`, int64(1000)},
		{`1.5`, 1.5},
		{`-0.25`, -0.25},
		{`1e3`, 1000.0},
		{`:a`, Keyword{Name: "a"}},
		{`:a/b`, Keyword{NS: "a", Name: "b"}},
		{`:'quoted-kw`, Keyword{Name: "quoted-kw"}},
		{`:kaocha.filter/focus`, Keyword{NS: "kaocha.filter", Name: "focus"}},
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
		`"unterminated`,
		`"bad \q escape"`,
		`#x 1`,
		`{:a 1 2}`,
		`[1] trailing`,
		`'`,
		`:`,
		`,`,
		`;`,
	}
	for _, in := range cases {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) expected error", in)
		}
	}
}

func TestParseNamedError(t *testing.T) {
	_, err := Parse("{:a 1 ; x")
	if err == nil {
		t.Fatal("Parse expected error")
	}
	if got, want := err.Error(), "ednlit: unclosed map at 9"; got != want {
		t.Errorf("unamed error = %q, want %q", got, want)
	}
	_, err = ParseNamed("modules/m/deps.edn", "{:a 1 ; x")
	if err == nil {
		t.Fatal("ParseNamed expected error")
	}
	if got, want := err.Error(), "ednlit: unclosed map at 9 in modules/m/deps.edn"; got != want {
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
