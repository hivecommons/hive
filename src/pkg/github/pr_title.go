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

// The PR-title emoji convention (CONTRIBUTING.md, AGENTS.md): one emoji
// per change kind, checked by the repository's `PR Type` gate. 📖 is
// documentation and 📝 is a proposal — hivecommons/hive#13131 opened a docs
// PR as "📝 docs: …" and failed the gate on exactly that swap.
const (
	PRTitleEmojiFeature  = "✨"
	PRTitleEmojiFix      = "🐛"
	PRTitleEmojiDocs     = "📖"
	PRTitleEmojiProposal = "📝"
	PRTitleEmojiBreaking = "⚠️"
	PRTitleEmojiOther    = "🌱"
)

// prTitleTypeEmoji maps a Conventional Commits type to its convention emoji.
var prTitleTypeEmoji = map[string]string{
	"feat": PRTitleEmojiFeature, "feature": PRTitleEmojiFeature,
	"fix": PRTitleEmojiFix, "bugfix": PRTitleEmojiFix, "hotfix": PRTitleEmojiFix,
	"docs": PRTitleEmojiDocs, "doc": PRTitleEmojiDocs,
	"proposal": PRTitleEmojiProposal, "rfc": PRTitleEmojiProposal, "adr": PRTitleEmojiProposal,
	"chore": PRTitleEmojiOther, "refactor": PRTitleEmojiOther, "test": PRTitleEmojiOther, "tests": PRTitleEmojiOther,
	"ci": PRTitleEmojiOther, "build": PRTitleEmojiOther, "perf": PRTitleEmojiOther, "style": PRTitleEmojiOther, "revert": PRTitleEmojiOther,
}

// prTitleEmojiRE splits a title into its leading emoji (any symbol run,
// variation selectors included) and the Conventional Commits header that
// follows: "📝 docs(ci): …" → ["📝", "docs", "(ci)", "", "…"].
var prTitleEmojiRE = regexp.MustCompile(`^([\p{So}\x{FE0F}\x{200D}]+)\s+([A-Za-z]+)(\([^)]*\))?(!)?:\s+(.+)$`)

// PRTitleEmojiFor returns the convention emoji for a Conventional Commits
// header, or "" when the type is not one the convention names. A "!"
// breaking marker wins over the type.
func PRTitleEmojiFor(typ string, breaking bool) string {
	if breaking {
		return PRTitleEmojiBreaking
	}
	return prTitleTypeEmoji[strings.ToLower(strings.TrimSpace(typ))]
}

// NormalizePRTitleEmoji makes a title's leading emoji agree with its
// Conventional Commits type: "📝 docs: …" becomes "📖 docs: …", "📖 fix: …"
// becomes "🐛 fix: …". A title with no leading emoji is never given one
// (repositories whose own title gate wants a bare "fix: …" header keep
// it), a type the convention does not name keeps its emoji, and a title
// already in agreement is returned unchanged.
func NormalizePRTitleEmoji(title string) string {
	m := prTitleEmojiRE.FindStringSubmatch(title)
	if m == nil {
		return title
	}
	want := PRTitleEmojiFor(m[2], m[4] == "!")
	if want == "" || prTitleEmojiEqual(m[1], want) {
		return title
	}
	normalized := want + " " + strings.TrimSpace(title[len(m[1]):])
	if utf8.RuneCountInString(normalized) > maxGitHubPRTitleLength {
		return title
	}
	return normalized
}

// prTitleEmojiEqual compares two emoji ignoring the U+FE0F presentation
// selector ("⚠" and "⚠️" are the same mark).
func prTitleEmojiEqual(a, b string) bool {
	strip := func(s string) string { return strings.ReplaceAll(s, "\uFE0F", "") }
	return strip(a) == strip(b)
}
