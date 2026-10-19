package commands

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/oktotest"
	"github.com/spf13/cobra"
)

// configAppBotLoginKey is the /api/config field carrying the hive's App bot
// login ("<slug>[bot]"), used when --app-login is not given.
const configAppBotLoginKey = "app_bot_login"

func newOkToTestTemplateCommand(env *commandEnv) *cobra.Command {
	var appLogins string
	var trustedAuthors string
	var command string
	cmd := &cobra.Command{
		Use:   "ok-to-test-template",
		Short: "Emit the self-contained GitHub Actions workflow for Hive /ok-to-test approvals",
		Long: "Print a self-contained " + oktotest.WorkflowPath + " that lets a repository GITHUB_TOKEN approve " +
			"trusted same-repo bot workflow runs after a Hive App posts " + oktotest.DefaultCommand + ". " +
			"The workflow references no reusable workflow, so it works under restrictive Actions policies and on GHES. " +
			"The Hive App itself needs no Actions: write permission. " +
			"When --app-login is omitted the login is read from the hive's /api/config.",
		Example: "  hivectl ok-to-test-template --app-login hivecommons-hive[bot] > " + oktotest.WorkflowPath + "\n" +
			"  hivectl --server http://127.0.0.1:3001 ok-to-test-template > " + oktotest.WorkflowPath,
		Args: argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			appLogins = strings.TrimSpace(appLogins)
			if appLogins == "" {
				login, err := env.configuredAppBotLogin(cmd.Context())
				if err != nil {
					return err
				}
				appLogins = login
			}
			if appLogins == "" {
				return &usageError{message: "--app-login is required when the hive has no GitHub App configured (for example --app-login hivecommons-hive[bot])"}
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), oktotest.Workflow(oktotest.Options{
				AppLogins:      appLogins,
				TrustedAuthors: trustedAuthors,
				Command:        command,
			}))
			return err
		},
	}
	cmd.Flags().StringVar(&appLogins, "app-login", "", "comma-separated Hive App bot login(s) allowed to issue the command; defaults to the hive's configured App login")
	cmd.Flags().StringVar(&appLogins, "hive-app-login", "", "alias for --app-login")
	_ = cmd.Flags().MarkDeprecated("hive-app-login", "use --app-login")
	cmd.Flags().StringVar(&trustedAuthors, "trusted-authors", "", "comma-separated trusted PR authors (default "+oktotest.DefaultTrustedAuthors+"; the App login(s) are always trusted)")
	cmd.Flags().StringVar(&command, "command", oktotest.DefaultCommand, "slash command Hive posts")
	return cmd
}

// configuredAppBotLogin asks the dashboard for the hive's App bot login.
func (e *commandEnv) configuredAppBotLogin(ctx context.Context) (string, error) {
	client, err := e.client()
	if err != nil {
		return "", err
	}
	result, err := client.Do(ctx, http.MethodGet, "/api/config", nil, nil)
	if err != nil {
		return "", fmt.Errorf("--app-login not given and the hive config could not be read: %w", err)
	}
	payload, _ := result.(map[string]any)
	login, _ := payload[configAppBotLoginKey].(string)
	return strings.TrimSpace(login), nil
}
