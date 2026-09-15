// Package issueshape holds the shared shape validators for agent-authored
// GitHub issue and comment content. The rules originated in the issue-request
// watcher (pkg/github), which validates the request files agents drop for the
// hive's own creation path. The proxy (pkg/proxy) enforces the same rules on
// the OTHER creation path — an agent's `gh issue create` becomes a raw
// POST /repos/{owner}/{repo}/issues that never touches the watcher — so the
// logic lives here as a single source of truth for both callers.
package issueshape

import (
	"regexp"
	"strings"
)

var (
	angleBracketTokenRE            = regexp.MustCompile(`<([^>\n]+)>`)
	templatePlaceholderContentRE   = regexp.MustCompile(`^(?:[a-z][a-z0-9]*(?:[ -][a-z0-9]+)+|analysis|fix)$`)
	markdownHTMLTagsWithAttributes = map[string]bool{
		"a": true, "br": true, "code": true, "dd": true, "del": true, "details": true,
		"div": true, "dl": true, "dt": true, "em": true, "h1": true, "h2": true,
		"h3": true, "h4": true, "h5": true, "h6": true, "hr": true, "img": true,
		"ins": true, "kbd": true, "li": true, "ol": true, "p": true, "pre": true,
		"rp": true, "rt": true, "ruby": true, "s": true, "samp": true, "source": true,
		"span": true, "strong": true, "sub": true, "summary": true, "sup": true,
		"table": true, "tbody": true, "td": true, "tfoot": true, "th": true,
		"thead": true, "tr": true, "ul": true, "var": true,
	}
)

// UnsubstitutedTemplatePlaceholder reports whether text still contains an
// unfilled policy-template placeholder like <analysis>, <fix>, or
// <specific description of the documentation gap>: lowercase words, with
// multi-word placeholders separated by spaces or hyphens. It deliberately
// skips URLs, generic type parameters, and recognized GitHub Markdown HTML
// tags (including attribute forms such as <details open>) so those legitimate
// constructs are not mistaken for an unfilled template. The returned string is
// the full matched token (with angle brackets) for use in an error message.
func UnsubstitutedTemplatePlaceholder(text string) (string, bool) {
	for _, match := range angleBracketTokenRE.FindAllStringSubmatch(text, -1) {
		if len(match) != 2 {
			continue
		}
		content := strings.TrimSpace(match[1])
		if content == "" || strings.Contains(content, "://") || strings.ContainsAny(content, "/=,") {
			continue
		}
		firstField := content
		if fields := strings.Fields(content); len(fields) > 0 {
			firstField = fields[0]
		}
		if markdownHTMLTagsWithAttributes[strings.ToLower(firstField)] {
			continue
		}
		if templatePlaceholderContentRE.MatchString(content) {
			return match[0], true
		}
	}
	return "", false
}

// BodyHasMisEscapedNewlines reports whether body is a single physical line
// whose markdown structure was encoded as literal backslash-n escape
// sequences instead of real newlines (the classic `## X\n\n<...>\n\n## Y`
// specimen). A body with real line breaks may legitimately discuss "\n" in
// prose or a fenced code block, so it requires the combination — no real
// newline, multiple literal \n tokens, and \n\n / \n## structure — before
// flagging, to avoid blocking ordinary one-line text that mentions the escape.
func BodyHasMisEscapedNewlines(body string) bool {
	return !strings.Contains(body, "\n") &&
		strings.Count(body, `\n`) >= 2 &&
		(strings.Contains(body, `\n\n`) || strings.Contains(body, `\n## `))
}
