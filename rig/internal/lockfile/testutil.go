package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func ForTest(modules ...string) *Document {
	if len(modules) == 0 {
		modules = []string{"."}
	}
	const artifactID = "org.clojure/clojure:1.11.0:jar"
	doc := &Document{
		Version:  SupportedVersion,
		Tool:     Tool{Name: "rig", Version: "0.0.0-test"},
		Resolver: Resolver{Lib: "test/resolver", Version: "0", GitSHA: testSHA},
		LockedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Workspace: Workspace{
			Modules:        modules,
			ManifestSHA256: testSHA,
		},
		Cooldown: Cooldown{Default: "48h"},
		Artifacts: []Artifact{{
			ID:         artifactID,
			Kind:       "mvn",
			Group:      "org.clojure",
			Name:       "clojure",
			Version:    "1.11.0",
			Extension:  "jar",
			Repository: "central",
			URL:        "https://example.invalid/clojure-1.11.0.jar",
			SHA256:     testSHA,
		}},
		Modules: map[string]Module{},
	}
	for _, m := range modules {
		ref := artifactID
		doc.Modules[m] = Module{
			ManifestSHA256: testSHA,
			Lib:            "test/" + strings.NewReplacer("/", "-", ".", "-").Replace(m),
			Version:        "0.0.1",
			Classpath:      []ClasspathEntry{{Ref: &ref}},
			Build:          Build{ClassDir: "target/classes"},
			Test:           Test{Enabled: true},
		}
	}
	return doc
}

// FreshFor makes d non-stale for the given manifest contents: contents["."]
// is the workspace root deps.edn, other keys are module dirs. Modules absent
// from contents keep their hash.
func (d *Document) FreshFor(contents map[string]string) {
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	if c, ok := contents["."]; ok {
		d.Workspace.ManifestSHA256 = sum(c)
	}
	for m, mod := range d.Modules {
		if m == "." {
			continue
		}
		if c, ok := contents[m]; ok {
			mod.ManifestSHA256 = sum(c)
			d.Modules[m] = mod
		}
	}
}
