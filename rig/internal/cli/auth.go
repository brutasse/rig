package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brutasse/rig/internal/oidc"
)

// newAuthCmd is the OIDC gate token command: "rig auth get [gate|url]"
// resolves one gate's bearer (the gate's RIG_TOKEN_<GATE> environment
// variable, else the gate's cached token, else a fresh negotiation) and
// prints it to stdout. The gate is a gate name from the config, or a
// repository URL the gate fronts.
func newAuthCmd(o *opts) *cobra.Command {
	var flow string
	get := &cobra.Command{
		Use:   "get [gate|url]",
		Short: "Print the bearer token of an OIDC gate",
		Long: "Resolve the bearer token of an OIDC gate and print it to stdout.\n\n" +
			"The gate comes from the gate config (~/.config/rig/auth.yaml): pass a\n" +
			"gate name or a repository URL the gate fronts, or nothing when the\n" +
			"config holds a single gate.\n\n" +
			"The token is the gate's RIG_TOKEN_<GATE> environment variable when set,\n" +
			"else the gate's cached token, else a fresh negotiation with the gate's\n" +
			"issuer (browser, or device code when headless), verified against the\n" +
			"issuer's JWKS before use.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := oidc.Load()
			if err != nil {
				return err
			}
			var gate *oidc.Gate
			switch {
			case len(args) == 0:
				if len(cfg.Gates) == 0 {
					return fmt.Errorf("no gate in %s (create it; see docs)", cfg.Path)
				}
				if len(cfg.Gates) > 1 {
					return fmt.Errorf("the config holds %d gates (%s); pass a gate name or repository URL", len(cfg.Gates), strings.Join(cfg.Names(), ", "))
				}
				gate = cfg.Gates[cfg.Names()[0]]
			case cfg.Gates[args[0]] != nil:
				gate = cfg.Gates[args[0]]
			case strings.Contains(args[0], "://"):
				gate, err = cfg.Gate(args[0])
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("gate %q not in %s (gates: %s)", args[0], cfg.Path, strings.Join(cfg.Names(), ", "))
			}
			store, err := o.store()
			if err != nil {
				return err
			}
			tok, err := oidc.NewResolver(cfg, store.Root).Token(cmd.Context(), gate, flow)
			if err != nil {
				return fmt.Errorf("gate %q: %v", gate.Name, err)
			}
			fmt.Println(tok)
			return nil
		},
	}
	get.Flags().StringVar(&flow, "flow", "",
		`negotiation flow when the token is not cached: "browser", "device", or empty for auto`)
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "OIDC gate tokens",
	}
	cmd.AddCommand(get)
	return cmd
}
