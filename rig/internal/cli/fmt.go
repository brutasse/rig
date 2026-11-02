package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/workspace"
)

func newFmtCmd(o *opts) *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use:   "fmt",
		Short: "Format the target module with the pinned cljfmt",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFmt(cmd.Context(), o, check)
		},
	}
	c.Flags().BoolVar(&check, "check", false, "check formatting without fixing")
	return c
}

// runFmt formats (or checks) the module with the cljfmt pinned in the
// kernel jar. No workspace lock needed; the kernel jar is fetched by its
// pinned hash.
func runFmt(ctx context.Context, o *opts, check bool) error {
	root, err := workspace.Find(".")
	if err != nil {
		return err
	}
	m, err := resolveModule(root, o.path)
	if err != nil {
		return err
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	jar, err := kernel.Kernel(store)
	if err != nil {
		return err
	}
	java, javaEnv, err := o.pickJava(ctx, store, root)
	if err != nil {
		return err
	}
	mode := "fix"
	if check {
		mode = "check"
	}
	dir := moduleDir(root, m)
	return launch(jvm.Run{Java: java, Args: []string{"-cp", jar, "rig.fmt", mode, dir}, Dir: dir, Env: javaEnv})
}
