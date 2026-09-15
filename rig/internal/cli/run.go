package cli

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/jvm"
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

func newReplCmd(o *opts) *cobra.Command {
	var alias string
	c := &cobra.Command{
		Use:   "repl",
		Short: "Start a Clojure REPL on the target module's classpath",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepl(cmd.Context(), o, alias)
		},
	}
	c.Flags().StringVar(&alias, "alias", "", "deps alias to run under")
	return c
}

func runRepl(ctx context.Context, o *opts, alias string) error {
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
	runArgs := append([]string{}, mod.JVMOpts...)
	runArgs = append(runArgs, al.JVMOpts...)
	runArgs = append(runArgs, "-cp", cp, "clojure.main")
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
