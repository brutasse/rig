// Package cvss computes CVSS base scores from vector strings, per the
// official FIRST CVSS v3 and v4 specifications (https://www.first.org/cvss/).
// OSV.dev records name their severity as a bare score or a vector string;
// the vector is what this package scores.
package cvss

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Score is the result of scoring one CVSS vector string.
type Score struct {
	// Version is the vector's spec version: "3.0", "3.1" or "4.0".
	Version string
	// Base is the computed base score, rounded per the spec (one
	// decimal place).
	Base float64
}

// Bucket maps a score to the common severity bucket: Critical at 9.0
// and up, High at 7.0, Medium at 4.0, Low below.
func Bucket(score float64) string {
	switch {
	case score >= 9.0:
		return "Critical"
	case score >= 7.0:
		return "High"
	case score >= 4.0:
		return "Medium"
	default:
		return "Low"
	}
}

// version is one supported CVSS spec version: its vector prefix, the
// metrics and values a vector may carry, the mandatory base metrics,
// and the base score function.
type version struct {
	prefix    string
	tag       string
	values    map[string]map[string]bool
	mandatory []string
	score     func(metrics map[string]string) (float64, error)
}

// BaseScore computes the base score of a CVSS 3.0, 3.1 or 4.0 vector
// string. Temporal and environmental metrics may be present; only the
// base metrics decide the score. The vector's fields may appear in any
// order; duplicates are refused.
func BaseScore(vector string) (Score, error) {
	for _, v := range versions {
		if !strings.HasPrefix(vector, v.prefix) {
			continue
		}
		metrics, err := parseFields(strings.TrimPrefix(vector, v.prefix), v)
		if err != nil {
			return Score{}, err
		}
		base, err := v.score(metrics)
		if err != nil {
			return Score{}, err
		}
		return Score{Version: v.tag, Base: base}, nil
	}
	return Score{}, fmt.Errorf("cvss: unsupported version in vector %q", vector)
}

// parseFields splits the vector's metric part ("AV:N/AC:L/…") into a
// metric -> value map, refusing unknown metrics, unknown values, empty
// fields and duplicates, and requiring every mandatory base metric.
func parseFields(part string, v *version) (map[string]string, error) {
	metrics := make(map[string]string)
	for _, field := range strings.Split(part, "/") {
		if field == "" {
			return nil, errors.New("cvss: empty field in vector")
		}
		name, value, ok := strings.Cut(field, ":")
		if !ok {
			return nil, fmt.Errorf("cvss: malformed field %q", field)
		}
		values, known := v.values[name]
		if !known {
			return nil, fmt.Errorf("cvss: unknown metric %q", name)
		}
		if !values[value] {
			return nil, fmt.Errorf("cvss: invalid value %q for metric %q", value, name)
		}
		if _, dup := metrics[name]; dup {
			return nil, fmt.Errorf("cvss: duplicate metric %q", name)
		}
		metrics[name] = value
	}
	var missing []string
	for _, m := range v.mandatory {
		if _, ok := metrics[m]; !ok {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cvss: missing mandatory metrics %s", strings.Join(missing, ", "))
	}
	return metrics, nil
}

// roundup1 is the spec's Roundup for v3: the smallest number with one
// decimal place that is equal to or higher than the input. The 1e-9
// guard keeps a value that is exactly on the 0.1 grid (but represented
// a hair above it by float arithmetic) from rounding to the next step.
func roundup1(x float64) float64 {
	return math.Ceil(x*10-1e-9) / 10
}

// finalRound is the spec's final rounding for v4: half-up to one
// decimal place. The 1e-6 guard makes values like 1.4499999999999993
// (a float artifact of an exact 1.45) round to 1.5, per the spec's
// worked examples.
func finalRound(x float64) float64 {
	return math.Round((x+1e-6)*10) / 10
}
