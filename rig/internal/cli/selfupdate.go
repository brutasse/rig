package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/updater"
)

// apiBase is the GitHub API base for release lookups (overridable in tests).
func apiBase() string {
	if v := os.Getenv("RIG_UPDATES_API"); v != "" {
		return v
	}
	return updater.DefaultAPI
}

// rigExecutable is the path of the running rig binary (overridden in tests).
var rigExecutable = os.Executable

func newSelfUpdateCmd(o *opts) *cobra.Command {
	var checkOnly bool
	var target string
	c := &cobra.Command{
		Use:   "self-update",
		Short: "Update the rig binary from the GitHub releases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate(cmd.Context(), o, checkOnly, target)
		},
	}
	c.Flags().BoolVar(&checkOnly, "check", false, "only report the available version")
	c.Flags().StringVar(&target, "version", "", "update to this exact version (vX.Y.Z); default: latest")
	return c
}

func runSelfUpdate(ctx context.Context, o *opts, checkOnly bool, target string) error {
	if Version == "dev" {
		return exitf(2, "dev build: 'rig self-update' is only available on release builds")
	}
	if o.offline {
		return exitf(1, "offline: 'rig self-update' needs the network")
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	var (
		rel *updater.Release
		err error
	)
	if target == "" {
		rel, err = updater.Latest(ctx, client, apiBase())
	} else {
		rel, err = updater.Get(ctx, client, apiBase(), target)
	}
	if err != nil {
		return exitf(1, "self-update: %v", err)
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if checkOnly {
		switch updater.CompareVersions(latest, Version) {
		case 1:
			fmt.Printf("rig %s available (running %s)\n", rel.Tag, Version)
		case 0:
			fmt.Printf("rig %s is up to date\n", Version)
		default:
			fmt.Printf("running %s is newer than %s\n", Version, rel.Tag)
		}
		return nil
	}
	if target == "" && updater.CompareVersions(latest, Version) <= 0 {
		fmt.Printf("rig %s is up to date\n", Version)
		return nil
	}
	exe, err := rigExecutable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	probe, err := os.CreateTemp(dir, ".rig-writable-")
	if err != nil {
		return exitf(1, "self-update: cannot write to %s (install to a writable dir, e.g. ~/.local/bin)", dir)
	}
	probe.Close()
	os.Remove(probe.Name())
	path, err := updater.InstallBinary(ctx, client, rel, runtime.GOOS, runtime.GOARCH, dir)
	if err != nil {
		return exitf(1, "self-update: %v", err)
	}
	selfUpdated = true
	if store, err := o.store(); err == nil {
		writeUpdateCheck(store.Root, rel.Tag)
	}
	fmt.Printf("updated rig %s -> %s at %s\n", Version, rel.Tag, path)
	fmt.Println("the matching kernel jar is fetched on next use")
	return nil
}
