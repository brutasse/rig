package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/graal"
	"github.com/brutasse/rig/internal/jdk"
	"github.com/brutasse/rig/internal/workspace"
)

func newGraalVMCmd(o *opts) *cobra.Command {
	c := &cobra.Command{
		Use:   "graalvm",
		Short: "Manage rig-managed GraalVMs",
	}
	c.AddCommand(
		newGraalVMInstallCmd(o),
		newGraalVMListCmd(o),
		newGraalVMUninstallCmd(o),
		newGraalVMUpdateCmd(o),
	)
	return c
}

func newGraalVMInstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "install <version>",
		Short: "Install a GraalVM community JDK into the rig state dir",
		Long: `Installs a GraalVM community JDK into the rig state dir, verifying the
download against the sha256 sidecar published on the graalvm/graalvm-ce-builds
GitHub release.

<version> is a feature version ("21" — the newest 21.x build) or an exact
version ("21.0.2").`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGraalVMInstall(cmd.Context(), o, args[0])
		},
	}
}

func runGraalVMInstall(ctx context.Context, o *opts, requested string) error {
	if !jdk.ValidRequested(requested) {
		return exitf(2, "bad version %q (want e.g. \"21\" or \"21.0.2\")", requested)
	}
	if o.offline {
		return exitf(1, "offline: cannot install a GraalVM (run 'rig graalvm install %s' online)", requested)
	}
	store, err := o.store()
	if err != nil {
		return err
	}
	st := graal.NewStoreAt(store.Root)
	a, err := graal.NewAPI("").Resolve(ctx, requested)
	if err != nil {
		return err
	}
	if inst, err := st.Lookup(a.Version); err == nil {
		fmt.Printf("already installed: %s %s\n", inst.Vendor, inst.Version)
		return nil
	}
	size := ""
	if a.Size > 0 {
		size = fmt.Sprintf(", %d MB", a.Size/1024/1024)
	}
	fmt.Printf("installing %s %s (%s%s)…\n", a.Vendor, a.Version, a.OS+"/"+a.Arch, size)
	inst, err := st.Install(ctx, a)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s %s to %s\n", inst.Vendor, inst.Version, inst.Home)
	return nil
}

func newGraalVMListCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed GraalVMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGraalVMList(o)
		},
	}
}

func runGraalVMList(o *opts) error {
	store, err := o.store()
	if err != nil {
		return err
	}
	insts, err := graal.NewStoreAt(store.Root).List()
	if err != nil {
		return err
	}
	fmt.Println("installed:")
	if len(insts) == 0 {
		fmt.Println("  (none)")
	}
	for _, i := range insts {
		fmt.Printf("  %s %-14s %s/%s  %s\n", i.Vendor, i.Version, i.OS, i.Arch, i.Home)
	}
	return nil
}

func newGraalVMUninstallCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <version>",
		Short: "Remove an installed GraalVM (exact version or unique prefix)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGraalVMUninstall(o, args[0])
		},
	}
}

func runGraalVMUninstall(o *opts, requested string) error {
	store, err := o.store()
	if err != nil {
		return err
	}
	v, err := graal.NewStoreAt(store.Root).Uninstall(requested)
	if err != nil {
		return err
	}
	fmt.Printf("uninstalled %s %s\n", graal.Vendor, v)
	return nil
}

func newGraalVMUpdateCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Bump the locked GraalVM to the newest build satisfying the manifest pin",
		Long: `Updates the exact GraalVM version recorded in deps.lock to the newest
community build satisfying the workspace's :rig/jvm pin. The manifest is
untouched; a fresh machine following the new lock installs that exact version.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGraalVMUpdate(cmd.Context(), o)
		},
	}
}

func runGraalVMUpdate(ctx context.Context, o *opts) error {
	root, err := workspace.Find(".")
	if err != nil {
		return err
	}
	if root.Lock == nil {
		return exitf(3, "no lock at %s (run 'rig lock')", root.LockPath())
	}
	pin := root.Lock.GraalVM
	if pin == nil {
		return exitf(2, "no GraalVM in the lock (the workspace needs a :rig/jvm pin and a :rig/native? module — run 'rig lock')")
	}
	if o.frozen {
		return exitf(3, "--frozen: the lock is read-only")
	}
	a, err := graal.NewAPI("").Resolve(ctx, pin.Requested)
	if err != nil {
		return err
	}
	if a.Version == pin.Version {
		fmt.Printf("graalvm up to date: %s\n", pin.Version)
		return nil
	}
	old := pin.Version
	pin.Version = a.Version
	if err := root.Lock.Save(root.LockPath()); err != nil {
		return err
	}
	fmt.Printf("graalvm pin updated: %s → %s\n", displayOrNone(old), a.Version)
	return nil
}
