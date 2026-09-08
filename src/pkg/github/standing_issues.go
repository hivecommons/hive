package github

import "strings"

// Standing meta issues — control panels and reports that are never completable
// work — must not enter the actionable set. Two kinds exist in practice:
//
//   - The hive's OWN advisory report (advisory.go): a digest the hive files
//     and updates itself. Offering it back to agents as work is circular, and
//     since it never closes it inflates every repo's actionable count by one,
//     forever.
//   - Bot dependency dashboards (Renovate's "Dependency Dashboard",
//     Dependabot's equivalent): standing, machine-maintained control panels.
//     Worse than noise: an agent that "works" one can tick its checkboxes,
//     which INSTRUCTS the bot to act (open PRs, unblock updates) — an agent
//     operating maintainer controls.
//
// The hub's contribute queue already encodes exactly this judgment for HUMAN
// contributors — its default deny lists match `*dependency dashboard*` /
// `*renovate dashboard*` titles and the renovate/dependabot/mergeraptor bot
// authors (config.Hub.ContributeDenyTitles/DenyAuthors) — but the agent path
// had no equivalent, so the same issue hive refuses to offer a person was
// counted "Actionable" on the dashboard and fed into every kick prompt. This
// filter closes that gap at the same choice point as the hold/exempt checks.
//
// This is a STRUCTURAL skip (like `issue.IsPullRequest()`), not a third
// operator-facing exclusion mechanism: issue_filter.go's one-exclusion-story
// (governor.labels.exempt is THE configurable exclude) is preserved. The
// bot-dashboard arm is deliberately narrow — a known bot author AND a
// dashboard title, mirroring the contribute-queue defaults — so a human's
// issue that merely mentions dependencies can never be swallowed.

// botControlPanelAuthors are the bot logins whose standing dashboard issues
// are skipped. Kept aligned with config.Hub.ContributeDenyAuthors defaults.
var botControlPanelAuthors = []string{"renovate[bot]", "dependabot[bot]", "mergeraptor[bot]"}

// botControlPanelTitleFragments mark a standing dashboard title, matched
// case-insensitively as substrings. Kept aligned with the
// config.Hub.ContributeDenyTitles defaults (`*dependency dashboard*`,
// `*renovate dashboard*`).
var botControlPanelTitleFragments = []string{"dependency dashboard", "renovate dashboard"}

// standingMetaIssueReason reports whether an issue is a standing meta issue
// and, when it is, a short reason for logs/tests. Empty means actionable as
// far as this filter is concerned.
func standingMetaIssueReason(title, author string, labels []string) string {
	if title == advisoryTitle {
		return "hive's own advisory report"
	}
	for _, l := range labels {
		if strings.EqualFold(l, advisoryLabelName) {
			return "hive's own advisory report (label)"
		}
	}
	for _, bot := range botControlPanelAuthors {
		if !strings.EqualFold(author, bot) {
			continue
		}
		lower := strings.ToLower(title)
		for _, fragment := range botControlPanelTitleFragments {
			if strings.Contains(lower, fragment) {
				return "bot dependency dashboard (control panel, not work)"
			}
		}
	}
	return ""
}
