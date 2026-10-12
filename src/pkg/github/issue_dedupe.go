package github

import (
	"regexp"
	"strings"
)

// Agents file findings whose subject is stable but whose title carries a
// free-text qualifier after an em/en dash (or "--"), e.g.
//
//	[scanner] Split web/e2e/card-cache-compliance.spec.ts (1237 lines)
//	[scanner] Split web/e2e/card-cache-compliance.spec.ts (1237 lines) — extract cache helpers module
//	[scanner] Split web/e2e/card-cache-compliance.spec.ts (1237 lines) — extract snapshot/report helpers
//
// The qualifier is model-authored, so it varies on every scan of the same
// file. Exact-title dedupe therefore never fires and the same finding is
// re-filed every cycle, which is how one 1237-line file accumulated six
// simultaneously-open issues. canonicalIssueSubject strips that qualifier so
// the stable part of the subject can be compared instead.
var issueTitleQualifierRE = regexp.MustCompile(`\s+(?:\x{2014}|\x{2013}|--)\s+.*$`)

// Leading role tag such as "[scanner]" or "[sec-check]".
var issueTitleRolePrefixRE = regexp.MustCompile(`^\s*\[[^\]\s]+\]\s*`)

// Parenthetical counts such as "(1237 lines)" that drift between scans.
var issueTitleCountRE = regexp.MustCompile(`\s*\(\s*\d[\d,.]*\s*[A-Za-z]+\s*\)`)

// Trailing "Subject: free text" qualifier; the colon must be followed by
// whitespace so paths and "host:port" are left intact.
var issueTitleColonRE = regexp.MustCompile(`:\s+.*$`)

var issueTitleWhitespaceRE = regexp.MustCompile(`\s+`)

const (
	// A canonical subject is only trusted as a dedupe key when enough of the
	// title survives stripping. Without this, short/generic stems (e.g.
	// "[operations] CI failure") would collapse unrelated findings into one.
	minCanonicalSubjectLen   = 20
	minCanonicalSubjectWords = 3
)

// canonicalIssueSubject reduces an issue title to the stable subject used for
// duplicate detection: role prefix, parenthetical counts and qualifier stripped, lowercased, whitespace collapsed.
// It returns "" when the result is too short or too generic to be a safe
// dedupe key, which callers must treat as "no canonical match possible".
func canonicalIssueSubject(title string) string {
	t := strings.TrimSpace(title)
	t = issueTitleRolePrefixRE.ReplaceAllString(t, "")
	t = issueTitleQualifierRE.ReplaceAllString(t, "")
	t = issueTitleColonRE.ReplaceAllString(t, "")
	t = issueTitleCountRE.ReplaceAllString(t, "")
	t = issueTitleWhitespaceRE.ReplaceAllString(strings.ToLower(t), " ")
	t = strings.TrimSpace(t)
	if len(t) < minCanonicalSubjectLen || len(strings.Fields(t)) < minCanonicalSubjectWords {
		return ""
	}
	return t
}
