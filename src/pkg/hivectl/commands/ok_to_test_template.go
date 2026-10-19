package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const okToTestDefaultCommand = "/ok-to-test"

const okToTestCallerTemplate = `name: Hive ok-to-test

on:
  issue_comment:
    types: [created]

permissions:
  actions: write
  contents: read
  issues: write
  pull-requests: read

jobs:
  ok-to-test:
    uses: hivecommons/hive/.github/workflows/hive-ok-to-test.yml@%s
    with:
      hive-app-logins: %s%s%s
`

func newOkToTestTemplateCommand(_ *commandEnv) *cobra.Command {
	var hiveAppLogins string
	var trustedAuthors string
	ref := "v5"
	command := okToTestDefaultCommand
	cmd := &cobra.Command{
		Use:   "ok-to-test-template",
		Short: "Emit the GitHub Actions caller workflow for Hive /ok-to-test approvals",
		Long: "Print a .github/workflows/hive-ok-to-test.yml caller that lets a repository GITHUB_TOKEN approve " +
			"trusted same-repo bot workflow runs after a Hive App posts /ok-to-test. The Hive App itself needs no Actions: write permission.",
		Example: "  hivectl ok-to-test-template --hive-app-login hivecommons-hive[bot] > .github/workflows/hive-ok-to-test.yml",
		Args:    argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			hiveAppLogins = strings.TrimSpace(hiveAppLogins)
			if hiveAppLogins == "" {
				return &usageError{message: "--hive-app-login is required (for example hivecommons-hive[bot])"}
			}
			trusted := ""
			if strings.TrimSpace(trustedAuthors) != "" {
				trusted = "\n      trusted-authors: " + strings.TrimSpace(trustedAuthors)
			}
			cmdLine := ""
			if strings.TrimSpace(command) != "" && strings.TrimSpace(command) != okToTestDefaultCommand {
				cmdLine = "\n      command: " + strings.TrimSpace(command)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), okToTestCallerTemplate, strings.TrimSpace(ref), hiveAppLogins, trusted, cmdLine)
			return err
		},
	}
	cmd.Flags().StringVar(&hiveAppLogins, "hive-app-login", "", "comma-separated Hive App bot login(s) allowed to issue /ok-to-test")
	cmd.Flags().StringVar(&trustedAuthors, "trusted-authors", "", "optional comma-separated trusted PR authors; reusable workflow defaults to Dependabot/Renovate plus Hive App login(s)")
	cmd.Flags().StringVar(&ref, "ref", "v5", "hivecommons/hive ref for the reusable workflow")
	cmd.Flags().StringVar(&command, "command", okToTestDefaultCommand, "slash command Hive posts")
	return cmd
}
