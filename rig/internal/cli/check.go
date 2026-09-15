package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/classpath"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/workspace"
)

type checkProblem struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Module   string `json:"module"`
	Coord    string `json:"coord"`
	Message  string `json:"message"`
}

func newCheckCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Verify the workspace is consistent (lock vs manifests, and code loads)",
		Long: `Checks the workspace without modifying it.

Stage 1 verifies the lock against every manifest: stale pins, version
conflicts, drift, floating RELEASE/LATEST, publish without a lib, unknown
repos. Stage 2 loads each target module's namespaces on its locked base
classpath, catching code that does not load. -p restricts stage 2 only;
stage 1 is always workspace-wide.

rig check never re-locks; it reports. It exits 1 when any error-severity
problem is found, 0 otherwise (warnings do not fail the check).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheck(cmd.Context(), o)
		},
	}
}

func runCheck(ctx context.Context, o *opts) error {
	root, err := workspace.Find(".")
	if err != nil {
		return err
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	jar, err := kernel.Current.Ensure(ctx, store, o.offline)
	if err != nil {
		return err
	}
	java, javaEnv, err := o.pickJava(ctx, store, root)
	if err != nil {
		return err
	}
	if err := o.resolve(ctx, root); err != nil {
		return err
	}

	var problems []checkProblem

	// Stage 1: kernel consistency report (workspace-wide, read-only).
	req := kernel.Request{
		Op:        "check",
		Workspace: root.Dir,
		Lock:      filepath.Base(root.LockPath()),
		Args:      map[string]any{},
	}
	out, err := kernel.Call(ctx, jar, java, req)
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return exitf(1, "check: %v", oe)
		}
		return err
	}
	var stage1 struct {
		OK       bool           `json:"ok"`
		Problems []checkProblem `json:"problems"`
	}
	if err := json.Unmarshal(out, &stage1); err != nil {
		return fmt.Errorf("check: bad kernel response: %w", err)
	}
	problems = append(problems, stage1.Problems...)

	// Stage 2: ns-load each target module on its locked base classpath.
	if root.Lock != nil {
		e := &hotEnv{root: root, lock: root.Lock, store: store, m2root: o.m2Root(), oidc: o.bearerFor(ctx, root)}
		e.client = o.fetchClient(ctx, root)
		if e.gitlibs, err = classpath.GitlibsRoot(); err != nil {
			return err
		}
		e.java = java
		e.javaEnv = javaEnv
		e.kernel = jar
		targets, err := targetModules(root, o.path)
		if err != nil {
			return err
		}
		for _, m := range targets {
			p, err := checkModuleLoads(ctx, e, m)
			if err != nil {
				return err
			}
			if p != nil {
				problems = append(problems, *p)
			}
		}
	}

	for _, p := range problems {
		tag := "error"
		if p.Severity == "warn" {
			tag = "warn "
		}
		loc := p.Module
		if p.Coord != "" {
			loc = p.Module + " " + p.Coord
		}
		fmt.Printf("check: %s [%s] %s: %s\n", tag, p.Kind, loc, p.Message)
	}
	errs, warns := 0, 0
	for _, p := range problems {
		if p.Severity == "warn" {
			warns++
		} else {
			errs++
		}
	}
	switch {
	case errs == 0 && warns == 0:
		fmt.Println("check: ok")
	case errs == 0:
		fmt.Printf("check: ok with %d warning(s)\n", warns)
	default:
		return exitf(1, "check: %d error(s), %d warning(s)", errs, warns)
	}
	return nil
}

// checkModuleLoads loads module m's namespaces on its locked base classpath
// via the runner's load-all. It returns a problem when a load fails, nil
// when everything loads (or when the module has no Clojure sources).
func checkModuleLoads(ctx context.Context, e *hotEnv, m string) (*checkProblem, error) {
	mod, err := e.module(m)
	if err != nil {
		return nil, err
	}
	var srcDirs []string
	hasSrc := false
	for _, p := range mod.Paths {
		abs := filepath.Join(e.modDir(m), p)
		srcDirs = append(srcDirs, abs)
		filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".clj") &&
				!strings.HasSuffix(path, "_init.clj") {
				hasSrc = true
			}
			return nil
		})
	}
	if !hasSrc {
		return nil, nil
	}
	cp, err := e.cpOf(ctx, m, "")
	if err != nil {
		return nil, err
	}
	full := cp + string(filepath.ListSeparator) + e.kernel
	runArgs := append([]string{}, mod.JVMOpts...)
	runArgs = append(runArgs, "-cp", full, "rig.runner", "load-all")
	runArgs = append(runArgs, srcDirs...)
	err = jvm.Run{Java: e.java, Args: runArgs, Dir: e.modDir(m), Env: e.javaEnv}.Run()
	if err == nil {
		return nil, nil
	}
	if code := jvm.Code(err); code == 1 {
		return &checkProblem{
			Severity: "error",
			Kind:     "load-fail",
			Module:   m,
			Message:  "one or more namespaces failed to load (see above)",
		}, nil
	}
	return nil, fmt.Errorf("check: load %s: %w", m, err)
}
