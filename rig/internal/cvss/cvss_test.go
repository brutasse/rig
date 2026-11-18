package cvss

import (
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestBaseScoreReference scores every vector in testdata/vectors.tsv
// against the reference scores computed with the official FIRST
// formulas (the cvss 3.6 library, verified against the publishers'
// labels). The fixture is the fleet's own OSV vectors (102 distinct,
// from the 7,359-finding sweep) plus one vector per macro vector of
// the v4 score table (270) and the live OSV vectors found in the
// re-scan. The v4 rows must cover every macro in the table: the
// distance reduction only varies per macro, so a fixture missing a
// macro leaves its neighbour lookups untested.
func TestBaseScoreReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.tsv")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if lines[0] != "vector\tscore" {
		t.Fatalf("bad fixture header %q", lines[0])
	}
	var (
		n3, n4   int
		min, max float64
	)
	macros := make(map[string]bool)
	for i, line := range lines[1:] {
		vec, wantStr, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("line %d: bad fixture row %q", i+2, line)
		}
		want, err := strconv.ParseFloat(wantStr, 64)
		if err != nil {
			t.Fatalf("line %d: bad score %q", i+2, wantStr)
		}
		got, err := BaseScore(vec)
		if err != nil {
			t.Errorf("%s: error: %v", vec, err)
			continue
		}
		switch got.Version {
		case "3.0", "3.1":
			n3++
		case "4.0":
			n4++
			macros[macroOf4(t, vec)] = true
		default:
			t.Errorf("%s: version %q", vec, got.Version)
		}
		if math.Abs(got.Base-want) > 1e-9 {
			t.Errorf("%s: base = %v, want %v", vec, got.Base, want)
		}
		if i == 1 {
			min, max = got.Base, got.Base
		}
		if got.Base < min {
			min = got.Base
		}
		if got.Base > max {
			max = got.Base
		}
	}
	t.Logf("%d vectors: %d v3, %d v4, scores in [%.1f, %.1f]", len(lines)-1, n3, n4, min, max)
	if n3 == 0 || n4 == 0 {
		t.Errorf("fixture covers only one major version (%d v3, %d v4)", n3, n4)
	}
	var missing []string
	for m := range lookup4 {
		if !macros[m] {
			missing = append(missing, m)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("fixture misses %d of %d v4 macros: %s …", len(missing), len(lookup4), strings.Join(missing[:3], ", "))
	}
}

// macroOf4 is the v4 vector's severity class, for the fixture's
// macro-coverage check.
func macroOf4(t *testing.T, vector string) string {
	t.Helper()
	v := versions[2]
	metrics, err := parseFields(strings.TrimPrefix(vector, v.prefix), v)
	if err != nil {
		t.Fatalf("%s: %v", vector, err)
	}
	m := m4(metrics)
	// The environmental defaults, as baseScore4 applies them.
	for _, key := range []string{"E", "CR", "IR", "AR"} {
		if _, ok := metrics[key]; !ok {
			m[key] = "X"
		}
	}
	return eqString(m.macroVector())
}

// The vectors below carry known base scores from the FIRST
// specification's worked examples and the v3/v4 calculators.
func TestBaseScoreKnown(t *testing.T) {
	cases := []struct {
		vector string
		want   float64
	}{
		// CVSS v3.1 examples (spec §5.5)
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
		{"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:N/I:L/A:N", 3.7},
		// Scope changed, PR weighting and the 1.08 factor.
		{"CVSS:3.1/AV:L/AC:L/PR:N/UI:R/S:C/C:H/I:H/A:H", 8.6},
		{"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H", 9.9},
		// No impact at all: the Impact Sub-Score is not positive.
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N", 0},
		// CVSS v3.0 scores the same way.
		{"CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
		// CVSS v4.0: the spec's own example vector and its 0.0 case.
		{"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N", 9.3},
		{"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:N/VI:N/VA:N/SC:N/SI:N/SA:N", 0},
	}
	for _, c := range cases {
		got, err := BaseScore(c.vector)
		if err != nil {
			t.Errorf("%s: error: %v", c.vector, err)
			continue
		}
		if math.Abs(got.Base-c.want) > 1e-9 {
			t.Errorf("%s: base = %v, want %v", c.vector, got.Base, c.want)
		}
	}
}

func TestBaseScoreMalformed(t *testing.T) {
	for _, vec := range []string{
		"",
		"CVSS:3.1/",
		"CVSS:3.1/AV:N/",
		"CVSS:3.1/AV",
		"CVSS:3.1//AC:L",
		"CVSS:3.1/AV:X/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",               // unknown value
		"CVSS:3.1/QQ:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",               // unknown metric
		"CVSS:3.1/AV:N/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",          // duplicate
		"CVSS:3.1/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",                    // missing AV
		"CVSS:4.0/AV:N/AC:L/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N", // v4 missing AT
		"CVSS:2/AV:N", // unsupported version
		"AV:N/AC:L",   // no prefix
	} {
		if _, err := BaseScore(vec); err == nil {
			t.Errorf("%q: error expected, got nil", vec)
		}
	}
}

// TestBucket pins the bucket boundaries: Critical at 9.0, High at 7.0,
// Medium at 4.0, Low below.
func TestBucket(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{0, "Low"},
		{3.9, "Low"},
		{4.0, "Medium"},
		{6.9, "Medium"},
		{7.0, "High"},
		{8.9, "High"},
		{9.0, "Critical"},
		{10, "Critical"},
	}
	for _, c := range cases {
		if got := Bucket(c.score); got != c.want {
			t.Errorf("Bucket(%v) = %q, want %q", c.score, got, c.want)
		}
	}
}

// TestRounding exercises the two rounding rules against values that sit
// on or just off the 0.1 grid, where float drift bites.
func TestRounding(t *testing.T) {
	if got := roundup1(4.0); got != 4.0 {
		t.Errorf("roundup1(4.0) = %v", got)
	}
	if got := roundup1(4.02); got != 4.1 {
		t.Errorf("roundup1(4.02) = %v", got)
	}
	// 4.1000000000000005 is a float artifact of an exact 4.1: it must
	// not round to 4.2.
	if got := roundup1(4.1000000000000005); got != 4.1 {
		t.Errorf("roundup1(4.1000000000000005) = %v", got)
	}
	if got := finalRound(1.4499999999999993); got != 1.5 {
		t.Errorf("finalRound(1.4499999999999993) = %v, want 1.5 (the spec's example)", got)
	}
	if got := finalRound(5.25); got != 5.3 {
		t.Errorf("finalRound(5.25) = %v, want 5.3 (half up)", got)
	}
	if got := finalRound(5.249); got != 5.2 {
		t.Errorf("finalRound(5.249) = %v, want 5.2", got)
	}
}
