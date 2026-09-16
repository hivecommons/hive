package github

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxGitHubPRTitleLength = 256

var (
	leadingLanePRTitleRE      = regexp.MustCompile(`^\[([A-Za-z0-9_-]+)\]\s*(.+)$`)
	conventionalCommitTitleRE = regexp.MustCompile(`^(feat|fix|chore|docs|style|refactor|perf|test|ci|build|revert)(\(.+\))?(!)?: .+`)
)

// NormalizePRTitle moves a leading [lane] prefix to the end of the title when
// the remainder is a valid Conventional Commits header. Titles that are not
// safely recognizable as lane-prefixed Conventional Commits headers are
// returned unchanged, preserving issue-style titles and arbitrary repository
// conventions. If normalization would exceed GitHub's 256-character PR title
// limit, the original title is returned rather than truncating user content.
func NormalizePRTitle(title string) string {
	matches := leadingLanePRTitleRE.FindStringSubmatch(title)
	if len(matches) != 3 {
		return title
	}

	lane := matches[1]
	remainder := matches[2]
	if !conventionalCommitTitleRE.MatchString(remainder) {
		return title
	}

	suffix := " [" + lane + "]"
	normalized := remainder
	if !strings.HasSuffix(remainder, suffix) {
		normalized += suffix
	}
	if utf8.RuneCountInString(normalized) > maxGitHubPRTitleLength {
		return title
	}
	return normalized
}
