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

// outdatedUpdate is one pinned coord with a newer version available.
type outdatedUpdate struct {
	Coord            string `json:"coord"`
	Current          string `json:"current"`
	LatestSatisfying string `json:"latest-satisfying"`
	Latest           string `json:"latest"`
	Breaking         bool   `json:"breaking"`
}

func newOutdatedCmd(o *opts) *cobra.Command {
	var breaking bool
	cmd := &cobra.Command{
		Use:   "outdated",
		Short: "Pinned vs available versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOutdated(cmd.Context(), o, breaking)
		},
	}
	cmd.Flags().BoolVar(&breaking, "breaking", false, "only list breaking updates")
	return cmd
}

func runOutdated(ctx context.Context, o *opts, breakingOnly bool) error {
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
	env, err := o.kernelEnv(ctx, root)
	if err != nil {
		return err
	}
	out, err := kernel.Call(ctx, jar, java, kernel.Request{
		Op:        "outdated",
		Workspace: root.Dir,
		Lock:      "deps.lock",
		Args:      map[string]any{"offline": o.offline, "breaking": breakingOnly},
	}, env...)
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return exitf(1, "outdated: %v", oe)
		}
		return err
	}
	var parsed struct {
		Updates []outdatedUpdate `json:"updates"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return fmt.Errorf("outdated: bad kernel response: %w", err)
	}
	if len(parsed.Updates) == 0 {
		fmt.Println("up to date")
		return nil
	}
	for _, u := range parsed.Updates {
		mark := ""
		if u.Breaking {
			mark = ", breaking"
		}
		fmt.Printf("%s %s -> %s (latest %s%s)\n",
			u.Coord, u.Current, u.LatestSatisfying, u.Latest, mark)
	}
	return nil
}
