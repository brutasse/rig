package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/ednlit"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
)

func newTestCmd(o *opts) *cobra.Command {
	var timeout string
	cmd := &cobra.Command{
		Use:   "test [opt value...]",
		Short: "Run the tests of the target modules",
		Long: `Runs each target module's test exec-fn on its locked test classpath.
Without -p, every module that declares a test exec-fn is run, in lock
order. EDN arguments are forwarded to the exec-fn as key/value opts:

  rig test :kaocha.filter/focus '[:unit]'

An exec-fn that never returns — a test runner wedged on a dead child
process, say — holds the module's slot forever; the runner JVM cannot be
trusted to exit on its own. A watchdog kills it and fails the run: by
default after 30m, set with --timeout, 0 disables.

  rig test --timeout 15m`,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := time.ParseDuration(timeout)
			if err != nil {
				return exitf(2, "test --timeout %q: %v", timeout, err)
			}
			return runTest(cmd.Context(), o, args, d)
		},
	}
	cmd.Flags().StringVar(&timeout, "timeout", "30m",
		"kill a test exec-fn that has not finished after this duration (0 disables)")
	return cmd
}

func runTest(ctx context.Context, o *opts, args []string, timeout time.Duration) error {
	e, err := o.hot(ctx, true)
	if err != nil {
		return err
	}
	// rig.runner jar only, not the kernel jar (same reason as check).
	if e.runner, err = kernel.Current.Runner(ctx, e.store, e.offline); err != nil {
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
		if v := e.clojureBelowFloor(m); v != "" {
			return fmt.Errorf("test: %s: org.clojure/clojure %s is below the %s floor: the rig runner cannot load on Clojure 1.7.x; upgrade the pin", m, v, runnerClojureFloor)
		}
		// test never compiles the target itself: its declared java
		// sources are javac'd here.
		if err := e.ensurePreps(ctx, m, mod, true); err != nil {
			return err
		}
		al := mod.Aliases["test"]
		cp, err := e.cpOf(ctx, m, "test")
		if err != nil {
			return err
		}
		// Project classpath first, runner jar last (rig.runner lives in
		// it; the project's own Clojure wins on conflicts). The runner is
		// its own artifact so the kernel's bundled dependencies stay off
		// the project's classpath.
		full := cp + string(filepath.ListSeparator) + e.runner
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
		runCtx := ctx
		var cancel context.CancelFunc
		if timeout > 0 {
			// Watchdog: a wedged exec-fn is killed at the deadline and
			// fails the run — hanging CI is worse than red CI.
			runCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		err = jvm.Run{Ctx: runCtx, Java: e.java, Args: runArgs, Dir: e.modDir(m), Env: append(envSlice(al.Env), e.javaEnv...)}.Run()
		if cancel != nil {
			stuck := errors.Is(runCtx.Err(), context.DeadlineExceeded)
			cancel()
			if stuck {
				return exitf(1, "test: %s: exec-fn %s did not finish in %s, killed (--timeout to raise, --timeout 0 to disable)", m, al.Exec.Fn, timeout)
			}
		}
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
