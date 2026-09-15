package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/classpath"
	"github.com/brutasse/rig/internal/fetch"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/workspace"
)

func newLockCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Resolve the workspace and write deps.lock",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := workspace.Find(".")
			if err != nil {
				return err
			}
			lock, err := o.relock(ctx, root)
			if err != nil {
				return err
			}
			printSkipped(lock)
			fmt.Printf("wrote %s: %d artifacts, %d modules\n",
				root.LockPath(), len(lock.Artifacts), len(lock.Modules))
			return nil
		},
	}
}

// relock resolves the workspace with the kernel (respecting the pins already
// in deps.lock when present), fetches and hashes every artifact, and writes
// the new lock. It returns the saved document.
func (o *opts) relock(ctx context.Context, root *workspace.Root) (*lockfile.Document, error) {
	return o.relockArgs(ctx, root, nil)
}

// relockArgs is relock with extra kernel args (e.g. respect-existing-pins).
func (o *opts) relockArgs(ctx context.Context, root *workspace.Root, extra map[string]any) (*lockfile.Document, error) {
	store, err := o.store()
	if err != nil {
		return nil, err
	}
	jar, err := kernel.Current.Ensure(ctx, store, o.offline)
	if err != nil {
		return nil, err
	}
	java, _, err := o.pickJava(ctx, store, root)
	if err != nil {
		return nil, err
	}
	args := map[string]any{"offline": o.offline, "force": o.force}
	for k, v := range extra {
		args[k] = v
	}
	req := kernel.Request{
		Op:        "resolve",
		Workspace: root.Dir,
		Lock:      "deps.lock",
		Args:      args,
	}
	env, err := o.kernelEnv(ctx, root)
	if err != nil {
		return nil, err
	}
	penv, err := o.kernelProxy(ctx, root)
	if err != nil {
		return nil, err
	}
	defer o.closeProxy()
	out, err := kernel.Call(ctx, jar, java, req, append(env, penv...)...)
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return nil, exitf(1, "lock: %v", oe)
		}
		return nil, err
	}
	lock, err := lockFromResponse(ctx, o.fetchClient(ctx, root), store, o.m2Root(), out)
	if err != nil {
		return nil, err
	}
	if err := lock.Save(root.LockPath()); err != nil {
		return nil, err
	}
	return lock, nil
}

// lockFromResponse parses a kernel resolve response, refuses cooldown-blocked
// requirements (exit 5), and returns the completed document.
func lockFromResponse(ctx context.Context, client *fetch.Client, store *cache.Store, m2root string, resp []byte) (*lockfile.Document, error) {
	var parsed struct {
		Lock    *lockfile.Document `json:"lock"`
		Refused []map[string]any   `json:"refused"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil, fmt.Errorf("lock: bad kernel response: %w", err)
	}
	lock := parsed.Lock
	if lock == nil {
		return nil, errors.New("lock: kernel response has no lock")
	}
	if len(parsed.Refused) > 0 {
		return nil, refusedErr(parsed.Refused)
	}
	return lock, completeLock(ctx, client, store, m2root, lock)
}

// completeLock fills tool/locked_at, the exact jvm version, and every mvn
// artifact's sha256 (fetching each), then validates.
func completeLock(ctx context.Context, client *fetch.Client, store *cache.Store, m2root string, lock *lockfile.Document) error {
	lock.Tool = lockfile.Tool{Name: "rig", Version: Version}
	lock.LockedAt = time.Now().UTC().Truncate(time.Second)

	if j := lock.JVM; j != nil {
		if j.Vendor == "" {
			j.Vendor = jdk.Vendor
		}
		if j.Version == "" && !client.Offline {
			a, err := jdk.NewAPI("").Resolve(ctx, j.Requested)
			if err != nil {
				return fmt.Errorf("lock: jvm: %w", err)
			}
			j.Version = a.Version
		}
	}

	var items []fetch.Item
	idx := make(map[string]int)
	for i := range lock.Artifacts {
		a := &lock.Artifacts[i]
		if a.Kind == "mvn" && a.URL != "" {
			idx[a.URL] = i
			items = append(items, fetch.Item{URL: a.URL, Repo: a.Repository, Local: classpath.M2Path(m2root, *a)})
		}
	}
	shas, err := fetch.FetchNewAll(ctx, client, store, items, 2)
	if err != nil {
		if errors.Is(err, fetch.ErrOffline) {
			return exitf(1, "offline: %v", err)
		}
		return err
	}
	for url, sha := range shas {
		lock.Artifacts[idx[url]].SHA256 = sha
	}
	if err := lock.Validate(); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	return nil
}

// refusedErr prints the cooldown-refused requirements to stderr and returns
// exit 5 (design §9: retry with --force).
func refusedErr(refused []map[string]any) error {
	for _, r := range refused {
		fmt.Fprintf(os.Stderr, "refused %v (cooldown %v)\n", r["coord"], r["cooldown"])
	}
	return exitf(5, "%d requirement(s) refused by cooldowns (retry with --force)", len(refused))
}
