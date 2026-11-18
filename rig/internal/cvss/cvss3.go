package cvss

import "fmt"

// CVSS v3.0 and v3.1 share the base score formula (the versions differ
// only in environmental metrics, which the base score does not use),
// so both prefixes dispatch to one scorer. The metric weights, the
// Impact Sub-Score and the base score follow the FIRST CVSS v3.1
// specification (https://www.first.org/cvss/v3.1/specification-document):
//
//	ISCBase = 1 - [(1-ImpactConf) x (1-ImpactInteg) x (1-ImpactAvail)]
//	ISS (Scope Unchanged) = 6.42 x ISCBase
//	ISS (Scope Changed)   = 7.52 x [ISCBase - 0.029] - 3.25 x [ISCBase - 0.02]^15
//	ESC                 = 8.22 x AV x AC x PR x UI
//	Base (Unchanged)      = Roundup(Minimum(ISS + ESC, 10))
//	Base (Changed)        = Roundup(Minimum(1.08 x (ISS + ESC), 10))
//
// with the base score 0 when the Impact Sub-Score is not positive.

var values3 = map[string]map[string]bool{
	"AV":  {"N": true, "A": true, "L": true, "P": true},
	"AC":  {"L": true, "H": true},
	"PR":  {"N": true, "L": true, "H": true},
	"UI":  {"N": true, "R": true},
	"S":   {"C": true, "U": true},
	"C":   {"H": true, "L": true, "N": true},
	"I":   {"H": true, "L": true, "N": true},
	"A":   {"H": true, "L": true, "N": true},
	"E":   {"X": true, "H": true, "F": true, "P": true, "U": true},
	"RL":  {"X": true, "U": true, "W": true, "T": true, "O": true},
	"RC":  {"X": true, "C": true, "R": true, "U": true},
	"CR":  {"X": true, "H": true, "M": true, "L": true},
	"IR":  {"X": true, "H": true, "M": true, "L": true},
	"AR":  {"X": true, "H": true, "M": true, "L": true},
	"MAV": {"X": true, "N": true, "A": true, "L": true, "P": true},
	"MAC": {"X": true, "L": true, "H": true},
	"MPR": {"X": true, "N": true, "L": true, "H": true},
	"MUI": {"X": true, "N": true, "R": true},
	"MS":  {"X": true, "C": true, "U": true},
	"MC":  {"X": true, "H": true, "L": true, "N": true},
	"MI":  {"X": true, "H": true, "L": true, "N": true},
	"MA":  {"X": true, "H": true, "L": true, "N": true},
}

var mandatory3 = []string{"AV", "AC", "PR", "UI", "S", "C", "I", "A"}

func baseScore3(metrics map[string]string) (float64, error) {
	scopeChanged := metrics["S"] == "C"
	// weights3 holds the v3 base metric weight table (spec §5.2);
	// Privileges Required weighs differently when the scope is changed.
	weight := func(name, value string) (float64, bool) {
		if name == "PR" && scopeChanged {
			switch value {
			case "N":
				return 0.85, true
			case "L":
				return 0.68, true
			case "H":
				return 0.50, true
			}
		}
		w, ok := weights3[name][value]
		return w, ok
	}
	var w [7]float64
	names := []string{"AV", "AC", "PR", "UI", "C", "I", "A"}
	for i, name := range names {
		x, ok := weight(name, metrics[name])
		if !ok {
			return 0, fmt.Errorf("cvss: invalid value %q for metric %q", metrics[name], name)
		}
		w[i] = x
	}
	iscBase := 1 - (1-w[4])*(1-w[5])*(1-w[6])
	esc := 8.22 * w[0] * w[1] * w[2] * w[3]
	var iss float64
	if scopeChanged {
		iss = 7.52*(iscBase-0.029) - 3.25*pow(iscBase-0.02, 15)
	} else {
		iss = 6.42 * iscBase
	}
	if iss <= 0 {
		return 0, nil
	}
	total := iss + esc
	if scopeChanged {
		total *= 1.08
	}
	if total > 10 {
		total = 10
	}
	return roundup1(total), nil
}

var weights3 = map[string]map[string]float64{
	"AV": {"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.20},
	"AC": {"L": 0.77, "H": 0.44},
	"PR": {"N": 0.85, "L": 0.62, "H": 0.27},
	"UI": {"N": 0.85, "R": 0.62},
	"C":  {"N": 0, "L": 0.22, "H": 0.56},
	"I":  {"N": 0, "L": 0.22, "H": 0.56},
	"A":  {"N": 0, "L": 0.22, "H": 0.56},
}

func pow(x float64, n int) float64 {
	r := 1.0
	for i := 0; i < n; i++ {
		r *= x
	}
	return r
}

var versions3 = []*version{
	{prefix: "CVSS:3.0/", tag: "3.0", values: values3, mandatory: mandatory3, score: baseScore3},
	{prefix: "CVSS:3.1/", tag: "3.1", values: values3, mandatory: mandatory3, score: baseScore3},
}
