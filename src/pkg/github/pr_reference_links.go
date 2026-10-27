package github

import (
	"regexp"
	"strconv"
	"strings"
)

// Via values on a PRLinkedIssue the enumeration derived from the PR's own
// text rather than from GitHub's closingIssuesReferences. An empty Via is
// GitHub's closing link. The dashboard's activity monitor draws every
// non-empty Via except PRLinkViaFixes as a soft "refs" link so maintainers
// can tell a PR that closes an issue from one that merely works towards it
// (hivecommons/hive#13544: a body opening with `Refs #13542 (part 1 of 2)`
// rendered "no linked issue").
const (
	// PRLinkViaFixes is a closing keyword in the title or body that GitHub
	// did not derive a closing reference from (a PR against a release line,
	// or the prose form `Resolves issue #N`).
	PRLinkViaFixes = "fixes"
	// PRLinkViaRefs is a reference keyword in the body: Refs, Ref,
	// References, Related (to), Part of, Towards, See, Addresses, ….
	PRLinkViaRefs = "refs"
	// PRLinkViaTitle is a parenthesised reference in the title:
	// `(#N, part 1)`, `(#N)`.
	PRLinkViaTitle = "title"
	// PRLinkViaMention is a bare `#N` in the body's first paragraph with no
	// keyword at all; consumers must resolve it to an issue (not a PR)
	// before showing it.
	PRLinkViaMention = "mention"
)

// prReferenceLinkLimit bounds the references one PR contributes, matching
// the closingIssuesReferences page the GraphQL pass reads.
const prReferenceLinkLimit = 20

// prTrackerReferenceKeywords is the display-tier reference vocabulary. It
// is wider than referenceKeywords on purpose: "related to" and "see" are
// excluded from the claim tier because a passing mention must not withhold
// an issue from the contribute queue, but on the activity monitor a soft
// link to the issue a PR talks about is exactly what a maintainer wants.
var prTrackerReferenceKeywords = append([]string{
	"related to", "relates to", "related", "see also", "see",
}, referenceKeywords...)

var prTrackerReferencePattern = regexp.MustCompile(
	`(?i)\b(` + strings.Join(prTrackerReferenceKeywords, "|") + `)\b[^.\n#]{0,40}?(?:([\w.-]+/[\w.-]+))?#(\d+)` + referenceListTail)

// prTitleReferencePattern is a parenthesised issue reference in a PR title:
// `(#13542, part 1)`, `(#13542 part 2 of 2)`, `(#13542)`,
// `(acme/other#5)`. The optional tail after the number may not contain
// another '#', so a title naming two issues yields both.
var prTitleReferencePattern = regexp.MustCompile(`\((?:([\w.-]+/[\w.-]+))?#(\d+)(?:[ \t]*[,;:]?[ \t]*[^#()]*)?\)`)

// PRReferencedIssues reads the issues a PR's title and body refer to
// without GitHub having derived a closing link, in priority order: closing
// keywords in the text (PRLinkViaFixes), reference keywords in the body
// (PRLinkViaRefs), parenthesised references in the title (PRLinkViaTitle),
// then bare `#N` mentions in the body's first paragraph (PRLinkViaMention)
// unless the text says the PR does not cover them (prMentionUncovered).
// Only same-repo references are kept — a bare `#N`, or `owner/repo#N`
// naming repo itself — never the PR's own number, each number once at its
// strongest tier. Title and State are left empty: the dashboard fills them
// when the issue is in its snapshot.
func PRReferencedIssues(title, body, repo string, prNumber int) []PRLinkedIssue {
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if title == "" && body == "" {
		return nil
	}
	var out []PRLinkedIssue
	seen := map[int]bool{}
	add := func(refRepo string, n int, via string) {
		if n <= 0 || n == prNumber || seen[n] || len(out) >= prReferenceLinkLimit {
			return
		}
		if refRepo != "" && !strings.EqualFold(refRepo, repo) {
			return
		}
		seen[n] = true
		out = append(out, PRLinkedIssue{Number: n, Repo: repo, Via: via})
	}
	for _, ref := range ParseClaimedIssues(title+"\n"+body, repo) {
		add(ref.Repo, ref.Issue, PRLinkViaFixes)
	}
	cleanBody := stripClosingFalsePositiveText(body)
	for _, ref := range parseRefs(prTrackerReferencePattern, cleanBody, repo) {
		add(ref.Repo, ref.Issue, PRLinkViaRefs)
	}
	for _, m := range prTitleReferencePattern.FindAllStringSubmatch(title, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		add(m[1], n, PRLinkViaTitle)
	}
	lead := firstParagraph(cleanBody)
	for _, m := range issueRefPattern.FindAllStringSubmatchIndex(lead, -1) {
		if prMentionUncovered(lead, m[0], m[1]) {
			continue
		}
		n, err := strconv.Atoi(lead[m[4]:m[5]])
		if err != nil {
			continue
		}
		refRepo := ""
		if m[2] >= 0 {
			refRepo = lead[m[2]:m[3]]
		}
		add(refRepo, n, PRLinkViaMention)
	}
	return out
}

// prMentionUncoveredPattern is prose after a bare `#N` saying the PR leaves
// that issue alone: "#14559 backend-specific routes remain", "#7 is out of
// scope".
var prMentionUncoveredPattern = regexp.MustCompile(`(?i)\b(?:remains?|remaining|left (?:open|for|to)|not covered|out of scope|not in this pr)\b`)

// prMentionQualifiedPattern is text ending in a reference, so a parenthetical
// opening right after it qualifies that reference.
var prMentionQualifiedPattern = regexp.MustCompile(`#\d+[ \t]*$`)

// prMentionUncovered reports whether the bare `#N` at text[start:end] names
// work the PR does not cover (hivecommons/hive#14667): its clause, up to the
// next , ; . ) or line end, says the issue remains, or it sits inside a
// parenthetical qualifying the reference just before it, as in
// `Part of #14550 (tracker; #14559 backend-specific routes remain)`.
func prMentionUncovered(text string, start, end int) bool {
	clause := text[end:]
	if i := strings.IndexAny(clause, ",;.)\n"); i >= 0 {
		clause = clause[:i]
	}
	if prMentionUncoveredPattern.MatchString(clause) {
		return true
	}
	open := strings.LastIndexByte(text[:start], '(')
	if open < 0 || strings.ContainsRune(text[open:start], ')') {
		return false
	}
	return prMentionQualifiedPattern.MatchString(text[:open])
}

// firstParagraph is text up to its first blank line.
func firstParagraph(text string) string {
	text = strings.TrimLeft(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if i := strings.Index(text, "\n\n"); i >= 0 {
		return text[:i]
	}
	return text
}
