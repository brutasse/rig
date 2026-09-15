package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/workspace"
)

func newCleanCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove build output directories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			modules, err := targetModules(root, o.path)
			if err != nil {
				return err
			}
			for _, m := range modules {
				dir := filepath.Join(root.Dir, m, root.TargetDir(m))
				if err := os.RemoveAll(dir); err != nil {
					return err
				}
				fmt.Printf("cleaned %s\n", dir)
			}
			return nil
		},
	}
}
