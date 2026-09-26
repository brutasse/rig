package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/graal"
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
			if line, err := o.graalvmInfo(root); err == nil && line != "" {
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

// graalvmInfo returns the `rig info` graalvm line for a lock that pins a
// GraalVM (installed or not); "" when the lock pins nothing.
func (o *opts) graalvmInfo(root *workspace.Root) (string, error) {
	if root == nil || root.Lock == nil || root.Lock.GraalVM == nil {
		return "", nil
	}
	pin := root.Lock.GraalVM
	store, err := o.store()
	if err != nil {
		return "", err
	}
	st := graal.NewStoreAt(store.Root)
	var (
		inst *graal.Inst
		lerr error
	)
	if pin.Version != "" {
		inst, lerr = st.Lookup(pin.Version)
	} else {
		inst, lerr = st.Best(pin.Requested)
	}
	if lerr == nil {
		return fmt.Sprintf("graalvm:\t%s (graalvm %s, pinned %q)\n",
			inst.NativeImagePath, inst.Version, pin.Requested), nil
	}
	return fmt.Sprintf("graalvm:\tpinned %q — graalvm not installed (run 'rig build --native' online to install it)\n",
		pin.Requested), nil
}
