// Package prgate lets the hive publish the PR-contents merge gate as commit
// statuses on the exact head of a pull request, so the required context
// `verify PR contents` keeps reporting once GitHub stops running
// `pull_request_target` workflows by default in public repositories.
//
// verdict.go is the Go port of prTypeVerdict and prIssueVerdict in
// .github/scripts/integration-governance.cjs (the readonly adaptation of the
// upstream title verifier). The two implementations are held together by the
// shared golden corpus .github/scripts/testdata/pr-gate/verdict-corpus.json:
// a Go test and a JS test both replay it, so neither can drift on its own.
package prgate

import (
	"regexp"
	"strings"
	"unicode"
)

// Conclusion values of a Verdict. They match the check-run conclusions the
// JS verifier records.
const (
	ConclusionSuccess = "success"
	ConclusionFailure = "failure"
)

// Verdict names, identical to the required check names on v5.
const (
	NamePRType  = "PR Type"
	NamePRIssue = "PR Issue"
)

const (
	bom       rune = 0xFEFF
	nextLine  rune = 0x85
	variation      = "\uFE0F"
)

// Verdict is one title verdict: the same fields the JS verdict objects carry.
type Verdict struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	Text       string `json:"text"`
}

type prTypeEntry struct {
	kind, code, emoji string
}

// prTypes is the order PRTypeFromTitle tries shortcodes and emoji in.
var prTypes = []prTypeEntry{
	{"feature", ":sparkles:", "✨"}, {"bugfix", ":bug:", "🐛"}, {"docs", ":book:", "📖"},
	{"infra", ":seedling:", "🌱"}, {"breaking", ":warning:", "⚠"}, {"release", ":rocket:", "🚀"},
	{"infra", ":running:", "🏃"},
}

var prTypeEmoji = map[string]string{
	"feature": "✨", "bugfix": "🐛", "docs": "📖", "infra": "🌱", "breaking": "⚠", "release": "🚀",
}

var (
	wipPrefix     = regexp.MustCompile(`(?i)^\W?WIP\W`)
	scopePrefix   = regexp.MustCompile(`^\[[\w.-]*\]`)
	issueTagInStr = regexp.MustCompile(`#[0-9]+\b`)
)

// jsSpace reports whether r is stripped by JavaScript's String.prototype.trim,
// which differs from unicode.IsSpace on U+FEFF (trimmed by JS only) and U+0085
// (trimmed by Go only).
func jsSpace(r rune) bool {
	if r == bom {
		return true
	}
	if r == nextLine {
		return false
	}
	return unicode.IsSpace(r)
}

func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

// prTypeFromTitle is notes/common/prefix.go PRTypeFromTitle: the PR kind ("" when
// none) and the title without its prefix.
func prTypeFromTitle(title string) (string, string) {
	title = jsTrim(title)
	for _, t := range prTypes {
		if strings.HasPrefix(title, t.code) || strings.HasPrefix(title, t.emoji) {
			if strings.HasPrefix(title, t.code) {
				title = title[len(t.code):]
			}
			title = strings.TrimPrefix(title, t.emoji)
			title = strings.TrimPrefix(title, variation)
			return t.kind, jsTrim(title)
		}
	}
	return "", title
}

// goQuote is Go's %#q as the JS port renders it: a backquoted string unless the
// text holds a backquote or a control character, then a JSON-quoted one.
func goQuote(text string) string {
	if !strings.ContainsFunc(text, needsJSONQuote) {
		return "`" + text + "`"
	}
	return jsonQuote(text)
}

func needsJSONQuote(r rune) bool {
	return r == '`' || r <= 0x08 || (r >= 0x0A && r <= 0x1F) || r == 0x7F || r == bom
}

const hexDigits = "0123456789abcdef"

// jsonQuote mirrors JSON.stringify for a string (no HTML escaping).
func jsonQuote(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[r>>4])
			b.WriteByte(hexDigits[r&0x0F])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

const (
	typeCheckTitle    = "PR Type in Title"
	issueCheckTitle   = "Issue/PR tag in PR title"
	typeMissingPrefix = "I saw a title of "
	typeMissingSuffix = ", which doesn't seem to have any of the acceptable prefixes.\n\n" +
		"You need to have one of these as the prefix of your PR title:\n\n" +
		"- Breaking change: ⚠ (`:warning:`)\n" +
		"- Non-breaking feature: ✨ (`:sparkles:`)\n" +
		"- Patch fix: 🐛 (`:bug:`)\n" +
		"- Docs: 📖 (`:book:`)\n" +
		"- Release: 🚀 (`:rocket:`)\n" +
		"- Infra/Tests/Other: 🌱 (`:seedling:`)\n\n" +
		"More details can be found at [sigs.k8s.io/kubebuilder-release-tools/VERSIONING.md](https://sigs.k8s.io/kubebuilder-release-tools/VERSIONING.md)."
	issueTagText = "The title should just be descriptive.\n" +
		"Issue numbers belong in the PR body as either `Fixes #XYZ` (if it closes the issue or PR), " +
		"or something like `Related to #XYZ` (if it's just related)."
)

// PRTypeVerdict ports prTypeVerdict (verify/type.go verifyPRType).
func PRTypeVerdict(rawTitle string) Verdict {
	title := wipPrefix.ReplaceAllString(rawTitle, "")
	title = jsTrim(title)
	title = scopePrefix.ReplaceAllString(title, "")
	title = jsTrim(title)
	kind, finalTitle := prTypeFromTitle(title)
	if kind == "" {
		return Verdict{Name: NamePRType, Conclusion: ConclusionFailure, Title: typeCheckTitle,
			Summary: "No matching PR type indicator found in title.",
			Text:    typeMissingPrefix + goQuote(title) + typeMissingSuffix}
	}
	return Verdict{Name: NamePRType, Conclusion: ConclusionSuccess, Title: typeCheckTitle,
		Summary: "Found " + prTypeEmoji[kind] + " PR (" + kind + ")",
		Text:    "Final title:\n\n\t" + finalTitle + "\n"}
}

// PRIssueVerdict ports prIssueVerdict (verify/issue.go checkIssueInTitle).
func PRIssueVerdict(rawTitle string) Verdict {
	_, title := prTypeFromTitle(rawTitle)
	if issueTagInStr.MatchString(title) {
		return Verdict{Name: NamePRIssue, Conclusion: ConclusionFailure, Title: issueCheckTitle,
			Summary: "Your PR has an Issue or PR number in the title.", Text: issueTagText}
	}
	return Verdict{Name: NamePRIssue, Conclusion: ConclusionSuccess, Title: issueCheckTitle,
		Summary: "Your PR title does not contain any Issue or PR tags", Text: ""}
}
