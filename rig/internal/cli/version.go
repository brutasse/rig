package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/workspace"
)

// Version and GitSHA are stamped at release build time via
// -ldflags "-X github.com/brutasse/rig/internal/cli.Version=…".
// Local/dev builds report "dev".
var (
	Version = "dev"
	GitSHA  = "dev"
)

func newVersionCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the project version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			var m string
			if o.path != "" {
				if m, err = root.Resolve(o.path); err != nil {
					return err
				}
			} else {
				m = "."
			}
			candidates := []string{filepath.Join(root.Dir, m, "VERSION")}
			if m != "." {
				candidates = append(candidates, filepath.Join(root.Dir, "VERSION"))
			}
			for _, c := range candidates {
				if b, err := os.ReadFile(c); err == nil {
					fmt.Println(strings.TrimSpace(string(b)))
					return nil
				}
			}
			if root.Lock != nil {
				if mod, err := root.Lock.Module(m); err == nil {
					fmt.Println(mod.Version)
					return nil
				}
			}
			return exitf(1, "no version found (no VERSION file and no lock)")
		},
	}
}
