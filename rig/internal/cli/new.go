package cli

import (
	"context"
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
	"github.com/brutasse/rig/internal/workspace"
)

var (
	coordRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*/[a-z0-9][a-z0-9.-]*$`)
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
)

// moduleDepsEdn is the scaffolded module manifest: a runnable, testable Clojure
// module that pins only the Clojure runtime and the test runner.
func moduleDepsEdn(lib, main string) string {
	return "{:rig/lib " + lib + "\n" +
		" :rig/main " + main + "\n" +
		" :paths [\"src\"]\n" +
		" :deps {org.clojure/clojure {:mvn/version \"1.12.5\"}}\n" +
		" :aliases\n" +
		" {:test {:extra-deps {lambdaisland/kaocha {:mvn/version \"1.66.1034\"}}\n" +
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

func coreTestClj(pkg string) string {
	return "(ns " + pkg + ".core-test\n" +
		"  (:require [clojure.test :refer :all]\n" +
		"            [" + pkg + ".core :as core]))\n" +
		"\n" +
		"(deftest greet-test\n" +
		"  (is (= \"hello world\" (core/greet \"world\"))))\n"
}

// writeModule lays down the module template (manifest + src + test) under
// base/moduleRel.
func writeModule(base, moduleRel, lib, main, pkg string) error {
	pkgPath := strings.Split(pkg, ".")
	src := append(append([]string{moduleRel, "src"}, pkgPath...), "core.clj")
	test := append(append([]string{moduleRel, "test"}, pkgPath...), "core_test.clj")
	files := map[string]string{
		filepath.Join(moduleRel, "deps.edn"): moduleDepsEdn(lib, main),
		filepath.Join(src...):                coreClj(pkg),
		filepath.Join(test...):               coreTestClj(pkg),
	}
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
			jvmPin, note := o.newJVMPin(cmd.Context())
			if err := os.MkdirAll(name, 0o755); err != nil {
				return err
			}
			rootDeps := "{:rig/modules [\"modules/" + name + "\"]"
			if jvmPin != "" {
				rootDeps += " :rig/jvm \"" + jvmPin + "\""
			}
			rootDeps += "}\n"
			if err := os.WriteFile(filepath.Join(name, "deps.edn"), []byte(rootDeps), 0o644); err != nil {
				return err
			}
			if err := writeModule(name, "modules/"+name, coord, main, pkg); err != nil {
				return err
			}
			fmt.Printf("created project %s (%s)\n", name, coord)
			if note != "" {
				fmt.Printf("%s\n", note)
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
			if err := writeModule(root.Dir, moduleRel, name, name+".core", name); err != nil {
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
