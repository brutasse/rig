package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/kernel"
	"github.com/brutasse/rig/internal/kernelrun"
)

// The checkpoint (CRaC) JVM: kernel.Call restores eligible ops from a
// warmed kernel checkpoint instead of forking a cold JVM; these commands
// manage the JDK that runs the checkpoint. The selection order lives in
// kernelrun.jdkFor: RIG_CRAC_JDK, managed install, cold JVM.

func newCRaCCmd(o *opts) *cobra.Command {
	c := &cobra.Command{
		Use:   "crac",
		Short: "Manage the checkpoint (CRaC) JVM",
		Long: `rig can run kernel commands by restoring a checkpoint of a warmed kernel
instead of booting a fresh JVM (~0.3 s instead of 1-3 s). Checkpointing is
always opportunistic: without a CRaC-capable JVM every command simply takes
the cold path.

The checkpoint JVM is an explicit RIG_CRAC_JDK when set, else a rig-managed
Zulu CRaC JDK installed with 'rig crac install', else the JVM rig picked
for the command itself.`,
	}
	c.AddCommand(
		newCRACInstallCmd(o),
		newCRACStatusCmd(o),
		newCRACCleanCmd(o),
	)
	return c
}

func newCRACInstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the rig-pinned Zulu CRaC JDK",
		Long: `Installs the exact Zulu CRaC JDK build pinned by this rig release into the
rig state dir, verifying the download against the pinned sha256. The kernel
picks it up automatically for checkpointing; uninstall with
'rm -rf <state dir>/jdks/zulu-crac-*'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCRACInstall(cmd.Context(), o)
		},
	}
}

func runCRACInstall(ctx context.Context, o *opts) error {
	if o.offline {
		return exitf(1, "offline: cannot install the CRaC JDK (run 'rig crac install' online)")
	}
	a, ok := jdk.PinnedCRaC()
	if !ok {
		return exitf(1, "no CRaC JDK pinned for %s/%s (checkpointing supports linux x64 and arm64)", runtime.GOOS, runtime.GOARCH)
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	st := jdk.NewStoreAt(store.Root)
	if inst, err := jdk.CRaCInstalled(st); err == nil {
		fmt.Printf("already installed: %s %s\n", inst.Vendor, inst.Version)
		return nil
	}
	fmt.Printf("installing %s %s (%s/%s, %d MB)…\n", a.Vendor, a.Version, a.OS, a.Arch, a.Size/1024/1024)
	inst, err := jdk.EnsureCRaC(ctx, st)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s %s to %s\nthe kernel will checkpoint with it automatically.\n", inst.Vendor, inst.Version, inst.Dir)
	return nil
}

func newCRACStatusCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the pinned and installed CRaC JDK",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCRACStatus(o)
		},
	}
}

func runCRACStatus(o *opts) error {
	a, ok := jdk.PinnedCRaC()
	if !ok {
		fmt.Printf("no CRaC JDK pinned for %s/%s — kernel commands run cold\n", runtime.GOOS, runtime.GOARCH)
		return nil
	}
	fmt.Printf("pinned: %s %s (%s/%s)\n", a.Vendor, a.Version, a.OS, a.Arch)
	store, err := o.store()
	if err != nil {
		return err
	}
	if inst, err := jdk.CRaCInstalled(jdk.NewStoreAt(store.Root)); err == nil {
		fmt.Printf("state:  installed %s\n", inst.Dir)
	} else {
		fmt.Printf("state:  not installed (rig crac install)\n")
	}
	if p := os.Getenv("RIG_CRAC_JDK"); p != "" {
		fmt.Printf("active: RIG_CRAC_JDK=%s overrides the managed install\n", p)
	}
	return nil
}

func newCRACCleanCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Prune checkpoint chains that can no longer be restored",
		Long: `Checkpoint images are keyed to the exact kernel jar bytes, JVM build and
CPU features: after a kernel or JDK rotation the old chains are dead
weight (a chain is a set of parent-referencing images, only deletable as
a whole). This prunes every chain the current JVM selection cannot
restore, plus interrupted-run leftovers. Chains rebuild themselves:
pruning one that was still live costs one cold command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCRACClean(cmd.Context(), o)
		},
	}
}

func runCRACClean(ctx context.Context, o *opts) error {
	if os.Getenv("RIG_KERNEL_JAR") != "" {
		return exitf(1, "RIG_KERNEL_JAR dev override is active: the current jar says nothing about release chains; refusing to prune")
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	// Offline resolution: pruning is housekeeping and must never download
	// the kernel jar.
	jar, err := kernel.Current.Ensure(ctx, store, true)
	if err != nil {
		return exitf(1, "cannot resolve the kernel jar (%v); nothing pruned", err)
	}
	n, freed, err := kernelrun.Clean(jar)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("no stale checkpoint chains")
		return nil
	}
	fmt.Printf("pruned %d stale item(s), %.1f MB freed\n", n, float64(freed)/1024/1024)
	return nil
}

// checkpointSweep is the boot-time housekeeping: once a day, on a machine
// with a checkpoint store, prune the chains chainMeta records as bootstrapped
// from a kernel jar other than the current pinned one — the staleness a rig
// update always leaves behind. No probe, no network, no fork: silent and
// milliseconds. Anything beyond jar staleness stays with 'rig crac clean'.
// Housekeeping is never a failure mode; verbose surfaces what it did.
func checkpointSweep(o *opts) {
	if os.Getenv("RIG_KERNEL_JAR") != "" {
		return // a dev jar must not classify release chains as dead
	}
	if !kernelrun.SweepDue() {
		return
	}
	store, err := cache.New() // the store the kernel itself uses, not --cache-dir
	if err != nil {
		return
	}
	jar, err := kernel.Current.Ensure(context.Background(), store, true)
	if err != nil {
		return // no idea what the current jar is: nothing is stale
	}
	n, freed, err := kernelrun.SweepDaily(jar)
	switch {
	case err != nil && o.verbose:
		fmt.Fprintf(os.Stderr, "checkpoint sweep: %v\n", err)
	case n > 0 && o.verbose:
		fmt.Fprintf(os.Stderr, "checkpoint sweep: pruned %d old-kernel chain(s), %.1f MB freed\n", n, float64(freed)/1024/1024)
	}
}
