package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/classpath"
	"github.com/brutasse/rig/internal/fetch"
)

func newVerifyCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Verify every lock artifact against the cache (CI security gate)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, lock, err := o.hotLock(ctx)
			if err != nil {
				return err
			}
			if err := o.resolve(ctx, root); err != nil {
				return err
			}
			store, err := o.store()
			if err != nil {
				return err
			}
			var items []fetch.Item
			var gitCount int
			m2 := o.m2Root()
			for _, a := range lock.Artifacts {
				switch a.Kind {
				case "mvn":
					if a.URL != "" {
						items = append(items, fetch.Item{URL: a.URL, Repo: a.Repository, Local: classpath.M2Path(m2, a), SHA: a.SHA256})
					}
				case "git":
					gitCount++
				}
			}
			cached, fetched, err := fetch.VerifyAll(ctx, o.fetchClient(ctx, root), store, items, 2)
			if err != nil {
				switch {
				case errors.Is(err, cache.ErrMismatch):
					return exitf(4, "artifact hash mismatch: %v", err)
				case errors.Is(err, fetch.ErrOffline):
					return exitf(1, "offline: %v", err)
				default:
					return err
				}
			}
			fmt.Printf("verified %d artifacts (%d cached, %d fetched", len(items), cached, fetched)
			if gitCount > 0 {
				fmt.Printf(", %d git deps pinned by commit sha", gitCount)
			}
			fmt.Println(")")
			return nil
		},
	}
}
