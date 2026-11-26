package commands

import (
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// newGitHubCommand groups the GitHub credential commands. Today that is PAT
// rotation (hivecommons/hive#19633): the scripted twin of the dashboard's
// Settings -> Forge App field, over the owner-only /api/github-token routes.
func newGitHubCommand(env *commandEnv) *cobra.Command {
	github := &cobra.Command{
		Use:   "github",
		Short: "Manage the hive's GitHub credential",
	}
	token := &cobra.Command{
		Use:   "token",
		Short: "Show or rotate the GitHub personal access token (owner only)",
	}
	token.AddCommand(&cobra.Command{
		Use:     "status",
		Short:   "Show the token's fingerprint, login and last verification (never the token)",
		Args:    argsNone(),
		Example: "  hivectl github token status --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.do(cmd, http.MethodGet, "/api/github-token", nil)
		},
	})
	token.AddCommand(githubTokenSetCommand(env))
	github.AddCommand(token)
	return github
}

// githubTokenSetCommand reads the new token from stdin or a file, never from
// argv (where it would show in the process list and shell history). The
// server verifies it before saving; a refused token leaves the current one
// in place and exits non-zero.
func githubTokenSetCommand(env *commandEnv) *cobra.Command {
	var file string
	var stdin bool
	command := &cobra.Command{
		Use:   "set",
		Short: "Rotate the GitHub personal access token without a restart",
		Args:  argsNone(),
		Example: "  pass show hive/github-pat | hivectl github token set --stdin\n" +
			"  hivectl github token set --file /run/secrets/github_pat",
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := readInput(env.in, file, stdin)
			if err != nil {
				return err
			}
			token := strings.TrimSpace(string(data))
			if token == "" {
				return &usageError{message: "the token input was empty"}
			}
			if strings.ContainsAny(token, " \t\r\n") {
				return &usageError{message: "the token input must be a single token with no whitespace inside"}
			}
			return env.do(cmd, http.MethodPut, "/api/github-token", map[string]string{"token": token})
		},
	}
	command.Flags().StringVarP(&file, "file", "f", "", "file containing the new token")
	command.Flags().BoolVar(&stdin, "stdin", false, "read the new token from stdin")
	return command
}
