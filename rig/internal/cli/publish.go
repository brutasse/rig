package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/lockfile"
	"github.com/brutasse/rig/internal/maven"
	"github.com/brutasse/rig/internal/workspace"
)

func newInstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the target module's jar into the local Maven repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstall(cmd.Context(), o)
		},
	}
}

func newPublishCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "publish",
		Short: "Deploy the target module's jar to its remote repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(cmd.Context(), o)
		},
	}
}

func runInstall(ctx context.Context, o *opts) error {
	e, err := o.hot(ctx, true)
	if err != nil {
		return err
	}
	targets, err := e.installTargets(o.path)
	if err != nil {
		return err
	}
	return e.publishModules(ctx, targets, false)
}

func runPublish(ctx context.Context, o *opts) error {
	if o.offline {
		return exitf(2, "publish: --offline is not supported (deploying requires the network)")
	}
	e, err := o.hot(ctx, true)
	if err != nil {
		return err
	}
	targets, err := e.publishTargets(o.path)
	if err != nil {
		return err
	}
	return e.publishModules(ctx, targets, true)
}

// installTargets returns the modules to install: the -p module, or every
// locked module with a :rig/lib coordinate.
func (e *hotEnv) installTargets(p string) ([]string, error) {
	if p != "" && p != "." {
		m, err := e.root.Resolve(p)
		if err != nil {
			return nil, err
		}
		mod, err := e.module(m)
		if err != nil {
			return nil, err
		}
		if mod.Lib == "" {
			return nil, exitf(2, "module %s has no :rig/lib coordinate", m)
		}
		return []string{m}, nil
	}
	var out []string
	for _, m := range e.root.Modules() {
		mod, err := e.lock.Module(m)
		if err != nil {
			continue
		}
		if mod.Lib != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, exitf(2, "no module in the lock has a :rig/lib coordinate")
	}
	return out, nil
}

// publishTargets returns the modules to deploy: the -p module (which must be
// publishable), or every locked module with :rig/publish? enabled.
func (e *hotEnv) publishTargets(p string) ([]string, error) {
	if p != "" && p != "." {
		m, err := e.root.Resolve(p)
		if err != nil {
			return nil, err
		}
		mod, err := e.module(m)
		if err != nil {
			return nil, err
		}
		if !mod.Publish.Enabled {
			return nil, exitf(2, "module %s is not publishable (missing :rig/publish?)", m)
		}
		return []string{m}, nil
	}
	out := publishableOf(e.root, e.lock)
	if len(out) == 0 {
		return nil, exitf(2, "no publishable module in the lock (:rig/publish?)")
	}
	return out, nil
}

// publishableOf returns the publishable modules of the lock, in module order.
func publishableOf(root *workspace.Root, lock *lockfile.Document) []string {
	var out []string
	for _, m := range root.Modules() {
		mod, err := lock.Module(m)
		if err != nil {
			continue
		}
		if mod.Publish.Enabled {
			out = append(out, m)
		}
	}
	return out
}

// publishModules builds each module's jar and installs (remote=false) or
// deploys (remote=true) it, printing one line per module.
func (e *hotEnv) publishModules(ctx context.Context, ms []string, remote bool) error {
	for _, m := range ms {
		if err := e.publishOne(ctx, m, remote); err != nil {
			return err
		}
	}
	return nil
}

type publishEntry struct {
	Coord   string `json:"coord"`
	Version string `json:"version"`
	Repo    string `json:"repo"`
	URL     string `json:"url"`
	Pom     string `json:"pom"`
}

func (e *hotEnv) publishOne(ctx context.Context, m string, remote bool) error {
	jar, err := e.buildOne(ctx, m, false, false)
	if err != nil {
		return err
	}
	resp, err := kernel.Call(ctx, e.kernel, e.java, kernel.Request{
		Op:        "publish",
		Workspace: e.root.Dir,
		Modules:   []string{m},
		Lock:      filepath.Base(e.root.LockPath()),
		Args:      map[string]any{"module": m},
	})
	if err != nil {
		var oe *kernel.OpError
		if errors.As(err, &oe) {
			return exitf(1, "publish: %v", oe)
		}
		return err
	}
	var parsed struct {
		Published []publishEntry `json:"published"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return fmt.Errorf("publish: bad kernel response: %w", err)
	}
	if len(parsed.Published) == 0 {
		return errors.New("publish: kernel returned no published entry")
	}
	plan := parsed.Published[0]

	pomPath := strings.TrimSuffix(jar, filepath.Ext(jar)) + ".pom"
	if err := os.WriteFile(pomPath, []byte(plan.Pom), 0o644); err != nil {
		return err
	}
	base := mavenPath(plan.Coord, plan.Version)

	if remote {
		if !strings.HasPrefix(plan.URL, "http://") && !strings.HasPrefix(plan.URL, "https://") {
			return exitf(2, "publish: unsupported repository URL %s (only http/https repositories are supported)", plan.URL)
		}
		user, pass := maven.Credentials(plan.Repo)
		bearer := ""
		if e.oidc != nil {
			bearer, _ = e.oidc(plan.Repo)
		}
		url := strings.TrimSuffix(plan.URL, "/") + base
		for _, f := range []string{jar, pomPath} {
			if err := putFile(ctx, url+"/"+filepath.Base(f), user, pass, bearer, f); err != nil {
				return err
			}
		}
		fmt.Printf("published %s %s -> %s\n", plan.Coord, plan.Version, url+"/"+filepath.Base(jar))
		return nil
	}

	dir := filepath.Join(e.m2root, base)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []string{jar, pomPath} {
		if err := copyFile(f, filepath.Join(dir, filepath.Base(f))); err != nil {
			return err
		}
	}
	fmt.Printf("installed %s %s -> %s\n", plan.Coord, plan.Version, filepath.Join(dir, filepath.Base(jar)))
	return nil
}

// mavenPath is the Maven layout path of a coord and version:
// /group-as-dir-path/artifact/version.
func mavenPath(coord, version string) string {
	i := strings.Index(coord, "/")
	return "/" + strings.ReplaceAll(coord[:i], ".", "/") + "/" + coord[i+1:] + "/" + version
}

// putFile uploads file to url with an HTTP PUT. Authorization: a bearer
// token when given, else basic auth when user != "", else none.
func putFile(ctx context.Context, url, user, pass, bearer, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	switch {
	case bearer != "":
		req.Header.Set("Authorization", "Bearer "+bearer)
	case user != "":
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("publish: PUT %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return exitf(1, "publish: PUT %s -> %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
