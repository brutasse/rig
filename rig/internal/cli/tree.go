package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/workspace"
)

func newTreeCmd(o *opts) *cobra.Command {
	var alias string
	cmd := &cobra.Command{
		Use:   "tree",
		Short: "Dependency tree",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			return o.treePrint(ctx, root, o.path, alias)
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "include the alias's extra-deps")
	return cmd
}

func (o *opts) treePrint(ctx context.Context, root *workspace.Root, module, alias string) error {
	if module == "" {
		module = "."
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	jar, err := kernel.Current.Ensure(ctx, store, o.offline)
	if err != nil {
		return err
	}
	java, _, err := o.pickJava(ctx, store, root)
	if err != nil {
		return err
	}
	env, err := o.kernelEnv(ctx, root)
	if err != nil {
		return err
	}
	penv, err := o.kernelProxy(ctx, root)
	if err != nil {
		return err
	}
	defer o.closeProxy()
	out, err := kernel.Call(ctx, jar, java, kernel.Request{
		Op:        "tree",
		Workspace: root.Dir,
		Lock:      "deps.lock",
		Args:      map[string]any{"module": module, "alias": alias, "offline": o.offline},
	}, append(env, penv...)...)
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return exitf(1, "tree: %v", oe)
		}
		return err
	}
	// The kernel renders the tree itself (it holds the resolved graph);
	// this side is a dumb printer.
	var parsed struct {
		Tree string `json:"tree"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return fmt.Errorf("tree: bad kernel response: %w", err)
	}
	fmt.Println(parsed.Tree)
	return nil
}
