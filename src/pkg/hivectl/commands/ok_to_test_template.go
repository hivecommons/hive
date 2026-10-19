package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const okToTestDefaultCommand = "/ok-to-test"
const okToTestDefaultTrustedAuthors = "dependabot[bot],renovate[bot]"

const okToTestWorkflowTemplate = `name: Hive ok-to-test

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
    name: Approve trusted bot workflow runs
    runs-on: ubuntu-latest
    env:
      HIVE_APP_LOGINS: "{{HIVE_APP_LOGINS}}"
      TRUSTED_AUTHORS: "{{TRUSTED_AUTHORS}}"
      COMMAND: "{{COMMAND}}"
    steps:
      - name: Approve held trusted same-repo workflow runs
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          set -euo pipefail

          repo="$GITHUB_REPOSITORY"
          event="$GITHUB_EVENT_PATH"
          issue_number="$(jq -r '.issue.number // empty' "$event")"
          is_pr="$(jq -r 'has("issue") and (.issue.pull_request != null)' "$event")"
          body="$(jq -r '.comment.body // ""' "$event" | tr -d '\r')"
          commenter="$(jq -r '.comment.user.login // ""' "$event")"
          comment_id="$(jq -r '.comment.id // empty' "$event")"

          if [ "$is_pr" != "true" ] || [ -z "$issue_number" ]; then
            echo "ok-to-test: comment is not on a pull request; ignoring"
            exit 0
          fi
          if ! printf '%s\n' "$body" | grep -Fxq -- "$COMMAND"; then
            echo "ok-to-test: command not present; ignoring"
            exit 0
          fi

          contains_login() {
            local list="$1" login="$2" item
            IFS=',' read -ra items <<< "$list"
            for item in "${items[@]}"; do
              item="$(echo "$item" | xargs)"
              if [ -n "$item" ] && [ "${item,,}" = "${login,,}" ]; then
                return 0
              fi
            done
            return 1
          }

          allowed_commenter=false
          if contains_login "$HIVE_APP_LOGINS" "$commenter"; then
            allowed_commenter=true
          else
            permission="$(gh api "repos/${repo}/collaborators/${commenter}/permission" --jq '.permission' 2>/dev/null || true)"
            case "$permission" in
              admin|maintain|write) allowed_commenter=true ;;
            esac
          fi
          if [ "$allowed_commenter" != "true" ]; then
            echo "::warning::ok-to-test: @${commenter} is not an allowed Hive App login and lacks write/maintain/admin"
            exit 0
          fi

          pr_json="$(gh api "repos/${repo}/pulls/${issue_number}")"
          head_repo="$(jq -r '.head.repo.full_name // ""' <<< "$pr_json")"
          base_repo="$(jq -r '.base.repo.full_name // ""' <<< "$pr_json")"
          head_sha="$(jq -r '.head.sha // ""' <<< "$pr_json")"
          author="$(jq -r '.user.login // ""' <<< "$pr_json")"
          if [ -z "$head_sha" ] || [ "$head_repo" != "$base_repo" ] || [ "$head_repo" != "$repo" ]; then
            echo "ok-to-test: PR is not a same-repository head; ignoring"
            exit 0
          fi
          trusted="${TRUSTED_AUTHORS},${HIVE_APP_LOGINS}"
          if ! contains_login "$trusted" "$author"; then
            echo "ok-to-test: PR author @${author} is not in trusted-authors; ignoring"
            exit 0
          fi
          if gh api "repos/${repo}/issues/${issue_number}" --jq '.labels[].name' | grep -Fxiq 'sentinel-alert'; then
            echo "ok-to-test: sentinel-alert is present; refusing to approve runs"
            exit 0
          fi

          mapfile -t run_ids < <(gh api --paginate "repos/${repo}/actions/runs?head_sha=${head_sha}&status=action_required&per_page=100" \
            --jq '.workflow_runs[] | select(.head_repository.full_name == "'"${repo}"'") | .id')
          if [ "${#run_ids[@]}" -eq 0 ]; then
            echo "ok-to-test: no action_required workflow runs for ${head_sha}"
            if [ -n "$comment_id" ]; then
              gh api -X POST "repos/${repo}/issues/comments/${comment_id}/reactions" -f content='+1' >/dev/null || true
            fi
            exit 0
          fi

          approved=0
          for run_id in "${run_ids[@]}"; do
            echo "Approving workflow run ${run_id} for ${head_sha}"
            gh api -X POST "repos/${repo}/actions/runs/${run_id}/approve" >/dev/null
            approved=$((approved + 1))
          done
          gh api -X POST "repos/${repo}/issues/${issue_number}/comments" \
            -f body="Hive ok-to-test approved ${approved} held workflow run(s) for trusted same-repo PR head ${head_sha}." >/dev/null
`

func renderOkToTestWorkflow(hiveAppLogins, trustedAuthors, command string) string {
	hiveAppLogins = strings.TrimSpace(hiveAppLogins)
	trustedAuthors = strings.TrimSpace(trustedAuthors)
	if trustedAuthors == "" {
		trustedAuthors = okToTestDefaultTrustedAuthors
		if hiveAppLogins != "" {
			trustedAuthors += "," + hiveAppLogins
		}
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = okToTestDefaultCommand
	}
	out := strings.NewReplacer(
		"{{HIVE_APP_LOGINS}}", hiveAppLogins,
		"{{TRUSTED_AUTHORS}}", trustedAuthors,
		"{{COMMAND}}", command,
	).Replace(okToTestWorkflowTemplate)
	return out
}

func newOkToTestTemplateCommand(_ *commandEnv) *cobra.Command {
	var hiveAppLogins string
	var trustedAuthors string
	command := okToTestDefaultCommand
	cmd := &cobra.Command{
		Use:   "ok-to-test-template",
		Short: "Emit the self-contained GitHub Actions workflow for Hive /ok-to-test approvals",
		Long: "Print a self-contained .github/workflows/hive-ok-to-test.yml workflow that lets a repository GITHUB_TOKEN approve " +
			"trusted same-repo bot workflow runs after this Hive App posts /ok-to-test. The Hive App itself needs no Actions: write permission and no reusable workflow dependency is required.",
		Example: "  hivectl ok-to-test-template --hive-app-login my-hive[bot] > .github/workflows/hive-ok-to-test.yml",
		Args:    argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			hiveAppLogins = strings.TrimSpace(hiveAppLogins)
			if hiveAppLogins == "" {
				return &usageError{message: "--hive-app-login is required (for example my-hive[bot])"}
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), renderOkToTestWorkflow(hiveAppLogins, trustedAuthors, command))
			return err
		},
	}
	cmd.Flags().StringVar(&hiveAppLogins, "hive-app-login", "", "comma-separated Hive App bot login(s) allowed to issue /ok-to-test")
	cmd.Flags().StringVar(&trustedAuthors, "trusted-authors", "", "optional comma-separated trusted PR authors; defaults to Dependabot/Renovate plus Hive App login(s)")
	cmd.Flags().StringVar(&command, "command", okToTestDefaultCommand, "slash command Hive posts")
	return cmd
}
