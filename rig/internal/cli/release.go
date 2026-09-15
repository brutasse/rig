package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

var releaseVersionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(-snapshot)?$`)

func newReleaseCmd(o *opts) *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "release",
		Short: "Release the current version: publish, tag, then bump to the next snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRelease(cmd.Context(), o, dryRun)
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the release plan without doing anything")
	return c
}

// gitOut runs git in dir and returns its trimmed combined output.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// runRelease performs the release sequence (design §8.1): remove-snapshot →
// publish all → commit VERSION → tag → bump-and-snapshot → commit → push.
// With dryRun it only prints the plan.
func runRelease(ctx context.Context, o *opts, dryRun bool) error {
	root, err := workspace.Find(".")
	if err != nil {
		return err
	}
	if root.Lock == nil {
		return exitf(3, "no lock at %s (run 'rig lock')", root.LockPath())
	}
	vfile := filepath.Join(root.Dir, "VERSION")
	raw, err := os.ReadFile(vfile)
	if err != nil {
		return exitf(2, "release: no VERSION file at %s (release works on the version file, not :rig/version)", vfile)
	}
	current := strings.TrimSpace(string(raw))
	vm := releaseVersionRe.FindStringSubmatch(current)
	if vm == nil {
		return exitf(2, "release: unsupported version %q (want X.Y.Z or X.Y.Z-snapshot)", current)
	}
	patch, err := strconv.Atoi(vm[3])
	if err != nil {
		return exitf(2, "release: unsupported version %q (want X.Y.Z or X.Y.Z-snapshot)", current)
	}
	releaseV := current
	if vm[4] == "-snapshot" {
		releaseV = vm[1] + "." + vm[2] + "." + vm[3]
	}
	nextV := vm[1] + "." + vm[2] + "." + strconv.Itoa(patch+1) + "-snapshot"
	branch, err := gitOut(ctx, root.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return exitf(2, "release: not a git repository")
	}

	modules := publishableOf(root, root.Lock)
	removeDetail := fmt.Sprintf("%s -> %s (VERSION)", current, releaseV)
	commitDetail := fmt.Sprintf("VERSION (\"Release v%s\")", releaseV)
	if releaseV == current {
		removeDetail = "(none to remove)"
		commitDetail = "(skipped, no change)"
	}
	plan := []struct {
		step   string
		detail string
	}{
		{"remove-snapshot", removeDetail},
		{"publish", describePublish(modules, root.Lock, releaseV)},
		{"commit", commitDetail},
		{"tag", "v" + releaseV},
		{"bump-and-snapshot", fmt.Sprintf("%s -> %s (VERSION)", releaseV, nextV)},
		{"commit", fmt.Sprintf("VERSION + deps.lock (\"Bump version to %s\")", nextV)},
		{"push", fmt.Sprintf("origin %s + tag v%s", branch, releaseV)},
	}

	if dryRun {
		if stale, err := root.Lock.Stale(root.Dir); err != nil {
			return err
		} else if len(stale) > 0 {
			fmt.Println("note: lock is stale; 'rig release' will re-lock first")
		}
		fmt.Println("dry run, no changes:")
		for i, p := range plan {
			fmt.Printf("  %d. %-18s %s\n", i+1, p.step, p.detail)
		}
		return nil
	}

	e, err := o.hot(ctx, true)
	if err != nil {
		return err
	}
	modules = publishableOf(e.root, e.lock)

	// 1. remove-snapshot.
	fmt.Printf("1. remove-snapshot: %s\n", removeDetail)
	if releaseV != current {
		if err := os.WriteFile(vfile, []byte(releaseV+"\n"), 0o644); err != nil {
			return err
		}
		lock, err := o.relock(ctx, root)
		if err != nil {
			return err
		}
		e.lock = lock
	}

	// 2. publish all publishable modules.
	fmt.Printf("2. publish: %d module(s)\n", len(modules))
	if err := e.publishModules(ctx, modules, true); err != nil {
		return err
	}

	// 3. commit the release version.
	if releaseV != current {
		if _, err := gitOut(ctx, root.Dir, "add", "VERSION"); err != nil {
			return err
		}
		if _, err := gitOut(ctx, root.Dir, "commit", "-m", "Release v"+releaseV); err != nil {
			return err
		}
	}
	fmt.Printf("3. commit: %s\n", commitDetail)

	// 4. tag.
	if _, err := gitOut(ctx, root.Dir, "rev-parse", "--verify", "--quiet", "refs/tags/v"+releaseV); err == nil {
		return exitf(1, "release: tag v%s already exists", releaseV)
	}
	if _, err := gitOut(ctx, root.Dir, "tag", "v"+releaseV); err != nil {
		return err
	}
	fmt.Printf("4. tag: v%s\n", releaseV)

	// 5. bump-and-snapshot.
	fmt.Printf("5. bump-and-snapshot: %s -> %s\n", releaseV, nextV)
	if err := os.WriteFile(vfile, []byte(nextV+"\n"), 0o644); err != nil {
		return err
	}
	lock, err := o.relock(ctx, root)
	if err != nil {
		return err
	}
	e.lock = lock

	// 6. commit the bump.
	if _, err := gitOut(ctx, root.Dir, "add", "VERSION", "deps.lock"); err != nil {
		return err
	}
	if _, err := gitOut(ctx, root.Dir, "commit", "-m", "Bump version to "+nextV); err != nil {
		return err
	}
	fmt.Printf("6. commit: \"Bump version to %s\"\n", nextV)

	// 7. push.
	if _, err := gitOut(ctx, root.Dir, "push", "origin", branch); err != nil {
		return err
	}
	if _, err := gitOut(ctx, root.Dir, "push", "origin", "v"+releaseV); err != nil {
		return err
	}
	fmt.Printf("7. push: origin %s + v%s\n", branch, releaseV)
	return nil
}

// describePublish formats the publish step of the release plan.
func describePublish(modules []string, lock *lockfile.Document, version string) string {
	if len(modules) == 0 {
		return "(no publishable modules)"
	}
	parts := make([]string, 0, len(modules))
	for _, m := range modules {
		mod, err := lock.Module(m)
		if err != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s -> %s", mod.Lib, version, mod.Publish.Repo))
	}
	return strings.Join(parts, ", ")
}
