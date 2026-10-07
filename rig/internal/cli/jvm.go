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

// pickJava picks the JVM for this workspace: the RIG_JAVA override, then —
// when the lock pins a JVM — the rig-managed JDK matching the pin, which
// takes precedence over the system java even when its feature version
// matches (installed on demand when online); a matching system JDK
// (JAVA_HOME, then PATH) serves only when no managed JDK is installed.
// Without a pin, the system java is used. It returns the java binary and
// the extra env entries for launched processes (JAVA_HOME when a managed
// JDK is in use).
func (o *opts) pickJava(ctx context.Context, store *cache.Store, root *workspace.Root) (string, []string, error) {
	if p := os.Getenv("RIG_JAVA"); p != "" {
		return p, nil, nil
	}
	system, serr := jvm.Find()
	if root != nil && root.Lock != nil && root.Lock.JVM != nil {
		pin := root.Lock.JVM
		st := jdk.NewStoreAt(store.Root)
		if inst, err := st.Best(pin.Requested); err == nil {
			return inst.JavaPath, []string{"JAVA_HOME=" + inst.Home}, nil
		}
		if serr == nil {
			if v, verr := jvm.Version(system); verr == nil && pinSatisfied(v, pin.Requested) {
				return system, nil, nil
			}
		}
		inst, err := jdk.Ensure(ctx, st, pin.Requested, o.offline)
		if err != nil {
			return "", nil, err
		}
		return inst.JavaPath, []string{"JAVA_HOME=" + inst.Home}, nil
	}
	if serr != nil {
		return "", nil, o.noJavaHint(ctx, serr)
	}
	return system, nil, nil
}

// pinSatisfied reports whether a JVM of version v satisfies requested, the
// workspace's :rig/jvm pin: an exact match on feature version, the same
// rule launch enforces (patch releases are irrelevant).
func pinSatisfied(v, requested string) bool {
	got := jdk.FeatureVersion(v)
	want := jdk.FeatureVersion(requested)
	return got != 0 && got == want
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
		newJVMPathCmd(o),
		newJVMUninstallCmd(o),
		newJVMUpdateCmd(o),
	)
	return c
}

func newJVMInstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "install <major>",
		Short: "Install a Temurin JDK into the rig state dir",
		Long: `Installs the newest Temurin JDK (Eclipse Adoptium) for a major version into
the rig state dir, verifying the download against the sha256 published by
the Adoptium API. One JDK per major version: when a matching major is
already installed, this exits without changing anything — use
'rig jvm update' to move it to the newest release.

<major> is a major (feature) version: "21".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJVMInstall(cmd.Context(), o, args[0])
		},
	}
}

func runJVMInstall(ctx context.Context, o *opts, requested string) error {
	if !jdk.ValidRequested(requested) {
		return exitf(2, "bad version %q (want a major version, e.g. \"21\")", requested)
	}
	if o.offline {
		return exitf(1, "offline: cannot install a JDK (run 'rig jvm install %s' online)", requested)
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	st := jdk.NewStoreAt(store.Root)
	if inst, err := st.Best(requested); err == nil {
		fmt.Printf("already installed: %s %s\n", inst.Vendor, inst.Version)
		return nil
	}
	a, err := jdk.NewAPI("").Resolve(ctx, requested)
	if err != nil {
		return err
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

func newJVMPathCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "path <major>",
		Short: "Print the JAVA_HOME of an installed JDK",
		Long: `Prints the home directory (the JAVA_HOME Rig exports for launched processes)
of the newest installed Temurin JDK satisfying <major> — a bare path on
stdout, for scripts and CI. The store is read only: nothing is installed or
downloaded, and the command needs no network. When no installed JDK
satisfies <major>, this exits 2 with the install hint.

<major> is a major (feature) version: "21".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJVMPath(o, args[0])
		},
	}
}

func runJVMPath(o *opts, requested string) error {
	if !jdk.ValidRequested(requested) {
		return exitf(2, "bad version %q (want a major version, e.g. \"21\")", requested)
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	inst, err := jdk.NewStoreAt(store.Root).Best(requested)
	if err != nil {
		return exitf(2, "temurin %s not installed (run 'rig jvm install %s')", requested, requested)
	}
	fmt.Println(inst.Home)
	return nil
}

func newJVMUninstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <version>",
		Short: "Remove an installed JDK (major version or unique prefix)",
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
		Short: "Update the rig-managed JDK for the pinned major to the newest release",
		Long: `Updates the rig-managed Temurin JDK for the workspace's :rig/jvm pin to the
newest release for that major version, replacing the installed one (rig
keeps one JDK per major version). When no JDK for the major is installed,
the newest release is installed. The manifest and the lock are untouched.`,
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
	if o.offline {
		return exitf(1, "offline: cannot update a JDK (run 'rig jvm update' online)")
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	st := jdk.NewStoreAt(store.Root)
	old, _ := st.Best(pin.Requested)
	a, err := jdk.NewAPI("").Resolve(ctx, pin.Requested)
	if err != nil {
		return err
	}
	if old != nil && old.Version == a.Version {
		fmt.Printf("jvm up to date: %s\n", old.Version)
		return nil
	}
	if old != nil {
		fmt.Printf("updating temurin %s → %s (%s, %d MB)…\n", old.Version, a.Version, a.OS+"/"+a.Arch, a.Size/1024/1024)
	} else {
		fmt.Printf("installing %s %s (%s, %d MB)…\n", a.Vendor, a.Version, a.OS+"/"+a.Arch, a.Size/1024/1024)
	}
	inst, err := st.Install(ctx, a)
	if err != nil {
		return err
	}
	// One JDK per major: drop any other install matching the pin.
	insts, err := st.List()
	if err != nil {
		return err
	}
	for _, i := range insts {
		if i.Version != inst.Version && jdk.Satisfies(pin.Requested, i.Version) {
			if _, err := st.Uninstall(i.Version); err != nil {
				return err
			}
		}
	}
	if old != nil {
		fmt.Printf("jvm updated: %s → %s\n", old.Version, inst.Version)
	} else {
		fmt.Printf("installed %s %s to %s\n", inst.Vendor, inst.Version, inst.Dir)
	}
	return nil
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
		inst, lerr := st.Best(pin.Requested)
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
