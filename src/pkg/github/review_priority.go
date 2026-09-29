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
// reads it to decide work. It is derived from metadata the PR already carries
// — the conventional title prefix and the lane label, plus changed paths for
// contributor PRs (ClassifyContributorReviewClass) — so adopting it costs
// zero agent behaviour change. The one writer is the opt-in, default-off
// review.priority_labels toggle, which mirrors the review queue's rank onto a
// review-priority/* label (review_queue.go, #9590).
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

// Class sources: which signal ClassifyReviewClass (or the contributor path
// fallback) actually used. They exist for the review queue's rank reasons
// (#9590), so a maintainer can see WHY a PR landed in its tier.
const (
	reviewClassSourceTitle = "title prefix"
	reviewClassSourceLabel = "label"
	reviewClassSourcePaths = "changed paths"
	reviewClassSourceNone  = "no title prefix, label or path signal"
)

// ClassifyReviewClass derives the triage class from a PR's title and labels.
// The title prefix wins over labels because it is the more explicit signal
// (a quality-lane agent that ships a genuine "fix:" gets fix priority), and
// labels break the tie for prefix-less titles. Anything else is unknown.
func ClassifyReviewClass(title string, labels []string) ReviewClass {
	class, _ := classifyReviewClassWithSource(title, labels)
	return class
}

// classifyReviewClassWithSource is ClassifyReviewClass plus the name of the
// signal that decided the class (reviewClassSource*).
func classifyReviewClassWithSource(title string, labels []string) (ReviewClass, string) {
	trimmed := strings.TrimSpace(title)
	for _, p := range reviewFixEmojiPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return ReviewClassFix, reviewClassSourceTitle
		}
	}
	for _, p := range reviewTestEmojiPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return ReviewClassTests, reviewClassSourceTitle
		}
	}
	for _, p := range reviewDocsEmojiPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return ReviewClassRefactorDocs, reviewClassSourceTitle
		}
	}

	// Strip any leading emoji / punctuation run so "🌱 fix: ..." and
	// "fix: ..." classify the same way, then look at the first word.
	words := strings.ToLower(strings.TrimLeftFunc(trimmed, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '['
	}))
	for _, p := range reviewFixWordPrefixes {
		if hasWordPrefix(words, p) {
			return ReviewClassFix, reviewClassSourceTitle
		}
	}
	for _, p := range reviewTestWordPrefixes {
		if hasWordPrefix(words, p) {
			return ReviewClassTests, reviewClassSourceTitle
		}
	}
	for _, p := range reviewDocsWordPrefixes {
		if hasWordPrefix(words, p) {
			return ReviewClassRefactorDocs, reviewClassSourceTitle
		}
	}

	for _, l := range labels {
		ll := strings.ToLower(l)
		for _, want := range reviewFixLaneLabels {
			if ll == want {
				return ReviewClassFix, reviewClassSourceLabel
			}
		}
	}
	for _, l := range labels {
		ll := strings.ToLower(l)
		for _, want := range reviewTestLaneLabels {
			if ll == want {
				return ReviewClassTests, reviewClassSourceLabel
			}
		}
	}
	for _, l := range labels {
		ll := strings.ToLower(l)
		for _, want := range reviewDocsLaneLabels {
			if ll == want {
				return ReviewClassRefactorDocs, reviewClassSourceLabel
			}
		}
	}
	return ReviewClassUnknown, reviewClassSourceNone
}

// agentLaneLabelPrefix marks the lane label every hive agent PR carries
// ("agent/scanner", "agent/quality", ...).
const agentLaneLabelPrefix = "agent/"

// hasAgentLaneLabel reports whether labels include an agent lane label.
func hasAgentLaneLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), agentLaneLabelPrefix) {
			return true
		}
	}
	return false
}

// ClassifyContributorReviewClass extends ClassifyReviewClass to PRs that
// carry no agent lane label - outside contributors' and maintainers' PRs,
// which rarely follow the lane conventions (#9590). It tries the same title
// prefix and label signals first; only when both are silent does it look at
// the changed paths (ClassifyReviewClassFromPaths).
//
// A PR WITH an agent lane label classifies exactly as ClassifyReviewClass
// does, paths ignored, so agent PRs keep the class they always had.
func ClassifyContributorReviewClass(title string, labels, paths []string) ReviewClass {
	class, _ := classifyContributorReviewClassWithSource(title, labels, paths)
	return class
}

func classifyContributorReviewClassWithSource(title string, labels, paths []string) (ReviewClass, string) {
	class, source := classifyReviewClassWithSource(title, labels)
	if class != ReviewClassUnknown || hasAgentLaneLabel(labels) {
		return class, source
	}
	if pc := ClassifyReviewClassFromPaths(paths); pc != ReviewClassUnknown {
		return pc, reviewClassSourcePaths
	}
	return ReviewClassUnknown, reviewClassSourceNone
}

// Path signals for ClassifyReviewClassFromPaths. A path is a TEST path when
// its file name carries a test suffix/infix or any directory segment is a
// test directory; a DOCS path when it has a docs extension or sits under a
// docs directory.
var (
	reviewTestFileSuffixes = []string{"_test.go", "_test.py", "_spec.rb", "_test.rs"}
	reviewTestFileInfixes  = []string{".test.", ".spec."}
	reviewTestFilePrefixes = []string{"test_"}
	reviewTestDirs         = []string{"test", "tests", "testdata", "__tests__", "e2e", "testutil"}
	reviewDocsExtensions   = []string{".md", ".mdx", ".rst", ".adoc"}
	reviewDocsDirs         = []string{"docs", "doc"}
)

// ClassifyReviewClassFromPaths derives a class from a PR's changed files
// alone. It is deliberately conservative: only a PR whose EVERY path is a
// test file is T2 tests, and only one whose every path is documentation is
// T1 docs. A path signal can never make a PR a fix - "this touches code"
// says nothing about whether it repairs something - so anything mixed, and
// an empty list, is unknown.
func ClassifyReviewClassFromPaths(paths []string) ReviewClass {
	allTests, allDocs, seen := true, true, false
	for _, p := range paths {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		seen = true
		if !isReviewTestPath(p) {
			allTests = false
		}
		if !isReviewDocsPath(p) {
			allDocs = false
		}
	}
	switch {
	case !seen:
		return ReviewClassUnknown
	case allTests:
		return ReviewClassTests
	case allDocs:
		return ReviewClassRefactorDocs
	default:
		return ReviewClassUnknown
	}
}

// isReviewTestPath expects a lower-cased, slash-separated path.
func isReviewTestPath(p string) bool {
	segments := strings.Split(p, "/")
	base := segments[len(segments)-1]
	for _, s := range reviewTestFileSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	for _, s := range reviewTestFileInfixes {
		if strings.Contains(base, s) {
			return true
		}
	}
	for _, s := range reviewTestFilePrefixes {
		if strings.HasPrefix(base, s) {
			return true
		}
	}
	for _, dir := range segments[:len(segments)-1] {
		for _, want := range reviewTestDirs {
			if dir == want {
				return true
			}
		}
	}
	return false
}

// isReviewDocsPath expects a lower-cased, slash-separated path.
func isReviewDocsPath(p string) bool {
	segments := strings.Split(p, "/")
	base := segments[len(segments)-1]
	for _, ext := range reviewDocsExtensions {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	for _, dir := range segments[:len(segments)-1] {
		for _, want := range reviewDocsDirs {
			if dir == want {
				return true
			}
		}
	}
	return false
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
