package commands

import (
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

func newContributeCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "contribute", Short: "Manage contributor registration tokens"}
	login := &cobra.Command{
		Use: "login [profile]", Short: "Rotate and save a token using your GitHub identity (default: active profile)", Args: argsMax(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			} else {
				deps, err := env.hivesDeps()
				if err != nil {
					return err
				}
				set, err := loadProfiles(cmd, deps, false)
				if err != nil {
					return err
				}
				name = set.Active
				if name == "" {
					return &usageError{message: "specify a saved hive profile: hivectl contribute login <profile>"}
				}
			}
			return env.runHivesReissue(cmd, name, &hivesReissueOptions{})
		},
	}
	tokens := &cobra.Command{Use: "tokens", Short: "Operator token age and revocation controls (requires hive write permissions)"}
	tokens.AddCommand(&cobra.Command{Use: "list", Short: "List contributor token issuance, expiry and rotation warnings", Args: argsNone(), RunE: func(cmd *cobra.Command, args []string) error {
		return env.do(cmd, http.MethodGet, "/api/contributors", nil)
	}})
	var all, yes bool
	revoke := &cobra.Command{Use: "revoke [contributor]", Short: "Revoke one token, or every token with --all --yes", Args: argsMax(1), RunE: func(cmd *cobra.Command, args []string) error {
		if all {
			if len(args) != 0 || !yes {
				return &usageError{message: "bulk revocation requires --all --yes and no contributor argument"}
			}
			return env.do(cmd, http.MethodPost, "/api/contributors/tokens/revoke-all", nil)
		}
		if len(args) != 1 {
			return &usageError{message: "specify a contributor or --all --yes"}
		}
		return env.do(cmd, http.MethodPost, "/api/contributors/"+url.PathEscape(args[0])+"/token/revoke", nil)
	}}
	revoke.Flags().BoolVar(&all, "all", false, "revoke all registration tokens (incident response)")
	revoke.Flags().BoolVar(&yes, "yes", false, "confirm bulk revocation")
	tokens.AddCommand(revoke)
	cmd.AddCommand(login, tokens)
	return cmd
}
