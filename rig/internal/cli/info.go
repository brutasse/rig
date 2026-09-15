package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/workspace"
)

func newInfoCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show project and tool information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			kind := "single-module"
			if root.Workspace {
				kind = "workspace"
			}
			fmt.Printf("project:\t%s\n", root.Dir)
			fmt.Printf("type:\t%s\n", kind)
			fmt.Printf("modules:\t%s\n", strings.Join(root.Modules(), ", "))
			if root.Lock == nil {
				fmt.Printf("lock:\tmissing (run 'rig lock')\n")
			} else {
				l := root.Lock
				fmt.Printf("lock:\t%s by %s %s (%s), %d artifacts\n",
					l.LockedAt.UTC().Format("2006-01-02T15:04:05Z"),
					l.Resolver.Lib, l.Resolver.Version, shortSHA(l.Resolver.GitSHA),
					len(l.Artifacts))
				if stale, err := l.Stale(root.Dir); err == nil && len(stale) > 0 {
					fmt.Printf("stale:\t%s\n", strings.Join(stale, ", "))
				}
			}
			store, err := o.store()
			if err != nil {
				return err
			}
			if line, err := o.javaInfo(root); err == nil {
				fmt.Print(line)
			}
			fmt.Printf("cache:\t%s\n", store.Root)
			p := kernel.Current
			fmt.Printf("rig:\t%s (%s)\n", Version, shortSHA(GitSHA))
			fmt.Printf("kernel:\t%s %s (%s)\n", p.Lib, p.Version, shortSHA(p.GitSHA))
			return nil
		},
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
