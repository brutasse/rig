package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/workspace"
)

// autocompleteSentinel is the NoOptDefVal of --autocomplete, so that the
// flag works bare ("rig --autocomplete") as well as
// ("rig --autocomplete=zsh"); it maps to "detect the shell".
const autocompleteSentinel = "detect"

// autocompleteShells lists the shells --autocomplete supports, in help order.
var autocompleteShells = []string{"bash", "zsh", "fish", "powershell"}

// printAutocomplete writes the completion script for shell to stdout.
// An empty shell is detected from the environment.
func printAutocomplete(cmd *cobra.Command, shell string) error {
	if shell == "" {
		if shell = detectShell(); shell == "" {
			return exitf(2, "could not detect the shell (valid: %s)", strings.Join(autocompleteShells, ", "))
		}
	}
	switch shell {
	case "bash":
		return cmd.GenBashCompletionV2(os.Stdout, true)
	case "zsh":
		return cmd.GenZshCompletion(os.Stdout)
	case "fish":
		return cmd.GenFishCompletion(os.Stdout, true)
	case "powershell":
		return cmd.GenPowerShellCompletionWithDesc(os.Stdout)
	}
	return exitf(2, "unknown shell %q (valid: %s)", shell, strings.Join(autocompleteShells, ", "))
}

// detectShell identifies the running shell from its environment.
func detectShell() string {
	switch {
	case os.Getenv("ZSH_VERSION") != "":
		return "zsh"
	case os.Getenv("FISH_VERSION") != "":
		return "fish"
	case os.Getenv("BASH_VERSION") != "":
		return "bash"
	case os.Getenv("PSModulePath") != "":
		return "powershell"
	}
	base := strings.ToLower(filepath.Base(os.Getenv("SHELL")))
	switch base {
	case "bash", "zsh", "fish":
		return base
	case "pwsh", "powershell":
		return "powershell"
	}
	return ""
}

// moduleCompletions suggests workspace module paths for -p/--path.
func moduleCompletions(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	root, err := workspace.Find(".")
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	mods := root.Modules()
	sort.Strings(mods)
	return mods, cobra.ShellCompDirectiveNoFileComp
}

// aliasCompletions suggests the target module's locked aliases for --alias.
func aliasCompletions(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	path := ""
	if f := cmd.Flag("path"); f != nil {
		path = f.Value.String()
	}
	root, err := workspace.Find(".")
	if err != nil || root.Lock == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	m, err := root.Resolve(path)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	mod, err := root.Lock.Module(m)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for name := range mod.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}
