package github

import (
	"strings"
)

// botControlPanelAuthors are the bot logins whose standing dashboard issues
// are skipped. Kept aligned with config.Hub.ContributeDenyAuthors defaults.
var botControlPanelAuthors = []string{"renovate[bot]", "dependabot[bot]", "mergeraptor[bot]"}

// botControlPanelTitleFragments mark a standing dashboard title, matched
// case-insensitively as substrings. Kept aligned with the
// config.Hub.ContributeDenyTitles defaults (`*dependency dashboard*`,
// `*renovate dashboard*`).
var botControlPanelTitleFragments = []string{"dependency dashboard", "renovate dashboard"}

// dedupeAdvisoryLabelName marks a dedupe-agent checklist that recommends no
// close, relabel, or PR edit (hivecommons/hive#11864). It is filed without
// needs-decision, so this label is what keeps it out of the work queue and
// out of the held bucket; it is counted as a hive advisory report.
const dedupeAdvisoryLabelName = "triage/advisory"

// DedupeAdvisoryLabelName is dedupeAdvisoryLabelName for the dashboard's
// stage tracker, which classifies issues wearing it as advisories.
const DedupeAdvisoryLabelName = dedupeAdvisoryLabelName

// standingMetaIssueReason preserves the diagnostic predicate introduced by
// #6158 while the typed classifier lets the repository breakdown distinguish
// advisory reports from dependency dashboards without reimplementing policy.
func standingMetaIssueReason(title, author string, labels []string) string {
	switch standingMetaIssueKindFor(title, author, labels) {
	case standingMetaHiveAdvisory:
		return "hive's own advisory report"
	case standingMetaDependencyDashboard:
		return "bot dependency dashboard (control panel, not work)"
	default:
		return ""
	}
}

type standingMetaIssueKind uint8

const (
	standingMetaNone standingMetaIssueKind = iota
	standingMetaHiveAdvisory
	standingMetaDependencyDashboard
)

func standingMetaIssueKindFor(title, author string, labels []string) standingMetaIssueKind {
	if isAdvisoryTitle(title) {
		return standingMetaHiveAdvisory
	}
	for _, label := range labels {
		if strings.EqualFold(label, advisoryLabelName) || strings.EqualFold(label, dedupeAdvisoryLabelName) {
			return standingMetaHiveAdvisory
		}
	}
	for _, bot := range botControlPanelAuthors {
		if !strings.EqualFold(author, bot) {
			continue
		}
		lowerTitle := strings.ToLower(title)
		for _, fragment := range botControlPanelTitleFragments {
			if strings.Contains(lowerTitle, fragment) {
				return standingMetaDependencyDashboard
			}
		}
	}
	return standingMetaNone
}
