package cvss

import (
	"fmt"
	"math"
	"strings"
)

// The CVSS v4.0 base score is a table-driven interpolation over the
// vector's severity "equations" (the spec's macro vector): the lookup
// table gives the score of each severity class, and the vector's
// distance to the neighbouring classes is weighted by how far each
// affected metric sits from its weakest value in the class. All the
// tables and steps are the FIRST CVSS v4.0 specification's ("Base
// Score" annex), via the official Red Hat calculator this file follows.
// https://www.first.org/cvss/v4.0/specification-document

var values4 = map[string]map[string]bool{
	"AV":  {"N": true, "A": true, "L": true, "P": true},
	"AC":  {"L": true, "H": true},
	"AT":  {"N": true, "P": true},
	"PR":  {"N": true, "L": true, "H": true},
	"UI":  {"N": true, "P": true, "A": true},
	"VC":  {"H": true, "L": true, "N": true},
	"VI":  {"H": true, "L": true, "N": true},
	"VA":  {"H": true, "L": true, "N": true},
	"SC":  {"H": true, "L": true, "N": true},
	"SI":  {"H": true, "L": true, "N": true},
	"SA":  {"H": true, "L": true, "N": true},
	"E":   {"X": true, "A": true, "P": true, "U": true},
	"CR":  {"X": true, "H": true, "M": true, "L": true},
	"IR":  {"X": true, "H": true, "M": true, "L": true},
	"AR":  {"X": true, "H": true, "M": true, "L": true},
	"MAV": {"X": true, "N": true, "A": true, "L": true, "P": true},
	"MAC": {"X": true, "L": true, "H": true},
	"MAT": {"X": true, "N": true, "P": true},
	"MPR": {"X": true, "N": true, "L": true, "H": true},
	"MUI": {"X": true, "N": true, "P": true, "A": true},
	"MVC": {"X": true, "H": true, "L": true, "N": true},
	"MVI": {"X": true, "H": true, "L": true, "N": true},
	"MVA": {"X": true, "H": true, "L": true, "N": true},
	"MSC": {"X": true, "H": true, "L": true, "N": true},
	"MSI": {"X": true, "S": true, "H": true, "L": true, "N": true},
	"MSA": {"X": true, "S": true, "H": true, "L": true, "N": true},
	"S":   {"X": true, "N": true, "P": true},
	"AU":  {"X": true, "N": true, "Y": true},
	"R":   {"X": true, "A": true, "U": true, "I": true},
	"V":   {"X": true, "D": true, "C": true},
	"RE":  {"X": true, "L": true, "M": true, "H": true},
	"U":   {"X": true, "Clear": true, "Green": true, "Amber": true, "Red": true},
}

var mandatory4 = []string{"AV", "AC", "AT", "PR", "UI", "VC", "VI", "VA", "SC", "SI", "SA"}

// m4 resolves one metric's effective value for scoring: undefined
// environmental metrics fall back to their defaults, and a modified
// metric (the M-prefixed one) overrides its base metric when defined.
type m4 map[string]string

func (m m4) get(metric string) string {
	v := m[metric]
	switch {
	case metric == "E" && v == "X":
		return "A"
	case metric == "CR" && v == "X":
		return "H"
	case metric == "IR" && v == "X":
		return "H"
	case metric == "AR" && v == "X":
		return "H"
	}
	if mv, ok := m["M"+metric]; ok && mv != "X" {
		return mv
	}
	return v
}

// macroVector computes the vector's severity class: one digit per
// equation (eq1 attack vector/privileges/user interaction, eq2
// complexity/attack requirement, eq3 vulnerable impact, eq4 subsequent
// impact, eq5 exploit maturity, eq6 requirement pairing).
func (m m4) macroVector() [6]int {
	av, pr, ui := m.get("AV"), m.get("PR"), m.get("UI")
	ac, at := m.get("AC"), m.get("AT")
	vc, vi, va := m.get("VC"), m.get("VI"), m.get("VA")
	sc, si, sa := m.get("SC"), m.get("SI"), m.get("SA")
	msi, msa := m.get("MSI"), m.get("MSA")
	cr, ir, ar := m.get("CR"), m.get("IR"), m.get("AR")

	eq1 := 2
	switch {
	case av == "N" && pr == "N" && ui == "N":
		eq1 = 0
	case av == "P" || (av != "N" && pr != "N" && ui != "N"):
		eq1 = 2
	default:
		eq1 = 1
	}
	eq2 := 1
	if ac == "L" && at == "N" {
		eq2 = 0
	}
	eq3 := 2
	switch {
	case vc == "H" && vi == "H":
		eq3 = 0
	case vc == "H" || vi == "H" || va == "H":
		eq3 = 1
	}
	eq4 := 2
	switch {
	case msi == "S" || msa == "S":
		eq4 = 0
	case sc == "H" || si == "H" || sa == "H":
		eq4 = 1
	}
	eq5 := 2
	switch m.get("E") {
	case "A":
		eq5 = 0
	case "P":
		eq5 = 1
	}
	eq6 := 1
	if (cr == "H" && vc == "H") || (ir == "H" && vi == "H") || (ar == "H" && va == "H") {
		eq6 = 0
	}
	return [6]int{eq1, eq2, eq3, eq4, eq5, eq6}
}

// baseScore4 implements the spec's base score computation: the macro
// vector's table entry, reduced by the weighted distance to the
// neighbouring severity classes.
func baseScore4(metrics map[string]string) (float64, error) {
	m := m4(metrics)
	for _, key := range []string{"E", "CR", "IR", "AR"} {
		if _, ok := metrics[key]; !ok {
			m[key] = "X"
		}
	}
	eq := m.macroVector()
	// No impact anywhere: the base score is 0.
	allN := true
	for _, key := range []string{"VC", "VI", "VA", "SC", "SI", "SA"} {
		if m.get(key) != "N" {
			allN = false
			break
		}
	}
	if allN {
		return 0, nil
	}

	// level distance: how far a metric value sits from the weakest
	// value of its equation (0 at the weakest).
	level := map[string]map[string]float64{
		"AV": {"N": 0.0, "A": 0.1, "L": 0.2, "P": 0.3},
		"AC": {"L": 0.0, "H": 0.1},
		"AT": {"N": 0.0, "P": 0.1},
		"PR": {"N": 0.0, "L": 0.1, "H": 0.2},
		"UI": {"N": 0.0, "P": 0.1, "A": 0.2},
		"VC": {"H": 0.0, "L": 0.1, "N": 0.2},
		"VI": {"H": 0.0, "L": 0.1, "N": 0.2},
		"VA": {"H": 0.0, "L": 0.1, "N": 0.2},
		"SC": {"H": 0.1, "L": 0.2, "N": 0.3},
		"SI": {"S": 0.0, "H": 0.1, "L": 0.2, "N": 0.3},
		"SA": {"S": 0.0, "H": 0.1, "L": 0.2, "N": 0.3},
		"CR": {"H": 0.0, "M": 0.1, "L": 0.2},
		"IR": {"H": 0.0, "M": 0.1, "L": 0.2},
		"AR": {"H": 0.0, "M": 0.1, "L": 0.2},
	}
	// distanceOf is the vector's level distance for one metric.
	distanceOf := func(metric string) float64 {
		return level[metric][m.get(metric)]
	}
	// distIn is the distance of a metric value embedded in a composed
	// (max) vector fragment.
	distIn := func(fragment, metric string) float64 {
		i := strings.Index(fragment, metric+":")
		end := i + len(metric) + 1
		if end >= len(fragment) {
			return 0
		}
		value := fragment[end:]
		if j := strings.IndexByte(value, '/'); j >= 0 {
			value = value[:j]
		}
		return level[metric][value]
	}

	macro := eqString(eq)
	value, ok := lookup4[macro]
	if !ok {
		return 0, fmt.Errorf("cvss: macro vector %s not in the v4 score table", macro)
	}

	// The weaker neighbour of each equation: the macro vector with
	// that equation one step further from the most severe.
	nextLower := func(i ...int) string {
		x := eq
		for _, j := range i {
			x[j]++
		}
		return eqString(x)
	}
	scoreOf := func(macro string) float64 {
		s, ok := lookup4[macro]
		if !ok {
			return math.NaN()
		}
		return s
	}
	scoreEq1 := scoreOf(nextLower(0))
	scoreEq2 := scoreOf(nextLower(1))
	scoreEq4 := scoreOf(nextLower(3))
	scoreEq5 := scoreOf(nextLower(4))
	// eq3 and eq6 weaken together; the neighbouring macro depends on
	// which corner the current one sits in.
	var scoreEq3eq6 float64
	switch {
	case eq[2] == 1 && eq[5] == 1, eq[2] == 0 && eq[5] == 1:
		scoreEq3eq6 = scoreOf(nextLower(2))
	case eq[2] == 1 && eq[5] == 0, eq[2] == 0 && eq[5] == 0:
		if eq[2] == 0 && eq[5] == 0 {
			scoreEq3eq6 = math.Max(scoreOf(nextLower(5)), scoreOf(nextLower(2)))
		} else {
			scoreEq3eq6 = scoreOf(nextLower(5))
		}
	default:
		scoreEq3eq6 = scoreOf(nextLower(2, 5))
	}

	// The composed vectors that hold the weakest value of each
	// equation group, for the severity distance against the vector.
	// eq3 pairs with eq6 (its second index).
	maxComposed := map[string]map[string][]string{
		"eq1": {
			"0": {"AV:N/PR:N/UI:N/"},
			"1": {"AV:A/PR:N/UI:N/", "AV:N/PR:L/UI:N/", "AV:N/PR:N/UI:P/"},
			"2": {"AV:P/PR:N/UI:N/", "AV:A/PR:L/UI:P/"},
		},
		"eq2": {
			"0": {"AC:L/AT:N/"},
			"1": {"AC:H/AT:N/", "AC:L/AT:P/"},
		},
		"eq4": {
			"0": {"SC:H/SI:S/SA:S/"},
			"1": {"SC:H/SI:H/SA:H/"},
			"2": {"SC:L/SI:L/SA:L/"},
		},
		"eq5": {
			"0": {"E:A/"},
			"1": {"E:P/"},
			"2": {"E:U/"},
		},
	}
	maxComposed3 := map[string]map[string][]string{
		"0": {
			"0": {"VC:H/VI:H/VA:H/CR:H/IR:H/AR:H/"},
			"1": {"VC:H/VI:H/VA:L/CR:M/IR:M/AR:H/", "VC:H/VI:H/VA:H/CR:M/IR:M/AR:M/"},
		},
		"1": {
			"0": {"VC:L/VI:H/VA:H/CR:H/IR:H/AR:H/", "VC:H/VI:L/VA:H/CR:H/IR:H/AR:H/"},
			"1": {"VC:L/VI:H/VA:L/CR:H/IR:M/AR:H/", "VC:L/VI:H/VA:H/CR:H/IR:M/AR:M/",
				"VC:H/VI:L/VA:H/CR:M/IR:H/AR:M/", "VC:H/VI:L/VA:L/CR:M/IR:H/AR:H/",
				"VC:L/VI:L/VA:H/CR:H/IR:H/AR:M/"},
		},
		"2": {
			"1": {"VC:L/VI:L/VA:L/CR:H/IR:H/AR:H/"},
		},
	}
	maxSeverity := map[string]map[string]float64{
		"eq1": {"0": 1, "1": 4, "2": 5},
		"eq2": {"0": 1, "1": 2},
		"eq4": {"0": 6, "1": 5, "2": 4},
		"eq5": {"0": 1, "1": 1, "2": 1},
	}
	// eq3eq6Maxes is the eq3/eq6 group's max severity, by corner.
	var eq3eq6Maxes map[string]float64
	switch {
	case eq[2] == 0 && eq[5] == 0:
		eq3eq6Maxes = map[string]float64{"0": 7, "1": 6}
	case eq[2] == 1:
		eq3eq6Maxes = map[string]float64{"0": 8, "1": 8}
	default:
		eq3eq6Maxes = map[string]float64{"1": 10}
	}
	distances := []float64{
		distanceOf("AV"), distanceOf("PR"), distanceOf("UI"),
		distanceOf("AC"), distanceOf("AT"),
		distanceOf("VC"), distanceOf("VI"), distanceOf("VA"),
		distanceOf("CR"), distanceOf("IR"), distanceOf("AR"),
		distanceOf("SC"), distanceOf("SI"), distanceOf("SA"),
	}
	distEq := func(d []float64, i []int) float64 {
		total := 0.0
		for _, j := range i {
			total += d[j]
		}
		return total
	}

	// Walk the composed weakest vectors until one no metric beats:
	// that is the vector's severity reference, and its per-metric
	// differences from the vector are the current severity distances.
	// The metric order must match distances'.
	metrics14 := []string{"AV", "PR", "UI", "AC", "AT", "VC", "VI", "VA", "CR", "IR", "AR", "SC", "SI", "SA"}
	maxes := []string{}
	for _, eq1Max := range maxComposed["eq1"][eqStr(eq[0])] {
		for _, eq2Max := range maxComposed["eq2"][eqStr(eq[1])] {
			for _, eq3eq6Max := range maxComposed3[eqStr(eq[2])][eqStr(eq[5])] {
				for _, eq4Max := range maxComposed["eq4"][eqStr(eq[3])] {
					for _, eq5Max := range maxComposed["eq5"][eqStr(eq[4])] {
						maxes = append(maxes, eq1Max+eq2Max+eq3eq6Max+eq4Max+eq5Max)
					}
				}
			}
		}
	}
	// refDistances is the severity distance to the reference vector.
	refDistances := distances
	for _, fragment := range maxes {
		var bad bool
		for i, metric := range metrics14 {
			if distances[i]-distIn(fragment, metric) < 0 {
				bad = true
				break
			}
		}
		if !bad {
			for i, metric := range metrics14 {
				refDistances[i] = distances[i] - distIn(fragment, metric)
			}
			break
		}
	}

	// Each equation's share of the gap to its weaker neighbour,
	// weighted by how far the vector sits from the equation's weakest
	// values; the mean of the existing gaps is the score reduction.
	step := 0.1
	available := []float64{
		value - scoreEq1,
		value - scoreEq2,
		value - scoreEq3eq6,
		value - scoreEq4,
		value - scoreEq5,
	}
	distEqGroups := []float64{
		distEq(refDistances, []int{0, 1, 2}),
		distEq(refDistances, []int{3, 4}),
		distEq(refDistances, []int{5, 6, 7, 8, 9, 10}),
		distEq(refDistances, []int{11, 12, 13}),
		0,
	}
	maxes4 := []float64{
		maxSeverity["eq1"][eqStr(eq[0])] * step,
		maxSeverity["eq2"][eqStr(eq[1])] * step,
		eq3eq6Maxes[eqStr(eq[5])] * step,
		maxSeverity["eq4"][eqStr(eq[3])] * step,
		maxSeverity["eq5"][eqStr(eq[4])] * step,
	}
	var sum, count float64
	for i, a := range available {
		if math.IsNaN(a) || a < 0 {
			continue
		}
		count += 1
		percent := distEqGroups[i] / maxes4[i]
		sum += a * percent
	}
	mean := 0.0
	if count > 0 {
		mean = sum / count
	}
	value -= mean
	if value < 0 {
		value = 0
	}
	if value > 10 {
		value = 10
	}
	return finalRound(value), nil
}

func eqStr(i int) string { return fmt.Sprintf("%d", i) }

func eqString(eq [6]int) string {
	return fmt.Sprintf("%d%d%d%d%d%d", eq[0], eq[1], eq[2], eq[3], eq[4], eq[5])
}

var versions4 = []*version{
	{prefix: "CVSS:4.0/", tag: "4.0", values: values4, mandatory: mandatory4, score: baseScore4},
}

var versions = append(versions3, versions4...)
