package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/ednlit"
	"github.com/brutasse/rig/internal/jvm"
)

func newTestCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "test [opt value...]",
		Short: "Run the tests of the target modules",
		Long: `Runs each target module's test exec-fn on its locked test classpath.
Without -p, every module that declares a test exec-fn is run, in lock
order. EDN arguments are forwarded to the exec-fn as key/value opts:

  rig test :kaocha.filter/focus '[:unit]'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTest(cmd.Context(), o, args)
		},
	}
}

func runTest(ctx context.Context, o *opts, args []string) error {
	e, err := o.hot(ctx, true)
	if err != nil {
		return err
	}
	optsText, err := testOpts(args)
	if err != nil {
		return err
	}
	targets, err := testTargets(e, o.path)
	if err != nil {
		return err
	}
	var failed []string
	for _, m := range targets {
		mod := e.lock.Modules[m]
		al := mod.Aliases["test"]
		cp, err := e.cpOf(ctx, m, "test")
		if err != nil {
			return err
		}
		// Project classpath first, kernel jar last (rig.runner lives in it;
		// the project's own Clojure wins on conflicts).
		full := cp + string(filepath.ListSeparator) + e.kernel
		optsFile, err := os.CreateTemp("", "rig-test-opts-*.edn")
		if err != nil {
			return err
		}
		name := optsFile.Name()
		if _, werr := optsFile.WriteString(optsText); werr != nil {
			optsFile.Close()
			os.Remove(name)
			return werr
		}
		if cerr := optsFile.Close(); cerr != nil {
			os.Remove(name)
			return cerr
		}
		defer os.Remove(name)

		runArgs := append([]string{}, mod.JVMOpts...)
		runArgs = append(runArgs, al.JVMOpts...)
		runArgs = append(runArgs, "-cp", full, "rig.runner", "test", al.Exec.Fn, name)
		err = jvm.Run{Java: e.java, Args: runArgs, Dir: e.modDir(m), Env: append(envSlice(al.Env), e.javaEnv...)}.Run()
		if err == nil {
			continue
		}
		if code := jvm.Code(err); code == 1 {
			failed = append(failed, m)
			continue
		}
		return fmt.Errorf("test: %s: %w", m, err)
	}
	if len(failed) > 0 {
		return exitf(1, "tests failed in: %s", strings.Join(failed, ", "))
	}
	return nil
}

// testOpts parses the EDN args as key/value pairs and returns the opts map
// formatted back to EDN.
func testOpts(args []string) (string, error) {
	if len(args)%2 != 0 {
		return "", exitf(2, "test opts must be key/value pairs, got %d args", len(args))
	}
	m := ednlit.Map{}
	for i := 0; i < len(args); i += 2 {
		k, err := ednlit.Parse(args[i])
		if err != nil {
			return "", exitf(2, "test opt key %q: %v", args[i], err)
		}
		v, err := ednlit.Parse(args[i+1])
		if err != nil {
			return "", exitf(2, "test opt value %q: %v", args[i+1], err)
		}
		m = append(m, ednlit.Pair{K: k, V: v})
	}
	return ednlit.Format(m), nil
}

// testTargets returns the modules to test: the -p module, or (without -p)
// every locked module that declares a test exec-fn, in lock order.
func testTargets(e *hotEnv, p string) ([]string, error) {
	if p != "" && p != "." {
		m, err := e.root.Resolve(p)
		if err != nil {
			return nil, err
		}
		if _, err := e.module(m); err != nil {
			return nil, err
		}
		al, ok := e.lock.Modules[m].Aliases["test"]
		if !ok || al.Exec == nil || al.Exec.Fn == "" {
			return nil, exitf(2, "module %s declares no test exec-fn", m)
		}
		return []string{m}, nil
	}
	var out []string
	for _, m := range e.root.Modules() {
		mod, err := e.lock.Module(m)
		if err != nil {
			continue
		}
		if al, ok := mod.Aliases["test"]; ok && al.Exec != nil && al.Exec.Fn != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, exitf(2, "no module in the lock declares a test exec-fn")
	}
	return out, nil
}
