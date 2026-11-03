// Package oktotest renders the repository-side GitHub Actions workflow that
// approves held (`action_required`) workflow runs on trusted same-repository
// bot PRs after a Hive App comments `/ok-to-test`.
//
// The approval logic lives once, in approve.sh. Two workflows are rendered
// from it:
//
//   - Workflow renders a fully self-contained `issue_comment` workflow for any
//     governed repository. It carries no `uses:` reference to hivecommons/hive,
//     so it works for repositories whose Actions policy only allows selected
//     reusable workflows, on GHES, and in organizations that do not permit
//     workflows from hivecommons.
//   - ReusableWorkflow renders the `workflow_call` variant hivecommons/hive
//     itself keeps under .github/workflows for its own caller.
//
// Tests assert that the committed copies under .github/workflows and
// src/docs/templates match these renderings.
package oktotest

import (
	_ "embed"
	"strings"
)

//go:embed approve.sh
var approveScript string

const (
	// PlaceholderAppLogin marks where the installing hive's App bot login goes
	// in the committed template.
	PlaceholderAppLogin = "HIVE_APP_LOGIN[bot]"
	// DefaultTrustedAuthors is the PR-author allow list when none is given.
	// The Hive App login(s) are always added by the script.
	DefaultTrustedAuthors = "dependabot[bot],renovate[bot]"
	// DefaultCommand is the slash command Hive posts on a held PR head.
	DefaultCommand = "/ok-to-test"
	// WorkflowPath is where governed repositories install the workflow.
	WorkflowPath = ".github/workflows/hive-ok-to-test.yml"

	scriptIndent = "          "
	// approveEndpointFragment is the REST path fragment the script hits to
	// release a held run; tests and docs anchor on it.
	approveEndpointFragment = "/actions/runs/"
)

// Options configure a rendered workflow.
type Options struct {
	// AppLogins is the comma-separated Hive App bot login(s) allowed to issue
	// the command. Empty renders PlaceholderAppLogin.
	AppLogins string
	// TrustedAuthors overrides DefaultTrustedAuthors when non-empty.
	TrustedAuthors string
	// Command overrides DefaultCommand when non-empty.
	Command string
}

func (o Options) appLogins() string {
	if v := strings.TrimSpace(o.AppLogins); v != "" {
		return v
	}
	return PlaceholderAppLogin
}

func (o Options) trustedAuthors() string {
	if v := strings.TrimSpace(o.TrustedAuthors); v != "" {
		return v
	}
	return DefaultTrustedAuthors
}

func (o Options) command() string {
	if v := strings.TrimSpace(o.Command); v != "" {
		return v
	}
	return DefaultCommand
}

// Script returns the shared bash approval logic.
func Script() string { return approveScript }

// ApproveEndpointFragment returns the REST path fragment used to approve runs.
func ApproveEndpointFragment() string { return approveEndpointFragment }

func indentedScript() string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(approveScript, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(scriptIndent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Workflow renders the self-contained repository workflow.
func Workflow(o Options) string {
	var b strings.Builder
	b.WriteString(`name: Hive ok-to-test

# Self-contained: no reusable-workflow reference, so it runs in repositories
# whose Actions policy only allows selected workflows and on GHES.
# The repository GITHUB_TOKEN (actions: write) approves held runs; the Hive App
# itself needs no Actions write permission.

on:
  issue_comment:
    types: [created]

jobs:
  ok-to-test:
    name: Approve trusted bot workflow runs
    if: ${{ github.event.issue.pull_request }}
    runs-on: ubuntu-latest
    permissions:
      actions: write
      contents: read
      issues: write
      pull-requests: read
    steps:
      - name: Approve held trusted same-repo workflow runs
        env:
          GH_TOKEN: ${{ github.token }}
          # Comma-separated Hive App bot login(s) allowed to issue the command.
          HIVE_APP_LOGINS: ` + yamlScalar(o.appLogins()) + `
          # Comma-separated PR authors whose same-repo held runs may be approved
          # (the Hive App login(s) above are always trusted too).
          TRUSTED_AUTHORS: ` + yamlScalar(o.trustedAuthors()) + `
          COMMAND: ` + yamlScalar(o.command()) + `
        run: |
`)
	b.WriteString(indentedScript())
	return b.String()
}

// ReusableWorkflow renders the workflow_call variant kept in hivecommons/hive.
func ReusableWorkflow() string {
	var b strings.Builder
	b.WriteString(`name: Hive ok-to-test reusable

# Generated from src/pkg/oktotest (approve.sh); edit there, not here.
# Third-party repositories should use the self-contained template from
# 'hivectl ok-to-test-template' instead of calling this workflow.

on:
  workflow_call:
    inputs:
      hive-app-logins:
        description: Comma-separated Hive App bot logins allowed to issue the command.
        required: true
        type: string
      trusted-authors:
        description: Comma-separated PR author logins whose same-repo held runs may be approved.
        required: false
        default: ` + DefaultTrustedAuthors + `
        type: string
      command:
        description: Slash command Hive posts to release trusted same-repo held workflow runs.
        required: false
        default: ` + DefaultCommand + `
        type: string

jobs:
  ok-to-test:
    name: Approve trusted bot workflow runs
    runs-on: ubuntu-latest
    permissions:
      actions: write
      contents: read
      issues: write
      pull-requests: read
    steps:
      - name: Approve held trusted same-repo workflow runs
        env:
          GH_TOKEN: ${{ github.token }}
          HIVE_APP_LOGINS: ${{ inputs.hive-app-logins }}
          TRUSTED_AUTHORS: ${{ inputs.trusted-authors }}
          COMMAND: ${{ inputs.command }}
        run: |
`)
	b.WriteString(indentedScript())
	return b.String()
}

// yamlScalar quotes a value so logins such as `name[bot]` and commands such as
// `/ok-to-test` survive YAML parsing verbatim.
func yamlScalar(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}
