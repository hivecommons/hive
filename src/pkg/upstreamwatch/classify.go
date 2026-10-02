package upstreamwatch

import "strings"

// Class is the kind of change an upstream item represents. The renderer
// prints it and fixer agents can triage on it.
type Class string

const (
	// ClassSecurity is a security fix or advisory. It wins over every other
	// class: a change that is both a feature and a security fix is filed as
	// security so it is never buried.
	ClassSecurity Class = "security"
	// ClassBugfix is a bug fix (conventional-commit "fix:").
	ClassBugfix Class = "bugfix"
	// ClassFeature is a new feature or enhancement (conventional-commit
	// "feat:").
	ClassFeature Class = "feature"
	// ClassChore is anything else: docs, refactors, tests, build or CI
	// churn. It is the fallback when nothing else matches.
	ClassChore Class = "chore"
)

// securityLabels are upstream label names (lower-cased) that mark an item as
// a security concern regardless of its title.
var securityLabels = map[string]bool{
	"security":      true,
	"vulnerability": true,
	"cve":           true,
	"advisory":      true,
}

// bugLabels and featureLabels map common upstream labels onto a class. Labels
// are consulted before the title prefix.
var (
	bugLabels = map[string]bool{
		"bug":    true,
		"bugfix": true,
		"defect": true,
		"fix":    true,
	}
	featureLabels = map[string]bool{
		"feature":     true,
		"enhancement": true,
		"feat":        true,
	}
)

// Classify maps an item to a Class. Upstream labels are consulted first and
// then the conventional-commit prefix of the title; a security signal from
// either source wins over everything else.
func Classify(item Item) Class {
	prefix := conventionalPrefix(item.Title)
	if hasSecuritySignal(item.Labels, item.Title, prefix) {
		return ClassSecurity
	}
	for _, l := range item.Labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if bugLabels[l] {
			return ClassBugfix
		}
		if featureLabels[l] {
			return ClassFeature
		}
	}
	switch prefix {
	case "fix":
		return ClassBugfix
	case "feat":
		return ClassFeature
	}
	return ClassChore
}

// hasSecuritySignal reports whether any label, the conventional-commit scope
// or the title text marks the item as security-related.
func hasSecuritySignal(labels []string, title, prefix string) bool {
	for _, l := range labels {
		if securityLabels[strings.ToLower(strings.TrimSpace(l))] {
			return true
		}
	}
	if prefix == "security" {
		return true
	}
	lower := strings.ToLower(title)
	return strings.Contains(lower, "security") ||
		strings.Contains(lower, "vulnerabilit") ||
		strings.Contains(lower, "cve-")
}

// conventionalPrefix returns the lower-cased type of a conventional-commit
// title ("feat", "fix", "chore", ...) or "" when the title has none. The
// optional "(scope)" and the breaking-change "!" are both tolerated.
func conventionalPrefix(title string) string {
	title = strings.TrimSpace(title)
	colon := strings.IndexByte(title, ':')
	if colon <= 0 {
		return ""
	}
	head := title[:colon]
	if i := strings.IndexByte(head, '('); i >= 0 {
		head = head[:i]
	}
	head = strings.TrimSuffix(head, "!")
	head = strings.ToLower(strings.TrimSpace(head))
	if strings.ContainsAny(head, " \t") {
		return ""
	}
	return head
}

// Difficulty is a cheap estimate of how hard porting an item is, derived from
// the size of the upstream change.
type Difficulty string

const (
	// DifficultyEasy is a small, self-contained change.
	DifficultyEasy Difficulty = "easy"
	// DifficultyModerate is a change of middling size.
	DifficultyModerate Difficulty = "moderate"
	// DifficultyHard is a large change spanning many files or lines.
	DifficultyHard Difficulty = "hard"
)

// Difficulty thresholds. A change is easy when it is small on both axes and
// hard when it is large on either; everything in between is moderate.
const (
	easyMaxFiles = 2
	easyMaxLines = 50
	hardMinFiles = 10
	hardMinLines = 500
)

// EstimateDifficulty estimates port difficulty from the number of touched
// files and total lines changed. Releases carry no file or line counts, so
// their size is unknown and they come back moderate.
func EstimateDifficulty(item Item) Difficulty {
	if item.Kind == KindRelease {
		return DifficultyModerate
	}
	files := len(item.Files)
	lines := item.Additions + item.Deletions
	if files >= hardMinFiles || lines >= hardMinLines {
		return DifficultyHard
	}
	if files <= easyMaxFiles && lines <= easyMaxLines {
		return DifficultyEasy
	}
	return DifficultyModerate
}
