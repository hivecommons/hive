package scheduler

import (
	"fmt"
	"strings"
)

// Work-tracker guidance for non-GitHub work sources.
//
// Every shipped policy template is written against GitHub Issues: `gh issue
// create`, `Fixes #N`, the `hold` label. When the hive's work source is Linear
// the items in ${ISSUE_LIST} read `owner/repo!TEAM-123` and none of those
// GitHub recipes apply to the tracker half of the job — only to the PR half,
// which still lives on GitHub. Rather than fork 36 templates into Linear
// variants, the scheduler renders ONE tracker section from the existing
// work_source.linear config (teams → repos, hold labels, states) and injects
// it at the same post-resolution seam as the held-PR preflight, so config
// templates, repo-sourced prompts, embedded defaults, and the hardcoded
// fallbacks all carry it and a customized policy cannot omit it. Templates
// that want to place it explicitly use ${WORK_TRACKER}; the seam then sees the
// header already present and appends nothing.
//
// The section deliberately does NOT invent a hive-side lifecycle for Linear
// issues: Linear's own GitHub integration links a PR whose branch name or
// body cites the identifier, moves the issue to In Progress when the PR
// opens, and to Done on merge (closing magic words). That is the parity of
// GitHub's `Fixes #N` auto-close, and agents are told to lean on it rather
// than mutate issue state by hand.

// workTrackerHeader is the marker the seam checks for before appending.
const workTrackerHeader = "## Work Tracker: Linear"

// workTrackerSection renders the tracker guidance, or "" when the work source
// is GitHub (the templates already describe GitHub) or unset.
func (s *Scheduler) workTrackerSection() string {
	if s.cfg == nil || s.cfg.Governor.WorkSource.Type != "linear" {
		return ""
	}
	ws := s.cfg.Governor.WorkSource.Linear
	var b strings.Builder
	b.WriteString(workTrackerHeader + " (GitHub Issues parity)\n\n")
	b.WriteString("This hive's WORK ITEMS come from Linear, not GitHub Issues. Wherever the policy\n")
	b.WriteString("above says to open, comment on, label, or cite an ISSUE, do it in Linear as\n")
	b.WriteString("described here. Everything about PULL REQUESTS is unchanged: code, branches,\n")
	b.WriteString("PRs, CI, and the `hold` label on PRs all stay on GitHub exactly as the policy says.\n\n")

	b.WriteString("- Work identity: items in the work list read `owner/repo!TEAM-123`. The part after\n")
	b.WriteString("  `!` is the Linear issue identifier; the part before is the GitHub repo the work is\n")
	b.WriteString("  done in. Team → repo mapping:\n")
	for _, t := range ws.Teams {
		line := fmt.Sprintf("    %s → %s", t.Key, t.Repo)
		if len(t.States) > 0 {
			line += fmt.Sprintf(" (states: %s)", strings.Join(t.States, ", "))
		}
		if t.Cycles == "current" {
			line += " (current cycle only)"
		}
		b.WriteString(line + "\n")
		for _, p := range t.Projects {
			repo := p.Repo
			if repo == "" {
				repo = t.Repo
			}
			b.WriteString(fmt.Sprintf("      project %q → %s\n", p.Name, repo))
		}
	}
	if ws.AssignedOnly {
		b.WriteString("  Only issues assigned or delegated to the Hive app are enumerated; do not pick up\n")
		b.WriteString("  other Linear issues you happen to find.\n")
	}

	b.WriteString("- Authentication: the hive placed your Linear credential in the environment, scoped\n")
	b.WriteString("  to your tier, exactly as it does for GitHub. Prefer the first that is set:\n")
	b.WriteString("    LINEAR_ACCESS_TOKEN → header `Authorization: Bearer $LINEAR_ACCESS_TOKEN`. The\n")
	b.WriteString("                          connected Hive app's own grant: writes are authored by the\n")
	b.WriteString("                          Hive app user. ALWAYS use this when it is set.\n")
	b.WriteString("    LINEAR_API_KEY      → header `Authorization: $LINEAR_API_KEY` (Linear's SDK, MCP\n")
	b.WriteString("                          server, and CLI read this variable natively). A personal\n")
	b.WriteString("                          key: writes are attributed to its human owner. Fallback\n")
	b.WriteString("                          only, for a hive with no connected app.\n")
	b.WriteString("  Endpoint: POST https://api.linear.app/graphql (JSON `{\"query\": ..., \"variables\": ...}`).\n")
	b.WriteString("  Neither set ⇒ you are advisory for Linear: read the work list, write beads, do not\n")
	b.WriteString("  go looking for, read, or echo any token file. Never put a token on a command line\n")
	b.WriteString("  (use the env var in the header); a token in a command is a token in your transcript.\n")

	b.WriteString("- ACMM gate: all traffic to api.linear.app flows through the hive proxy, which permits\n")
	b.WriteString("  mutations by your tier — `issueCreate` / `issueUpdate` / `issueAddLabel` /\n")
	b.WriteString("  `issueRemoveLabel` at ISSUES_ONLY and above; `commentCreate` with the converse\n")
	b.WriteString("  capability; `attachment*` at ISSUES_AND_PRS; deletes, archives, and admin operations\n")
	b.WriteString("  are denied at EVERY tier. A 403 from the proxy names the operation: report it in your\n")
	b.WriteString("  summary, do not work around it.\n")

	b.WriteString("- Filing an issue (replaces `gh issue create`): resolve the team id once, then create:\n")
	b.WriteString("    query  { teams(filter: { key: { eq: \"TEAM\" } }) { nodes { id } } }\n")
	b.WriteString("    mutation { issueCreate(input: { teamId: \"<id>\", title: \"[agent] …\", description: \"…\" }) { issue { identifier url } } }\n")
	b.WriteString("  Keep the same `[agent]` title prefix and body structure the policy prescribes for\n")
	b.WriteString("  GitHub issues. Comment with `commentCreate(input: { issueId, body })`.\n")
	b.WriteString("- Labels (replaces `--label`): a bare issueCreate lands UNLABELED. Wherever the policy\n")
	b.WriteString("  labels a GitHub issue, apply the Linear labels of the same names: resolve the ids\n")
	b.WriteString("  once per kick and pass them on create, or add them afterwards —\n")
	b.WriteString("    query    { issueLabels(filter: { name: { in: [\"quality\", \"testing\"] } }) { nodes { id name } } }\n")
	b.WriteString("    create   issueCreate(input: { …, labelIds: [\"<id>\", …], priority: <1 urgent|2 high|3 medium|4 low> })\n")
	b.WriteString("    or after issueAddLabel(id: \"<issueId>\", labelId: \"<id>\")\n")
	b.WriteString("  A label the workspace does not have is skipped, not created (issueLabelCreate is an\n")
	b.WriteString("  admin operation). Set `priority` from the policy's Impact line the same way.\n")

	b.WriteString("- Linking PRs (replaces `Fixes #N`): Linear's GitHub integration auto-links a PR to the\n")
	b.WriteString("  issue when the identifier appears in the PR TITLE, the BRANCH NAME, or the PR body.\n")
	b.WriteString("  Do all three:\n")
	b.WriteString("    PR title:  `[TEAM-123]: <short description>` — the bracketed identifier prefix is\n")
	b.WriteString("               the required convention, e.g. `[ONB-1952]: Add maintenance window tooling`\n")
	b.WriteString("    branch:    `<agent>/team-123-short-slug`\n")
	b.WriteString("    PR body:   cite it exactly as you would a GitHub issue:\n")
	b.WriteString("    `Fixes TEAM-123` / `Closes TEAM-123` / `Resolves TEAM-123` — ONLY when this PR fully\n")
	b.WriteString("      resolves the issue; Linear moves it to Done when the PR merges.\n")
	b.WriteString("    `Part of TEAM-123` / `Refs TEAM-123` / `Contributes to TEAM-123` — partial or\n")
	b.WriteString("      multi-phase work; non-closing, the issue stays open.\n")
	b.WriteString("  Linear moves the issue to In Progress when the PR opens and to Done on merge. Do NOT\n")
	b.WriteString("  change the state of PR-driven issues by hand — the integration owns that transition.\n")

	if len(ws.HoldLabels) > 0 {
		b.WriteString(fmt.Sprintf("- Hold: issues carrying any of these labels are excluded from your work list and are\n  off-limits, exactly like GitHub `hold`: %s. Never remove a hold label.\n", strings.Join(ws.HoldLabels, ", ")))
	} else {
		b.WriteString("- Hold: an issue the operator has held is excluded from your work list; never re-add\n  work you were not given.\n")
	}
	b.WriteString("- Never: delete or archive an issue, move it between teams or projects, change its\n")
	b.WriteString("  assignee or delegate, or create API keys or webhooks. The proxy refuses these anyway.\n")
	b.WriteString("\n")
	return b.String()
}

// addWorkTrackerSection appends the tracker guidance to a fully resolved kick
// message when the work source is Linear and the template did not already
// place it via ${WORK_TRACKER}. Empty messages (fail-closed kicks) stay empty.
func (s *Scheduler) addWorkTrackerSection(message string) string {
	if message == "" {
		return message
	}
	section := s.workTrackerSection()
	if section == "" || strings.Contains(message, workTrackerHeader) {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + section
}
