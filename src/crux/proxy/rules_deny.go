package proxy

import (
	"regexp"
)

// Hard-deny rules: operations refused for EVERY agent mode because they must
// route through a hive-mediated relay. Split out of rules.go so the
// mode-escalation table stays within its size ratchet.

// denyRule is a hard block that applies to EVERY agent mode, regardless of the
// mode-escalation rules. It exists for operations agents must route through a
// hive-mediated path instead of calling GitHub directly.
type denyRule struct {
	PathPattern *regexp.Regexp
	Method      string
	Msg         string // agent-facing directive surfaced in the 403 body
}

// denyRules are checked BEFORE the mode rules. Two cases are hard-denied for
// EVERY agent mode:
//
//  1. Direct PR creation: a POST /repos/*/pulls — whether from `gh pr create`
//     or the GitHub MCP create_pull_request/create_pull_request_with_copilot
//     tool — authors the PR as the Copilot login user, not the App bot.
//     Blocking it here forces agents to use `hive-open-pr`, which the hive
//     fulfills with the App token so the PR is authored by the App bot. This
//     closes the MCP path the gh-wrapper redirect cannot see.
//
//  2. Direct PR merge: a PUT /repos/*/pulls/{n}/merge (H1, CWE-863). Permitting
//     this at ModeIssuesPRsMerge on a UID-derived mode check ALONE let an
//     injected merge-mode agent `gh api -X PUT .../merge` any reachable PR,
//     bypassing the bound merge relay's fail-closed SHA pin + merge-eligible
//     binding (see bindMergeAuthz). Hard-denying it forces ALL agent-initiated
//     merges through `hive-merge`, which the hive fulfills as the App over REST
//     with those bindings enforced. This mirrors the GraphQL mergePullRequest
//     deny (GraphQLAllowed treats it as a mutation), so neither REST nor GraphQL
//     is an agent-reachable merge bypass.
//
// In BOTH cases the hive's OWN call (CreatePR / MergePR) does NOT traverse this
// agent proxy — it originates from the hive process (owner UID), which the
// forced-egress iptables redirect exempts — so the relay is unaffected.
var denyRules = []denyRule{
	// ── GitHub ──
	{
		PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`),
		Method:      "POST",
		Msg:         "direct PR creation is disabled for agents — use `hive-open-pr --repo <owner/repo> --head <branch> --title <t> --body <b>` so the hive opens the PR as the App bot (never the login user). Do NOT use `gh pr create` or the GitHub MCP create_pull_request/create_pull_request_with_copilot.",
	},
	{
		PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/merge$`),
		Method:      "PUT",
		Msg:         "direct PR merge is disabled for agents — use `hive-merge --repo <owner/repo> --number <n> --expect-sha <head-sha>` so the hive merges as the App bot with the SHA-pin + merge-eligible binding enforced. Do NOT use `gh api -X PUT .../merge`, `gh pr merge`, or the GitHub MCP merge_pull_request.",
	},

	// ── GitLab ──
	{
		PathPattern: regexp.MustCompile(`^(?:/api/v4)?/projects/.+?/merge_requests$`),
		Method:      "POST",
		Msg:         "direct MR creation is disabled for agents — use the request-file path so the hive opens the MR as the App/bot identity. Do NOT use direct GitLab API calls.",
	},
	{
		PathPattern: regexp.MustCompile(`^(?:/api/v4)?/projects/.+?/merge_requests/\d+/merge$`),
		Method:      "PUT",
		Msg:         "direct MR merge is disabled for agents — use the request-file path so the hive merges as the App/bot identity with bindings enforced. Do NOT use direct GitLab API calls.",
	},
	{
		PathPattern: regexp.MustCompile(`^(?:/api/v4)?/projects/.+?/merge_requests/\d+/merge$`),
		Method:      "POST",
		Msg:         "direct MR merge is disabled for agents — use the request-file path so the hive merges as the App/bot identity with bindings enforced. Do NOT use direct GitLab API calls.",
	},

	// ── Gitea / Forgejo ──
	{
		PathPattern: regexp.MustCompile(`^/api/v1/repos/[^/]+/[^/]+/pulls$`),
		Method:      "POST",
		Msg:         "direct PR creation is disabled for agents — use the request-file path so the hive opens the PR as the App/bot identity. Do NOT use direct Gitea API calls.",
	},
	{
		PathPattern: regexp.MustCompile(`^(?:/api/v1)?/repos/[^/]+/[^/]+/pulls/\d+/merge$`),
		Method:      "POST",
		Msg:         "direct PR merge is disabled for agents — use the request-file path so the hive merges as the App/bot identity with bindings enforced. Do NOT use direct Gitea API calls.",
	},
	{
		PathPattern: regexp.MustCompile(`^/api/v1/repos/[^/]+/[^/]+/pulls/\d+/merge$`),
		Method:      "PUT",
		Msg:         "direct PR merge is disabled for agents — use the request-file path so the hive merges as the App/bot identity with bindings enforced. Do NOT use direct Gitea API calls.",
	},
}

// DeniedMessage returns the agent-facing directive for a request blocked by a
// hard-deny rule, and true if such a rule matched. It lets the proxy surface WHY
// a request was refused (e.g. "use hive-open-pr") instead of the generic ACMM
// message. Returns ("", false) when no deny rule matches — the caller then falls
// back to the normal mode-based block reason.
func DeniedMessage(method, path string) (string, bool) {
	for _, r := range denyRules {
		if r.Method == method && r.PathPattern.MatchString(path) {
			return r.Msg, true
		}
	}
	return "", false
}
