package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/workspace"
)

type migrateEdit struct {
	File    string `json:"file"`
	Changed bool   `json:"changed"`
}

func newMigrateCmd(o *opts) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate legacy deps.edn manifests to the :rig/* keys in place",
		Long: `Migrates every deps.edn in the workspace from the legacy
exoscale.project / deps-modules keys to the :rig/* keys, rewriting the
files in place. The migration is mechanical: it reproduces the effective
(post-merge) dependencies exactly, without version drift.

--dry-run reports what would change without writing. When the kernel
reports blocking problems, no file is written and the command exits 1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), o, dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report the changes without writing")
	return cmd
}

func runMigrate(ctx context.Context, o *opts, dryRun bool) error {
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
	java, _, err := o.pickJava(ctx, store, root)
	if err != nil {
		return err
	}
	req := kernel.Request{
		Op:        "migrate",
		Workspace: root.Dir,
		Args:      map[string]any{"dry-run?": dryRun},
	}
	out, err := kernel.Call(ctx, jar, java, req)
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return exitf(1, "migrate: %v", oe)
		}
		return err
	}
	var res struct {
		Edits    []migrateEdit `json:"edits"`
		Warnings []string      `json:"warnings"`
		Problems []string      `json:"problems"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("migrate: bad kernel response: %w", err)
	}
	for _, p := range res.Problems {
		fmt.Fprintln(os.Stderr, "migrate:", p)
	}
	if len(res.Problems) > 0 {
		return exitf(1, "migrate: %d problem(s), no files written", len(res.Problems))
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "migrate:", w)
	}
	changed := 0
	for _, e := range res.Edits {
		switch {
		case e.Changed && dryRun:
			fmt.Printf("%s: would change\n", e.File)
			changed++
		case e.Changed:
			fmt.Printf("%s: migrated\n", e.File)
			changed++
		default:
			fmt.Printf("%s: unchanged\n", e.File)
		}
	}
	if changed == 0 {
		fmt.Println("migrate: nothing to migrate")
	}
	return nil
}
