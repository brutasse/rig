// Package ghtoken resolves the GitHub API token rig authenticates its
// GitHub releases lookups with.
package ghtoken

import (
	"os"
	"strings"
)

// envVars are the token environment variables, in the gh CLI's precedence.
var envVars = []string{"GH_TOKEN", "GITHUB_TOKEN"}

// Hint is the clause an unauthenticated 403 error ends with.
const Hint = "set GH_TOKEN or GITHUB_TOKEN to raise the rate limit"

// Token returns the GitHub API token, or "" when none is configured. Without
// it, GitHub's REST API allows ~60 requests per hour per source IP — shared
// CI runners exhaust that pool; a token raises the limit to the
// per-repository one.
func Token() string {
	for _, k := range envVars {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
