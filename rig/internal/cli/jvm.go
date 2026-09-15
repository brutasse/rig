package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/jvm"
	"github.com/brutasse/rig/internal/workspace"
)

// pickJava picks the JVM for this workspace: the RIG_JAVA override, then the
// lock's pinned managed JDK (installed on demand when online), then
// JAVA_HOME, then PATH. It returns the java binary and the extra env entries
// for launched processes (JAVA_HOME when a managed JDK is in use).
func (o *opts) pickJava(ctx context.Context, store *cache.Store, root *workspace.Root) (string, []string, error) {
	if p := os.Getenv("RIG_JAVA"); p != "" {
		return p, nil, nil
	}
	if root != nil && root.Lock != nil && root.Lock.JVM != nil {
		pin := root.Lock.JVM
		inst, err := jdk.Ensure(ctx, jdk.NewStoreAt(store.Root), pin.Requested, pin.Version, o.offline)
		if err != nil {
			return "", nil, err
		}
		return inst.JavaPath, []string{"JAVA_HOME=" + inst.Home}, nil
	}
	java, err := jvm.Find()
	if err != nil {
		return "", nil, o.noJavaHint(ctx, err)
	}
	return java, nil, nil
}

// noJavaHint appends an install suggestion to the "no java found" error.
// Online it names the current LTS feature version — a best-effort,
// time-bounded lookup; the hint never installs anything.
func (o *opts) noJavaHint(ctx context.Context, err error) error {
	hint := "install one with: rig jvm install <version> (e.g. \"21\" — Temurin, hash-verified, rig state dir)"
	if !o.offline {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if lts, lerr := jdk.NewAPI("").LatestLTS(lctx); lerr == nil {
			hint = fmt.Sprintf("install the current LTS with: rig jvm install %d (Temurin, hash-verified, rig state dir)", lts)
		}
		cancel()
	}
	return fmt.Errorf("%v — %s", err, hint)
}

func newJVMCmd(o *opts) *cobra.Command {
	c := &cobra.Command{
		Use:   "jvm",
		Short: "Manage rig-managed JDKs",
	}
	c.AddCommand(
		newJVMInstallCmd(o),
		newJVMListCmd(o),
		newJVMUninstallCmd(o),
		newJVMUpdateCmd(o),
	)
	return c
}

func newJVMInstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "install <version>",
		Short: "Install a Temurin JDK into the rig state dir",
		Long: `Installs a Temurin JDK (Eclipse Adoptium) into the rig state dir,
verifying the download against the sha256 published by the Adoptium API.

<version> is a feature version ("21" — the newest 21.x release) or an exact
version ("21.0.10", "21.0.10+7").`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJVMInstall(cmd.Context(), o, args[0])
		},
	}
}

func runJVMInstall(ctx context.Context, o *opts, requested string) error {
	if !jdk.ValidRequested(requested) {
		return exitf(2, "bad version %q (want e.g. \"21\" or \"21.0.10+7\")", requested)
	}
	if o.offline {
		return exitf(1, "offline: cannot install a JDK (run 'rig jvm install %s' online)", requested)
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	st := jdk.NewStoreAt(store.Root)
	a, err := jdk.NewAPI("").Resolve(ctx, requested)
	if err != nil {
		return err
	}
	if inst, err := st.Lookup(a.Version); err == nil {
		fmt.Printf("already installed: %s %s\n", inst.Vendor, inst.Version)
		return nil
	}
	fmt.Printf("installing %s %s (%s, %d MB)…\n", a.Vendor, a.Version, a.OS+"/"+a.Arch, a.Size/1024/1024)
	inst, err := st.Install(ctx, a)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s %s to %s\n", inst.Vendor, inst.Version, inst.Dir)
	return nil
}

func newJVMListCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed JDKs and the system java",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJVMList(o)
		},
	}
}

func runJVMList(o *opts) error {
	store, err := o.store()
	if err != nil {
		return err
	}
	insts, err := jdk.NewStoreAt(store.Root).List()
	if err != nil {
		return err
	}
	fmt.Println("installed:")
	if len(insts) == 0 {
		fmt.Println("  (none)")
	}
	for _, i := range insts {
		fmt.Printf("  %s %-14s %s/%s  %s\n", i.Vendor, i.Version, i.OS, i.Arch, i.Dir)
	}
	system := map[string]string{}
	if home := os.Getenv("JAVA_HOME"); home != "" {
		if p := filepath.Join(home, "bin", "java"); fileExists(p) {
			system[p] = "JAVA_HOME"
		}
	}
	if p, err := exec.LookPath("java"); err == nil {
		if _, ok := system[p]; !ok {
			system[p] = "PATH"
		}
	}
	if len(system) > 0 {
		fmt.Println("system:")
		for p, via := range system {
			v, err := jvm.Version(p)
			if err != nil {
				v = "?"
			}
			fmt.Printf("  %s  %s (%s)\n", v, p, via)
		}
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func newJVMUninstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <version>",
		Short: "Remove an installed JDK (exact version or unique prefix)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJVMUninstall(o, args[0])
		},
	}
}

func runJVMUninstall(o *opts, requested string) error {
	store, err := o.store()
	if err != nil {
		return err
	}
	v, err := jdk.NewStoreAt(store.Root).Uninstall(requested)
	if err != nil {
		return err
	}
	fmt.Printf("uninstalled %s %s\n", jdk.Vendor, v)
	return nil
}

func newJVMUpdateCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Bump the locked JVM to the newest release satisfying the manifest pin",
		Long: `Updates the exact JVM version recorded in deps.lock to the newest release
satisfying the workspace's :rig/jvm pin. The manifest is untouched; a fresh
machine following the new lock installs that exact version.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJVMUpdate(cmd.Context(), o)
		},
	}
}

func runJVMUpdate(ctx context.Context, o *opts) error {
	root, err := workspace.Find(".")
	if err != nil {
		return err
	}
	if root.Lock == nil {
		return exitf(3, "no lock at %s (run 'rig lock')", root.LockPath())
	}
	pin := root.Lock.JVM
	if pin == nil {
		return exitf(2, "no :rig/jvm pin in the lock (add it to the root deps.edn and re-lock)")
	}
	if o.frozen {
		return exitf(3, "--frozen: the lock is read-only")
	}
	a, err := jdk.NewAPI("").Resolve(ctx, pin.Requested)
	if err != nil {
		return err
	}
	if a.Version == pin.Version {
		fmt.Printf("jvm up to date: %s\n", pin.Version)
		return nil
	}
	old := pin.Version
	pin.Version = a.Version
	if err := root.Lock.Save(root.LockPath()); err != nil {
		return err
	}
	fmt.Printf("jvm pin updated: %s → %s\n", displayOrNone(old), a.Version)
	return nil
}

func displayOrNone(s string) string {
	if s == "" {
		return "(unresolved)"
	}
	return s
}

// javaInfo returns the `rig info` java lines: the pinned managed JDK when the
// lock pins one (installed or not), the RIG_JAVA override, else the system
// java.
func (o *opts) javaInfo(root *workspace.Root) (string, error) {
	if p := os.Getenv("RIG_JAVA"); p != "" {
		return fmt.Sprintf("java:\t%s (RIG_JAVA)\n", p), nil
	}
	if root != nil && root.Lock != nil && root.Lock.JVM != nil {
		pin := root.Lock.JVM
		store, err := o.store()
		if err != nil {
			return "", err
		}
		st := jdk.NewStoreAt(store.Root)
		var (
			inst *jdk.Inst
			lerr error
		)
		if pin.Version != "" {
			inst, lerr = st.Lookup(pin.Version)
		} else {
			inst, lerr = st.Best(pin.Requested)
		}
		if lerr == nil {
			return fmt.Sprintf("java:\t%s (temurin %s, pinned %q)\n", inst.JavaPath, inst.Version, pin.Requested), nil
		}
		return fmt.Sprintf("java:\tpinned %q — temurin not installed (run 'rig jvm install %s')\n",
			pin.Requested, strings.TrimSpace(pin.Requested)), nil
	}
	java, err := jvm.Find()
	if err != nil {
		return fmt.Sprintf("java:\tnot found (%v)\n", err), nil
	}
	if v, verr := jvm.Version(java); verr == nil {
		return fmt.Sprintf("java:\t%s (%s)\n", java, v), nil
	}
	return fmt.Sprintf("java:\t%s\n", java), nil
}
