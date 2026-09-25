package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/cache"
	"github.com/brutasse/rig/internal/fetch"
	"github.com/brutasse/rig/internal/maven"
	"github.com/brutasse/rig/internal/workspace"
)

type opts struct {
	path         string
	offline      bool
	frozen       bool
	force        bool
	cacheDir     string
	verbose      bool
	autocomplete string
	oidcA        *oidcAuth // lazily resolved per command (see oidc.go)
}

func (o *opts) store() (*cache.Store, error) {
	if o.cacheDir != "" {
		return cache.NewAt(o.cacheDir), nil
	}
	return cache.New()
}

// fetchClient builds the artifact fetch client: basic auth from
// ~/.m2/settings.xml per repository id, and a bearer token for the
// workspace's :auth :oidc repositories (see oidc.go).
func (o *opts) fetchClient(ctx context.Context, root *workspace.Root) *fetch.Client {
	c := fetch.New(o.offline)
	if p := maven.DefaultUserSettings(); p != "" {
		if st, err := maven.Load(p); err == nil {
			c.CredFor = func(repo string) (string, string, bool) {
				if s, ok := st.Servers[repo]; ok {
					return s.Username, s.Password, true
				}
				return "", "", false
			}
		}
	}
	c.BearerFor = o.bearerFor(ctx, root)
	return c
}

// m2Root returns the local Maven repository: <localRepository> from
// ~/.m2/settings.xml when set, else ~/.m2/repository.
func (o *opts) m2Root() string {
	if p := maven.DefaultUserSettings(); p != "" {
		if st, err := maven.Load(p); err == nil && st.LocalRepo != "" {
			return st.LocalRepo
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".m2", "repository")
}

func Execute() int {
	o := &opts{}
	root := &cobra.Command{
		Use:           "rig",
		Short:         "Build and run Clojure projects, the obvious way",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.autocomplete == "" {
				return cmd.Help()
			}
			shell := o.autocomplete
			if shell == autocompleteSentinel && len(args) > 0 {
				// pflag refuses to consume a bare "--autocomplete bash":
				// the shell arrives as a trailing positional instead.
				shell, args = args[0], args[1:]
			}
			if len(args) > 0 {
				return exitf(2, "unknown command %q for \"rig\"", args[0])
			}
			if shell == autocompleteSentinel {
				shell = "" // detect
			}
			return printAutocomplete(cmd, shell)
		},
	}
	// --autocomplete is the only completion interface; hide the stock
	// "completion" subcommand cobra would otherwise add.
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return exitf(2, "%v", err)
	})
	pf := root.PersistentFlags()
	pf.StringVarP(&o.path, "path", "p", "", "target module")
	pf.BoolVar(&o.offline, "offline", false, "never use the network")
	pf.BoolVar(&o.frozen, "frozen", false, "never modify the lock; fail if it is stale")
	pf.BoolVar(&o.force, "force", false, "bypass cooldowns")
	pf.StringVar(&o.cacheDir, "cache-dir", "", "state dir (default ~/.local/share/rig)")
	pf.BoolVarP(&o.verbose, "verbose", "v", false, "debug logging")
	root.Flags().StringVar(&o.autocomplete, "autocomplete", "",
		"print a completion script (bash, zsh, fish, powershell; default: detect)")
	// Optional value: bare --autocomplete detects the shell.
	root.Flags().Lookup("autocomplete").NoOptDefVal = autocompleteSentinel
	root.AddCommand(
		newAuthCmd(o),
		newVerifyCmd(o),
		newCleanCmd(o),
		newVersionCmd(o),
		newInfoCmd(o),
		newLockCmd(o),
		newMigrateCmd(o),
		newUpdateCmd(o),
		newAddCmd(o),
		newRemoveCmd(o),
		newCheckCmd(o),
		newTestCmd(o),
		newRunCmd(o),
		newReplCmd(o),
		newLaunchCmd(o),
		newBuildCmd(o),
		newInstallCmd(o),
		newPublishCmd(o),
		newReleaseCmd(o),
		newOutdatedCmd(o),
		newTreeCmd(o),
		newLintCmd(o),
		newFmtCmd(o),
		newExecCmd(o),
		newNewCmd(o),
		newNewModuleCmd(o),
		newSelfUpdateCmd(o),
		newJVMCmd(o),
	)
	// Dynamic completion: -p/--path offers the workspace's module paths,
	// --alias offers the target module's locked aliases (lockfile reads only).
	root.RegisterFlagCompletionFunc("path", moduleCompletions)
	root.RegisterFlagCompletionFunc("autocomplete", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return autocompleteShells, cobra.ShellCompDirectiveNoFileComp
	})
	for _, c := range root.Commands() {
		if c.Flags().Lookup("alias") != nil {
			c.RegisterFlagCompletionFunc("alias", aliasCompletions)
		}
	}
	err := root.Execute()
	code := 0
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, ee.msg)
			}
			code = ee.code
		} else {
			fmt.Fprintln(os.Stderr, "rig:", err)
			code = 1
		}
	}
	if notice := o.updateNotice(); notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
	return code
}

func targetModules(root *workspace.Root, p string) ([]string, error) {
	if p == "" || p == "." {
		return root.Modules(), nil
	}
	m, err := root.Resolve(p)
	if err != nil {
		return nil, err
	}
	return []string{m}, nil
}
