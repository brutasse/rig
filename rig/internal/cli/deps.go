package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

// manifestEdit is one manifest change reported by the kernel edit-dep op.
type manifestEdit struct {
	File        string  `json:"file"`
	Action      string  `json:"action"`
	Coord       string  `json:"coord"`
	Requirement *string `json:"requirement"`
}

// editModule is the target module for manifest edits: -p, else the root.
func (o *opts) editModule() string {
	if o.path != "" {
		return o.path
	}
	return "."
}

func newAddCmd(o *opts) *cobra.Command {
	var alias string
	var shared bool
	cmd := &cobra.Command{
		Use:   "add <coord> [version]",
		Short: "Add a dependency (no version: newest eligible one)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			version := "latest"
			if len(args) == 2 {
				version = args[1]
			}
			return o.editAndPrint(ctx, root, o.editModule(), args[0], version, alias, shared, false)
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "add under :aliases/<alias>/:extra-deps")
	cmd.Flags().BoolVar(&shared, "shared", true, "record in :rig/deps and propagate to modules")
	return cmd
}

func newRemoveCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <coord>",
		Short: "Remove a dependency (shared, from all modules)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			return o.editAndPrint(ctx, root, o.editModule(), args[0], "", "", true, false)
		},
	}
}

func newUpdateCmd(o *opts) *cobra.Command {
	var alias string
	var sharedOnly bool
	cmd := &cobra.Command{
		Use:   "update [coord [version]]",
		Short: "Update a dependency, or re-resolve all floating versions",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			if len(args) == 0 {
				lock, err := o.relockArgs(ctx, root, map[string]any{"respect-existing-pins": false})
				if err != nil {
					return err
				}
				printSkipped(lock)
				fmt.Printf("wrote %s: %d artifacts, %d modules\n",
					root.LockPath(), len(lock.Artifacts), len(lock.Modules))
				return nil
			}
			version := "latest"
			if len(args) == 2 {
				version = args[1]
			}
			return o.editAndPrint(ctx, root, o.editModule(), args[0], version, alias, true, sharedOnly)
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "update under :aliases/<alias>/:extra-deps")
	cmd.Flags().BoolVar(&sharedOnly, "shared-only", false, "update only the shared requirement in :rig/deps")
	return cmd
}

// manifestSnap is one pre-edit manifest (raw bytes + permissions) kept
// in memory so a failed edit can restore it byte-for-byte.
type manifestSnap struct {
	data []byte
	mode os.FileMode
}

// snapshotManifests reads every manifest in the workspace (the root and all
// module manifests, at any depth) before an edit can touch one.
func snapshotManifests(root *workspace.Root) (map[string]manifestSnap, error) {
	snap := map[string]manifestSnap{}
	err := filepath.WalkDir(root.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "deps.edn" {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			st, err := os.Stat(path)
			if err != nil {
				return err
			}
			snap[path] = manifestSnap{data: b, mode: st.Mode().Perm()}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// revertManifests restores every manifest that no longer matches its
// snapshot and reports the restored files on stderr.
func revertManifests(snap map[string]manifestSnap) {
	n := 0
	for path, s := range snap {
		cur, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(cur, s.data) {
			if err := os.WriteFile(path, s.data, s.mode); err == nil {
				n++
			}
		}
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "reverted %d manifest file(s): the edit failed\n", n)
	}
}

// editDep runs the kernel edit-dep op: version "" removes the requirement,
// "latest" selects the newest eligible version (refusal: exit 5), anything
// else pins it. It completes and writes the lock, returning the edits and
// the saved document. On any failure after the kernel may have edited a
// manifest, the manifests are restored byte-for-byte.
func (o *opts) editDep(ctx context.Context, root *workspace.Root, module, coord, version, alias string, shared, sharedOnly bool) (edits []manifestEdit, lock *lockfile.Document, err error) {
	args := map[string]any{
		"module":      module,
		"coord":       coord,
		"shared":      shared,
		"shared-only": sharedOnly,
		"offline":     o.offline,
		"force":       o.force,
		"requirement": nil,
	}
	if version != "" {
		args["requirement"] = version
	}
	if alias != "" {
		args["alias"] = alias
	}
	store, err := o.store()
	if err != nil {
		return nil, nil, err
	}
	jar, err := kernel.Current.Ensure(ctx, store, o.offline)
	if err != nil {
		return nil, nil, err
	}
	java, _, err := o.pickJava(ctx, store, root)
	if err != nil {
		return nil, nil, err
	}
	env, err := o.kernelEnv(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	penv, err := o.kernelProxy(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	defer o.closeProxy()
	snap, err := snapshotManifests(root)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			revertManifests(snap)
		}
	}()
	out, err := kernel.Call(ctx, jar, java, kernel.Request{
		Op:        "edit-dep",
		Workspace: root.Dir,
		Lock:      "deps.lock",
		Args:      args,
	}, append(env, penv...)...)
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return nil, nil, exitf(1, "edit: %v", oe)
		}
		return nil, nil, err
	}
	var parsed struct {
		Lock    *lockfile.Document `json:"lock"`
		Refused []map[string]any   `json:"refused"`
		Edits   []manifestEdit     `json:"edits"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, nil, fmt.Errorf("edit: bad kernel response: %w", err)
	}
	if len(parsed.Refused) > 0 {
		return nil, nil, refusedErr(parsed.Refused)
	}
	if err := completeLock(ctx, o.fetchClient(ctx, root), store, o.m2Root(), parsed.Lock); err != nil {
		return nil, nil, err
	}
	if err := parsed.Lock.Save(root.LockPath()); err != nil {
		return nil, nil, err
	}
	return parsed.Edits, parsed.Lock, nil
}

// editAndPrint runs editDep and prints the manifest edits, the skip
// decisions and the lock summary.
func (o *opts) editAndPrint(ctx context.Context, root *workspace.Root, module, coord, version, alias string, shared, sharedOnly bool) error {
	edits, lock, err := o.editDep(ctx, root, module, coord, version, alias, shared, sharedOnly)
	if err != nil {
		return err
	}
	printEdits(edits)
	printSkipped(lock)
	fmt.Printf("wrote %s: %d artifacts, %d modules\n",
		root.LockPath(), len(lock.Artifacts), len(lock.Modules))
	return nil
}

func printEdits(edits []manifestEdit) {
	for _, e := range edits {
		if e.Requirement == nil {
			fmt.Printf("%s: remove %s\n", e.File, e.Coord)
		} else {
			fmt.Printf("%s: set %s %q\n", e.File, e.Coord, *e.Requirement)
		}
	}
}

// printSkipped reports the lock's cooldown/force/explicit decisions (design §9).
func printSkipped(lock *lockfile.Document) {
	for _, s := range lock.Skipped {
		switch s.Reason {
		case "explicit":
			fmt.Printf("pinned %s %s (explicit)\n", s.Coord, s.Version)
		case "forced":
			fmt.Printf("forced %s %s (cooldown %s)\n", s.Coord, s.Version, s.Cooldown)
		default: // "cooldown"
			if s.PublishedAt != nil {
				if pin := pinOf(lock, s.Coord); pin != "" {
					fmt.Printf("skipped %s %s (published %s ago, cooldown %s); using %s\n",
						s.Coord, s.Version, age(*s.PublishedAt), s.Cooldown, pin)
					continue
				}
			}
			fmt.Printf("skipped %s %s (cooldown %s)\n", s.Coord, s.Version, s.Cooldown)
		}
	}
}

// pinOf is the version of coord in the lock's artifacts, "" if not locked.
func pinOf(lock *lockfile.Document, coord string) string {
	for i := range lock.Artifacts {
		a := &lock.Artifacts[i]
		if a.Kind == "mvn" && a.Group+"/"+a.Name == coord {
			return a.Version
		}
	}
	return ""
}

// age renders how long ago t was, in hours under 48h, else days.
func age(t time.Time) string {
	h := int(time.Since(t).Hours())
	if h < 48 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dd", h/24)
}
