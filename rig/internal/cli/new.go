package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/ednlit"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/workspace"
)

var (
	coordRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*/[a-z0-9][a-z0-9.-]*$`)
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
)

// scaffoldDep is a coordinate a scaffold pins, with the version written when
// the lookup has no answer: --offline writes it without asking, and so does
// anything else that stops the lookup (every release inside its cooldown, or
// a repository that cannot be reached or serves nothing).
type scaffoldDep struct {
	coord    string
	fallback string
}

// The test runner a scaffold starts with. Its version is resolved at scaffold
// time by the latest op, so a rig release does not have to track kaocha's:
// defaultTestRunnerVersion is the fallback, written under --offline and
// whenever the lookup has no answer (every release inside its cooldown, or a
// repository that cannot be reached).
const (
	testRunnerCoord          = "lambdaisland/kaocha"
	defaultTestRunnerVersion = "1.91.1392"
	// The Clojure runtime both the scaffolded module and the dev module pin:
	// a local-root module contributes its paths, not its deps, to the dev
	// classpath, so without the pin the dev REPL would run on the Clojure a
	// transitive dep happens to serve (and below the nREPL floor).
	appClojureVersion = "1.12.5"
)

// The :test alias extra-deps of a scaffolded module: the test runner, the
// property-based and mocking tools, and the kaocha reporters (JUnit XML and
// coverage), which sit dormant until `rig test :plugins ...` loads them.
var scaffoldTestDeps = []scaffoldDep{
	{testRunnerCoord, defaultTestRunnerVersion},
	{"org.clojure/test.check", "1.1.3"},
	{"tortue/spy", "2.15.0"},
	{"lambdaisland/kaocha-cloverage", "1.1.89"},
	{"lambdaisland/kaocha-junit-xml", "1.17.101"},
}

// The dev module's dependency beyond the app module it loads: the
// reloaded-workflow library
// (https://cognitect.com/blog/2013/06/04/clojure-workflow-reloaded).
var scaffoldDevDeps = []scaffoldDep{
	{"reloaded.repl/reloaded.repl", "0.2.4"},
}

// scaffoldDeps is every coordinate a scaffold resolves at scaffold time, in
// one latest-op call.
func scaffoldDeps() []scaffoldDep {
	return append(append([]scaffoldDep{}, scaffoldTestDeps...), scaffoldDevDeps...)
}

// scaffoldRepos replaces the repositories the scaffold's latest lookups read.
// It is nil in production, where the lookups read the scaffolded workspace's
// repositories like any other op does; tests set it to point the lookups at a
// local metadata server.
var scaffoldRepos map[string]any

// depPairs renders coord {:mvn/version version} ... for the scaffold's deps,
// without the surrounding braces.
func depPairs(deps []scaffoldDep, versions map[string]string) string {
	var sb strings.Builder
	for i, d := range deps {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%s {:mvn/version %q}", d.coord, versions[d.coord])
	}
	return sb.String()
}

// moduleDepsEdn is the scaffolded module manifest: a runnable, testable Clojure
// module that pins the Clojure runtime and the scaffold's test deps.
func moduleDepsEdn(lib, main string, versions map[string]string) string {
	return "{:rig/lib " + lib + "\n" +
		" :rig/main " + main + "\n" +
		" :paths [\"src\"]\n" +
		" :deps {org.clojure/clojure {:mvn/version \"" + appClojureVersion + "\"}}\n" +
		" :aliases\n" +
		" {:test {:extra-deps {" + depPairs(scaffoldTestDeps, versions) + "}\n" +
		"         :extra-paths [\"test\"]\n" +
		"         :exec-fn kaocha.runner/exec-fn}}}\n"
}

func coreClj(pkg string) string {
	return "(ns " + pkg + ".core)\n" +
		"\n" +
		"(defn greet [who]\n" +
		"  (str \"hello \" who))\n" +
		"\n" +
		"(defn -main [& args]\n" +
		"  (println (greet (or (first args) \"world\"))))\n"
}

// devDepsEdn is the dev module manifest: the app module by local root
// (relative to the dev directory), the Clojure runtime (a local-root module
// contributes its paths, not its deps), and the reloaded-workflow library.
func devDepsEdn(appCoord, moduleDir string, versions map[string]string) string {
	return "{:paths [\".\"]\n" +
		" :deps {" + appCoord + " {:local/root \"../" + moduleDir + "\"}\n" +
		"        org.clojure/clojure {:mvn/version \"" + appClojureVersion + "\"}\n" +
		"        " + depPairs(scaffoldDevDeps, versions) + "}}\n"
}

// userClj is the dev module's scratch namespace: the reloaded-workflow
// machinery, wired in once the app grows a component system.
func userClj() string {
	return "(ns user\n" +
		"  \"Scratch for the development REPL: rig nrepl -p dev.\n" +
		"\n" +
		"  When the app grows a component system, wire the reloaded workflow to it:\n" +
		"\n" +
		"    (require '[reloaded.repl :as reloaded])\n" +
		"    (reloaded/set-init! #'start-system)\n" +
		"    (reloaded/go)     ;; start, or reload changed namespaces and restart\n" +
		"    (reloaded/stop)\n" +
		"    (reloaded/reset)  ;; stop, unload every namespace, start again\n" +
		"  \")\n"
}

func coreTestClj(pkg string) string {
	return "(ns " + pkg + ".core-test\n" +
		"  (:require [clojure.test :refer :all]\n" +
		"            [" + pkg + ".core :as core]))\n" +
		"\n" +
		"(deftest greet-test\n" +
		"  (is (= \"hello world\" (core/greet \"world\"))))\n"
}

// writeFiles lays down relPath → content files under base.
func writeFiles(base string, files map[string]string) error {
	for relPath, content := range files {
		p := filepath.Join(base, relPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeModule lays down the module template (manifest + src + test) under
// base/moduleRel.
func writeModule(base, moduleRel, lib, main, pkg string, versions map[string]string) error {
	pkgPath := strings.Split(pkg, ".")
	src := append(append([]string{moduleRel, "src"}, pkgPath...), "core.clj")
	test := append(append([]string{moduleRel, "test"}, pkgPath...), "core_test.clj")
	return writeFiles(base, map[string]string{
		filepath.Join(moduleRel, "deps.edn"): moduleDepsEdn(lib, main, versions),
		filepath.Join(src...):                coreClj(pkg),
		filepath.Join(test...):               coreTestClj(pkg),
	})
}

// writeDev lays down the workspace's development module under base/dev: one
// dev directory for the whole workspace, loading the app module and the
// reloaded-workflow library.
func writeDev(base, appCoord, moduleDir string, versions map[string]string) error {
	return writeFiles(base, map[string]string{
		filepath.Join("dev", "deps.edn"): devDepsEdn(appCoord, moduleDir, versions),
		filepath.Join("dev", "user.clj"): userClj(),
	})
}

func newNewCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "new <group/name>",
		Short: "Scaffold a new single-module project",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitf(2, "usage: rig new <group/name>")
			}
			coord := args[0]
			if !coordRe.MatchString(coord) {
				return exitf(2, "invalid coordinate %q (want group/name)", coord)
			}
			name := coord[strings.LastIndex(coord, "/")+1:]
			if st, _ := os.Stat(name); st != nil {
				return exitf(1, "%s already exists", name)
			}
			pkg := strings.ReplaceAll(coord, "/", ".")
			main := pkg + ".core"
			jvmPin, jvmNote := o.newJVMPin(cmd.Context())
			if err := os.MkdirAll(name, 0o755); err != nil {
				return err
			}
			rootDeps := "{:rig/modules [\"modules/" + name + "\" \"dev\"]"
			if jvmPin != "" {
				rootDeps += " :rig/jvm \"" + jvmPin + "\""
			}
			rootDeps += "}\n"
			if err := os.WriteFile(filepath.Join(name, "deps.edn"), []byte(rootDeps), 0o644); err != nil {
				return err
			}
			// The module manifests go down after the lookup: the kernel answers
			// from the repositories and cooldown policy of the workspace, which is
			// on disk from the line above.
			versions, notes := o.scaffoldVersions(cmd.Context(), name)
			if err := writeModule(name, "modules/"+name, coord, main, pkg, versions); err != nil {
				return err
			}
			if err := writeDev(name, coord, "modules/"+name, versions); err != nil {
				return err
			}
			fmt.Printf("created project %s (%s)\n", name, coord)
			for _, note := range append([]string{jvmNote}, notes...) {
				if note != "" {
					fmt.Printf("%s\n", note)
				}
			}
			fmt.Printf("next: cd %s && rig lock && rig test\n", name)
			return nil
		},
	}
}

// newJVMPin returns the :rig/jvm value a scaffolded project starts with: the
// current LTS feature version from the Adoptium info endpoint. It returns ""
// under --offline, and "" plus a note line when the lookup fails — the
// scaffold proceeds, just without a pin.
func (o *opts) newJVMPin(ctx context.Context) (pin, note string) {
	if o.offline {
		return "", ""
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lts, err := jdk.NewAPI("").LatestLTS(lctx)
	if err != nil {
		return "", fmt.Sprintf("note: could not determine the current LTS JVM (%v); scaffolded without a :rig/jvm pin", err)
	}
	return strconv.Itoa(lts), ""
}

// scaffoldVersions returns the version to write for every scaffold coordinate:
// what the repositories of the workspace at ws serve, one latest-op call for
// all of them, or the bundled fallback with a note when the lookup has no
// answer for a coordinate. --offline writes the bundled versions without
// asking, and anything else that stops the lookup writes them with a note — a
// repository that cannot be reached is no reason for `rig new` to fail.
func (o *opts) scaffoldVersions(ctx context.Context, ws string) (map[string]string, []string) {
	deps := scaffoldDeps()
	versions := make(map[string]string, len(deps))
	if o.offline {
		for _, d := range deps {
			versions[d.coord] = d.fallback
		}
		return versions, nil
	}
	coords := make([]string, len(deps))
	for i, d := range deps {
		coords[i] = d.coord
	}
	resolved, reasons, err := o.latestVersions(ctx, ws, coords)
	if err != nil {
		for _, d := range deps {
			versions[d.coord] = d.fallback
		}
		return versions, []string{fmt.Sprintf("note: could not resolve the scaffold's tooling (%v); scaffolded with the bundled versions", err)}
	}
	var notes []string
	for _, d := range deps {
		if v, ok := resolved[d.coord]; ok {
			versions[d.coord] = v
			continue
		}
		versions[d.coord] = d.fallback
		if reasons[d.coord] == "cooldown" {
			notes = append(notes, fmt.Sprintf("note: every %s release is inside its cooldown; scaffolded with %s", d.coord, d.fallback))
		} else {
			notes = append(notes, fmt.Sprintf("note: no repository of the workspace serves %s; scaffolded with %s", d.coord, d.fallback))
		}
	}
	return versions, notes
}

// latestVersions asks the latest op, in one call, for the newest
// cooldown-eligible version of every coordinate as resolved by the workspace
// at ws: resolved is {coord version} (the coordinates a release was found
// for), reasons the rest, "cooldown" or "not-found".
func (o *opts) latestVersions(ctx context.Context, ws string, coords []string) (resolved, reasons map[string]string, err error) {
	root, err := workspace.Find(ws)
	if err != nil {
		return nil, nil, err
	}
	store, err := o.store()
	if err != nil {
		return nil, nil, err
	}
	jar, err := kernel.Kernel(store)
	if err != nil {
		return nil, nil, err
	}
	java, _, err := o.pickJava(ctx, store, root)
	if err != nil {
		return nil, nil, err
	}
	env, err := o.kernelEnv(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	args := map[string]any{"coords": coords}
	if scaffoldRepos != nil {
		args["repos"] = scaffoldRepos
	}
	out, err := kernel.Call(ctx, jar, java, kernel.Request{
		Op:        "latest",
		Workspace: root.Dir,
		Args:      args,
	}, env...)
	if err != nil {
		return nil, nil, err
	}
	var parsed struct {
		Versions map[string]string `json:"versions"`
		Reasons  map[string]string `json:"reasons"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, nil, fmt.Errorf("bad kernel response: %w", err)
	}
	return parsed.Versions, parsed.Reasons, nil
}

func newNewModuleCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "new-module <name>",
		Short: "Scaffold a module in the workspace (adds it to :rig/modules and re-locks)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitf(2, "usage: rig new-module <name>")
			}
			name := args[0]
			if !nameRe.MatchString(name) {
				return exitf(2, "invalid module name %q", name)
			}
			ctx := cmd.Context()
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			moduleRel := "modules/" + name
			if st, _ := os.Stat(filepath.Join(root.Dir, moduleRel, "deps.edn")); st != nil {
				return exitf(1, "module %s already exists", moduleRel)
			}
			versions, notes := o.scaffoldVersions(ctx, root.Dir)
			if err := writeModule(root.Dir, moduleRel, name, name+".core", name, versions); err != nil {
				return err
			}
			if err := addModuleToRoot(filepath.Join(root.Dir, "deps.edn"), moduleRel); err != nil {
				return err
			}
			lock, err := o.relock(ctx, root)
			if err != nil {
				return err
			}
			printSkipped(lock)
			fmt.Printf("added module %s\n", moduleRel)
			fmt.Printf("wrote %s: %d artifacts, %d modules\n",
				root.LockPath(), len(lock.Artifacts), len(lock.Modules))
			for _, note := range notes {
				if note != "" {
					fmt.Printf("%s\n", note)
				}
			}
			return nil
		},
	}
}

// addModuleToRoot inserts moduleRel into the root manifest's :rig/modules
// vector (creating it when absent) and rewrites the manifest.
func addModuleToRoot(rootFile, moduleRel string) error {
	b, err := os.ReadFile(rootFile)
	if err != nil {
		return err
	}
	v, err := ednlit.Parse(string(b))
	if err != nil {
		return fmt.Errorf("parse %s: %w", rootFile, err)
	}
	m, ok := v.(ednlit.Map)
	if !ok {
		return fmt.Errorf("%s: expected a map", rootFile)
	}
	key := ednlit.Keyword{NS: "rig", Name: "modules"}
	for i := range m {
		if m[i].K != key {
			continue
		}
		vec, _ := m[i].V.([]any)
		for _, e := range vec {
			if e == moduleRel {
				return nil
			}
		}
		m[i].V = append(vec, moduleRel)
		return os.WriteFile(rootFile, []byte(ednlit.Format(v)), 0o644)
	}
	m = append(m, ednlit.Pair{K: key, V: []any{moduleRel}})
	return os.WriteFile(rootFile, []byte(ednlit.Format(v)), 0o644)
}
