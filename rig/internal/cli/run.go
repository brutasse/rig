package cli

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
)

func newRunCmd(o *opts) *cobra.Command {
	var alias string
	c := &cobra.Command{
		Use:   "run [args...]",
		Short: "Run the target module's main",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMain(cmd.Context(), o, alias, args)
		},
	}
	c.Flags().StringVar(&alias, "alias", "", "deps alias to run under")
	// Everything after the first positional arg is passed to the main.
	c.Flags().SetInterspersed(false)
	return c
}

func runMain(ctx context.Context, o *opts, alias string, args []string) error {
	e, err := o.hot(ctx, false)
	if err != nil {
		return err
	}
	m, err := e.targetModule(o.path)
	if err != nil {
		return err
	}
	mod, err := e.module(m)
	if err != nil {
		return err
	}
	// run never compiles the target itself: its declared java sources
	// are javac'd here.
	if err := e.ensurePreps(ctx, m, mod, true); err != nil {
		return err
	}
	if mod.Main == "" {
		return exitf(2, "module %s declares no main", m)
	}
	al, env, err := e.alias(m, alias)
	if err != nil {
		return err
	}
	cp, err := e.cpOf(ctx, m, alias)
	if err != nil {
		return err
	}
	runArgs := append([]string{}, mod.JVMOpts...)
	runArgs = append(runArgs, al.JVMOpts...)
	runArgs = append(runArgs, "-cp", cp, "clojure.main", "-m", mod.Main)
	runArgs = append(runArgs, args...)
	return launch(jvm.Run{Java: e.java, Args: runArgs, Dir: e.modDir(m), Env: append(env, e.javaEnv...)})
}

func newNreplCmd(o *opts) *cobra.Command {
	var alias, host string
	var port int
	c := &cobra.Command{
		Use:   "nrepl",
		Short: "Start an nREPL server on the target module's classpath",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNrepl(cmd.Context(), o, alias, host, port)
		},
	}
	c.Flags().StringVar(&alias, "alias", "", "deps alias to run under")
	c.Flags().StringVar(&host, "host", "", "interface to bind (default: nREPL's, loopback)")
	c.Flags().IntVar(&port, "port", 0, "port to bind (default: a free port, printed on startup)")
	return c
}

// nreplClojureFloor is the lowest org.clojure/clojure the vendored nREPL
// loads on: nREPL 1.x is built against Clojure 1.10.
const nreplClojureFloor = "1.10.0"

func runNrepl(ctx context.Context, o *opts, alias, host string, port int) error {
	e, err := o.hot(ctx, false)
	if err != nil {
		return err
	}
	m, err := e.targetModule(o.path)
	if err != nil {
		return err
	}
	mod, err := e.module(m)
	if err != nil {
		return err
	}
	// nrepl never compiles the target itself: its declared java sources
	// are javac'd here.
	if err := e.ensurePreps(ctx, m, mod, true); err != nil {
		return err
	}
	if v := e.clojureBelowFloor(m, nreplClojureFloor); v != "" {
		return exitf(2, "module %s locks org.clojure/clojure %s: rig nrepl's nREPL %s requires %s or newer (raise the pin, or declare nrepl/nrepl in an alias and run it with rig exec)",
			m, v, kernel.NreplVersion, nreplClojureFloor)
	}
	al, env, err := e.alias(m, alias)
	if err != nil {
		return err
	}
	cp, err := e.cpOf(ctx, m, alias)
	if err != nil {
		return err
	}
	nreplJar, err := kernel.Nrepl(e.store)
	if err != nil {
		return err
	}
	// The nREPL jar goes on the classpath last: a project that declares
	// nrepl/nrepl (an alias, say) keeps its own version. Self-attach lets
	// nREPL's JVMTI agent stop interrupt-immune evals on Java 21+.
	cp += string(filepath.ListSeparator) + nreplJar
	runArgs := append([]string{}, mod.JVMOpts...)
	runArgs = append(runArgs, al.JVMOpts...)
	runArgs = append(runArgs, "-Djdk.attach.allowAttachSelf", "-cp", cp, "clojure.main", "-m", "nrepl.cmdline")
	if host != "" {
		runArgs = append(runArgs, "--host", host)
	}
	if port != 0 {
		runArgs = append(runArgs, "--port", strconv.Itoa(port))
	}
	// --interactive last: it attaches this terminal's console to the
	// server (quitting the console stops it), and everything after it is
	// passthrough to the boot namespace, not flags.
	runArgs = append(runArgs, "--interactive")
	return launch(jvm.Run{Java: e.java, Args: runArgs, Dir: e.modDir(m), Env: append(env, e.javaEnv...)})
}

func newExecCmd(o *opts) *cobra.Command {
	var alias string
	c := &cobra.Command{
		Use:   "exec <command> [args...]",
		Short: "Run a command with the locked classpath as CLASSPATH",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExec(cmd.Context(), o, alias, args)
		},
	}
	c.Flags().StringVar(&alias, "alias", "", "deps alias for the classpath")
	// Everything after the first positional arg belongs to the command.
	c.Flags().SetInterspersed(false)
	return c
}

func runExec(ctx context.Context, o *opts, alias string, args []string) error {
	e, err := o.hot(ctx, false)
	if err != nil {
		return err
	}
	m, err := e.targetModule(o.path)
	if err != nil {
		return err
	}
	mod, err := e.module(m)
	if err != nil {
		return err
	}
	al, env, err := e.alias(m, alias)
	if err != nil {
		return err
	}
	cp, err := e.cpOf(ctx, m, alias)
	if err != nil {
		return err
	}
	env = append(env, "CLASSPATH="+cp)
	jo := append(append([]string{}, mod.JVMOpts...), al.JVMOpts...)
	if len(jo) > 0 {
		env = append(env, "JAVA_OPTS="+strings.Join(jo, " "))
	}
	return launch(jvm.Run{Java: args[0], Args: args[1:], Dir: e.modDir(m), Env: append(env, e.javaEnv...)})
}
