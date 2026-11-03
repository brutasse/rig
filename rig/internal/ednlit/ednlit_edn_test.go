package ednlit

// Ported subset of the Clojure reader test suite,
// clojure/test/clojure/test_clojure/reader.cljc (reference: Clojure 1.12.6,
// the version pinned by resolver/deps.edn): Symbols, Literals, Strings,
// Numbers, reading-keywords, namespaced-maps, namespaced-map-errors,
// namespaced-map-edn and invalid-symbol-value.
//
// Not ported:
//   - file-based string reading (ednlit only reads strings)
//   - BigInt (N), BigDecimal (M), Ratio and explicit-radix literals: valid
//     Clojure reader syntax that ednlit rejects with an explicit error
//     (covered by TestParseErrors)
//   - #::s {…} alias resolution: ednlit has no namespace aliases, so a
//     non-empty qualifier of an auto-resolving namespaced map is always
//     rejected ("unknown auto-resolved namespace alias: s")

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEDNSymbols(t *testing.T) {
	cases := []struct {
		in   string
		want Symbol
	}{
		{`abc`, Symbol{Name: "abc"}},
		{`*+!-_?`, Symbol{Name: "*+!-_?"}},
		{`abc:def:ghi`, Symbol{Name: "abc:def:ghi"}},
		{`abc/def`, Symbol{NS: "abc", Name: "def"}},
		{`abc.def/ghi`, Symbol{NS: "abc.def", Name: "ghi"}},
		{`abc/def.ghi`, Symbol{NS: "abc", Name: "def.ghi"}},
		{`abc:def/ghi:jkl.mno`, Symbol{NS: "abc:def", Name: "ghi:jkl.mno"}},
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

func TestEDNLiterals(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`nil`, nil},
		{`false`, false},
		{`true`, true},
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

func TestEDNStrings(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{`""`, ""},
		{`"\"` + `"`, `"`},
		{`"\n"`, "\n"},
		{`"\0"`, "\x00"},
		{`"\000"`, "\x00"},
		{`"\3"`, "\x03"},
		{`"\03"`, "\x03"},
		{`"\003"`, "\x03"},
		{`"\0003"`, "\x003"},
		{`"\0030"`, "\x030"},
		{`"\377"`, "\xff"},
		{`"\0008"`, "\x008"},
		{`"\u0000"`, "\x00"},
		{`"\ud7ff"`, "\ud7ff"},
		{`"\ud800"`, string(rune(0xd800))}, // lone surrogate, as in the JVM
		{`"\udfff"`, string(rune(0xdfff))},
		{`"\ue000"`, "\ue000"},
		{`"\uffff"`, "\uffff"},
		{`"\u00041"`, "\x041"},
	}
	for _, c := range valid {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", c.in, err)
			continue
		}
		s, ok := got.(string)
		if !ok {
			t.Errorf("Parse(%q) = %#v, want string", c.in, got)
			continue
		}
		if s != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.in, s, c.want)
		}
	}

	errs := []struct {
		in   string
		want string
	}{
		{`"\`, "EOF while reading string"},
		{`"\o"`, "unsupported escape character: \\o"},
		{`"\400"`, "octal escape sequence must be in range [0, 377]"},
		{`"\8"`, "invalid digit: 8"},
		{`"\8000"`, "invalid digit: 8"},
		{`"\0800"`, "invalid digit: 8"},
		{`"\0080"`, "invalid digit: 8"},
		{`"\2and"`, "invalid digit: a"},
		{`"\u"`, "invalid unicode escape: \\u\""},
		{`"\ug"`, "invalid unicode escape: \\ug"},
		{`"\ug000"`, "invalid unicode escape: \\ug"},
		{`"\u0"`, "invalid character length: 1, should be: 4"},
		{`"\u004"`, "invalid character length: 3, should be: 4"},
		{`"\u004g"`, "invalid digit: g"},
	}
	for _, c := range errs {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) expected error", c.in)
			continue
		}
		if got := err.Error(); !strings.Contains(got, c.want) {
			t.Errorf("Parse(%q) error %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

func TestEDNNumbers(t *testing.T) {
	// The int64 range of the JVM's Long; beyond it the JVM reads BigInt,
	// which ednlit rejects.
	ints := []struct {
		in   string
		want int64
	}{
		{`2147483647`, 2147483647},
		{`+1`, 1},
		{`1`, 1},
		{`+0`, 0},
		{`0`, 0},
		{`-0`, 0},
		{`-1`, -1},
		{`-2147483648`, -2147483648},
		{`2147483648`, 2147483648},
		{`-2147483649`, -2147483649},
		{`9223372036854775807`, 9223372036854775807},
		{`-9223372036854775808`, -9223372036854775808},
	}
	for _, c := range ints {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}

	doubles := []string{
		`+1.0e+1`, `+1.e+1`, `+1e+1`,
		`+1.0e1`, `+1.e1`, `+1e1`,
		`+1.0e-1`, `+1.e-1`, `+1e-1`,
		`1.0e+1`, `1.e+1`, `1e+1`,
		`1.0e1`, `1.e1`, `1e1`,
		`1.0e-1`, `1.e-1`, `1e-1`,
		`-1.0e+1`, `-1.e+1`, `-1e+1`,
		`-1.0e1`, `-1.e1`, `-1e1`,
		`-1.0e-1`, `-1.e-1`, `-1e-1`,
		`+1.0`, `+1.`,
		`1.0`, `1.`,
		`+0.0`, `+0.`,
		`0.0`, `0.`,
		`-0.0`, `-0.`,
		`-1.0`, `-1.`,
	}
	for _, in := range doubles {
		if _, err := Parse(in); err != nil {
			t.Errorf("Parse(%q) error: %v", in, err)
		}
	}
}

func TestEDNReadingKeywords(t *testing.T) {
	cases := []struct {
		in   string
		want Keyword
	}{
		{`:foo`, Keyword{Name: "foo"}},
		{`:foo/bar`, Keyword{NS: "foo", Name: "bar"}},
		{`::foo`, Keyword{NS: "user", Name: "foo"}},
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
	for _, in := range []string{`foo:`, `:bar/`, `::does.not/exist`} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) expected error", in)
		}
	}
}

func TestEDNNamespacedMaps(t *testing.T) {
	cases := []struct {
		in   string
		want Map
	}{
		{`#:a{1 nil :b nil :b/c nil :_/d nil}`,
			Map{{K: int64(1), V: nil}, {K: Keyword{NS: "a", Name: "b"}, V: nil},
				{K: Keyword{NS: "b", Name: "c"}, V: nil}, {K: Keyword{Name: "d"}, V: nil}}},
		{`#::{1 nil :a nil :a/b nil :_/d nil}`,
			Map{{K: int64(1), V: nil}, {K: Keyword{NS: "user", Name: "a"}, V: nil},
				{K: Keyword{NS: "a", Name: "b"}, V: nil}, {K: Keyword{Name: "d"}, V: nil}}},
		{`#:: {1 nil :a nil :a/b nil :_/d nil}`,
			Map{{K: int64(1), V: nil}, {K: Keyword{NS: "user", Name: "a"}, V: nil},
				{K: Keyword{NS: "a", Name: "b"}, V: nil}, {K: Keyword{Name: "d"}, V: nil}}},
		{`#:a{b 1 b/c 2}`,
			Map{{K: Symbol{NS: "a", Name: "b"}, V: int64(1)}, {K: Symbol{NS: "b", Name: "c"}, V: int64(2)}}},
		{`#::{b 1 b/c 2 _/d 3}`,
			Map{{K: Symbol{NS: "user", Name: "b"}, V: int64(1)}, {K: Symbol{NS: "b", Name: "c"}, V: int64(2)},
				{K: Symbol{Name: "d"}, V: int64(3)}}},
		{`#:a{1 1, :b 2, :b/c 3, :_/d 4}`,
			Map{{K: int64(1), V: int64(1)}, {K: Keyword{NS: "a", Name: "b"}, V: int64(2)},
				{K: Keyword{NS: "b", Name: "c"}, V: int64(3)}, {K: Keyword{Name: "d"}, V: int64(4)}}},
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

func TestEDNNamespacedMapErrors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`#:::`, "invalid token"},
		{`#:s{1}`, "namespaced map literal must contain an even number of forms"},
		{`#:s/t{1 2}`, "namespaced map must specify a valid namespace"},
		{`#::BOGUS{1 2}`, "unknown auto-resolved namespace alias"},
		{`#: s{:a 1}`, "namespaced map must specify a namespace"},
		{`#::{:a 1 :a 2}`, "duplicate key: :user/a"},
		{`#::{a 1 a 2}`, "duplicate key: user/a"},
		// ednlit has no aliases to resolve.
		{`#::s{1 2}`, "unknown auto-resolved namespace alias"},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) expected error", c.in)
			continue
		}
		if got := err.Error(); !strings.Contains(got, c.want) {
			t.Errorf("Parse(%q) error %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

func TestEDNInvalidSymbolValue(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{`##5`, "invalid token"},
		{`##Foo`, "unknown symbolic value"},
	} {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) expected error", c.in)
			continue
		}
		if got := err.Error(); !strings.Contains(got, c.want) {
			t.Errorf("Parse(%q) error %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

// TestParseRealDepsEdn parses the manifest that pins the reference Clojure
// version and checks the structure rig reads from it.
func TestParseRealDepsEdn(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "resolver", "deps.edn"))
	if err != nil {
		t.Skipf("resolver/deps.edn not found: %v", err)
	}
	v, err := ParseNamed("resolver/deps.edn", string(b))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	m, ok := v.(Map)
	if !ok {
		t.Fatalf("top level = %T, want Map", v)
	}

	if got := lookup(t, m, Keyword{Name: "paths"}); !reflect.DeepEqual(got, []any{"src"}) {
		t.Errorf(":paths = %#v, want [src]", got)
	}

	deps, ok := lookup(t, m, Keyword{Name: "deps"}).(Map)
	if !ok {
		t.Fatal(":deps = " + Format(lookup(t, m, Keyword{Name: "deps"})) + ", want Map")
	}
	clojure, ok := lookup(t, deps, Symbol{NS: "org.clojure", Name: "clojure"}).(Map)
	if !ok {
		t.Fatal(":deps org.clojure/clojure missing or not a map")
	}
	if got := lookup(t, clojure, Keyword{NS: "mvn", Name: "version"}); got != "1.12.6" {
		t.Errorf("clojure :mvn/version = %#v, want 1.12.6", got)
	}
	cheshire, ok := lookup(t, deps, Symbol{NS: "cheshire", Name: "cheshire"}).(Map)
	if !ok {
		t.Fatal(":deps cheshire/cheshire missing or not a map")
	}
	if got := lookup(t, cheshire, Keyword{Name: "exclusions"}); !reflect.DeepEqual(got, []any{Symbol{NS: "tigris", Name: "tigris"}}) {
		t.Errorf("cheshire :exclusions = %#v, want [tigris/tigris]", got)
	}

	if got := lookup(t, m, Keyword{NS: "rig", Name: "modules"}); !reflect.DeepEqual(got, []any{"runner"}) {
		t.Errorf(":rig/modules = %#v, want [runner]", got)
	}
	if got := lookup(t, m, Keyword{NS: "rig", Name: "jvm"}); got != "8" {
		t.Errorf(":rig/jvm = %#v, want \"8\"", got)
	}
	if got := lookup(t, m, Keyword{NS: "rig", Name: "timestamp-string"}); got != "2020-01-01T00:00:02Z" {
		t.Errorf(":rig/timestamp-string = %#v", got)
	}

	aliases, ok := lookup(t, m, Keyword{Name: "aliases"}).(Map)
	if !ok {
		t.Fatal(":aliases missing or not a map")
	}
	test, ok := lookup(t, aliases, Keyword{Name: "test"}).(Map)
	if !ok {
		t.Fatal(":aliases :test missing or not a map")
	}
	if got := lookup(t, test, Keyword{Name: "exec-fn"}); !reflect.DeepEqual(got, Symbol{NS: "kaocha.runner", Name: "exec-fn"}) {
		t.Errorf(":test :exec-fn = %#v, want kaocha.runner/exec-fn", got)
	}
}

func lookup(t *testing.T, m Map, k any) any {
	t.Helper()
	for _, p := range m {
		if reflect.DeepEqual(p.K, k) {
			return p.V
		}
	}
	t.Fatalf("key %v not found in map", k)
	return nil
}
