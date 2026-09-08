package github

import (
	"sort"
	"strings"
	"unicode"
)

// ReviewClass is the coarse triage class of a pull request as a REVIEWER
// sees it, per src/docs/review-queue-triage.md (#6183). It exists so the
// queue snapshot (last-actionable.json) and the dashboard's PR list can be
// ordered fixes > refactors/docs > tests instead of purely by age, which at
// ACMM L5 hold-gated left production fixes aging behind a wall of coverage
// PRs of identical apparent urgency.
//
// It is PRESENTATIONAL ONLY. Nothing in the governor, scheduler, or any agent
// reads it; no label is written from it. It is derived from metadata the PR
// already carries — the conventional title prefix and the lane label — so
// adopting it costs zero label churn and zero agent behaviour change.
type ReviewClass string

const (
	// ReviewClassFix is T0: runtime bug fixes, security fixes, CI/pipeline
	// fixes. Latency has production cost; these are also the smallest diffs.
	ReviewClassFix ReviewClass = "fix"
	// ReviewClassRefactorDocs is T1: refactors (even dead-code deletion),
	// docs, and planning pages that gate other decisions. Refactors go stale
	// fastest as the tree moves.
	ReviewClassRefactorDocs ReviewClass = "refactor-docs"
	// ReviewClassTests is T2: additive test coverage. Near-zero wrong-merge
	// cost; safe to age.
	ReviewClassTests ReviewClass = "tests"
	// ReviewClassUnknown is the empty class for PRs carrying no recognised
	// prefix or lane label (adopter feature PRs, hand-written titles). They
	// rank with T1 so an unlabelled feature is never pushed behind coverage
	// PRs merely for lacking a prefix.
	ReviewClassUnknown ReviewClass = ""
)

// Review ranks: lower sorts first. Unknown deliberately shares the T1 rank
// (see ReviewClassUnknown).
const (
	reviewRankFix          = 0
	reviewRankRefactorDocs = 1
	reviewRankTests        = 2
)

// reviewRank maps a class to its sort rank.
func reviewRank(c ReviewClass) int {
	switch c {
	case ReviewClassFix:
		return reviewRankFix
	case ReviewClassTests:
		return reviewRankTests
	default:
		return reviewRankRefactorDocs
	}
}

// Title-prefix signals, checked in this order. The emoji set follows
// CONTRIBUTING.md's PR-title convention (🐛 fix, 📖 docs, ...); the word
// prefixes are the conventional-commit forms agents and humans both use
// ("fix:", "fix(proxy):", "refactor:", "test:") plus the "[lane]" prefix that
// classify.classifyLane already treats as the most explicit routing signal.
// A word prefix only matches as a whole word (see hasWordPrefix) so
// "fixtures: ..." is not a fix.
var (
	reviewFixEmojiPrefixes  = []string{"🐛", "🔒", "🛡", "🚑", "🔥"}
	reviewTestEmojiPrefixes = []string{"🧪", "✅"}
	reviewDocsEmojiPrefixes = []string{"📖", "📝", "🏗", "♻"}

	reviewFixWordPrefixes  = []string{"fix", "hotfix", "bugfix", "security", "sec", "[scanner]", "[ci-maintainer]", "[sec-check]"}
	reviewTestWordPrefixes = []string{"test", "tests", "coverage", "[quality]"}
	reviewDocsWordPrefixes = []string{"refactor", "docs", "doc", "chore", "planning", "[architect]", "[guide]", "[strategist]"}
)

// Lane labels (agent/<lane>) consulted when the title carries no recognised
// prefix. These are the lanes as configured by default in pkg/config; a
// custom lane name simply classifies as unknown.
var (
	reviewFixLaneLabels  = []string{"agent/scanner", "agent/ci-maintainer", "agent/sec-check", "kind/bug", "kind/security", "kind/regression"}
	reviewTestLaneLabels = []string{"agent/quality"}
	reviewDocsLaneLabels = []string{"agent/architect", "agent/guide", "agent/strategist", "kind/documentation", "kind/cleanup"}
)

// ClassifyReviewClass derives the triage class from a PR's title and labels.
// The title prefix wins over labels because it is the more explicit signal
// (a quality-lane agent that ships a genuine "fix:" gets fix priority), and
// labels break the tie for prefix-less titles. Anything else is unknown.
func ClassifyReviewClass(title string, labels []string) ReviewClass {
	trimmed := strings.TrimSpace(title)
	for _, p := range reviewFixEmojiPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return ReviewClassFix
		}
	}
	for _, p := range reviewTestEmojiPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return ReviewClassTests
		}
	}
	for _, p := range reviewDocsEmojiPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return ReviewClassRefactorDocs
		}
	}

	// Strip any leading emoji / punctuation run so "🌱 fix: ..." and
	// "fix: ..." classify the same way, then look at the first word.
	words := strings.ToLower(strings.TrimLeftFunc(trimmed, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '['
	}))
	for _, p := range reviewFixWordPrefixes {
		if hasWordPrefix(words, p) {
			return ReviewClassFix
		}
	}
	for _, p := range reviewTestWordPrefixes {
		if hasWordPrefix(words, p) {
			return ReviewClassTests
		}
	}
	for _, p := range reviewDocsWordPrefixes {
		if hasWordPrefix(words, p) {
			return ReviewClassRefactorDocs
		}
	}

	for _, l := range labels {
		ll := strings.ToLower(l)
		for _, want := range reviewFixLaneLabels {
			if ll == want {
				return ReviewClassFix
			}
		}
	}
	for _, l := range labels {
		ll := strings.ToLower(l)
		for _, want := range reviewTestLaneLabels {
			if ll == want {
				return ReviewClassTests
			}
		}
	}
	for _, l := range labels {
		ll := strings.ToLower(l)
		for _, want := range reviewDocsLaneLabels {
			if ll == want {
				return ReviewClassRefactorDocs
			}
		}
	}
	return ReviewClassUnknown
}

// hasWordPrefix reports whether s starts with word as a WHOLE word: the word
// must be followed by end-of-string or a delimiter (":", "(", "!", "/", "]",
// whitespace). "fix: x", "fix(proxy): x", "fix x" all match "fix";
// "fixtures: x" does not. A "[lane]" prefix already ends in "]" so it matches
// verbatim.
func hasWordPrefix(s, word string) bool {
	if !strings.HasPrefix(s, word) {
		return false
	}
	if len(s) == len(word) {
		return true
	}
	switch s[len(word)] {
	case ':', '(', '!', '/', ']', ' ', '\t', '-':
		return true
	}
	return false
}

// SortPullRequestsForReview orders prs in place by review class (fixes >
// refactors/docs > tests) and, within a class, oldest first — the same
// oldest-first order the issue queue already uses, so nothing regresses for
// PRs of one class. The sort is stable so PRs with equal class and creation
// time keep their enumeration order. Each PR's ReviewClass is (re)derived
// here so the snapshot carries the class the sort actually used.
func SortPullRequestsForReview(prs []PullRequest) {
	for i := range prs {
		prs[i].ReviewClass = ClassifyReviewClass(prs[i].Title, prs[i].Labels)
	}
	sort.SliceStable(prs, func(i, j int) bool {
		ri, rj := reviewRank(prs[i].ReviewClass), reviewRank(prs[j].ReviewClass)
		if ri != rj {
			return ri < rj
		}
		return prs[i].CreatedAt.Before(prs[j].CreatedAt)
	})
}

// SortHoldItemsForReview applies the same class-then-age order to the hold
// list, which at ACMM L5 hold-gated IS the human review queue (every agent PR
// carries `hold`). HoldItems already carry the class computed at enumeration
// time from the labels fetchPRs saw. Held issues have no class and rank with
// T1; a zero CreatedAt (a snapshot written before the field existed) sorts
// first within its class rather than being dropped.
func SortHoldItemsForReview(items []HoldItem) {
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := reviewRank(items[i].ReviewClass), reviewRank(items[j].ReviewClass)
		if ri != rj {
			return ri < rj
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
}
