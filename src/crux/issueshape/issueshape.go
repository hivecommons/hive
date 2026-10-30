// Package issueshape holds the shared shape validators for agent-authored
// GitHub issue and comment content. The rules originated in the issue-request
// watcher (pkg/github), which validates the request files agents drop for the
// hive's own creation path. The proxy (pkg/proxy) enforces the same rules on
// the OTHER creation path — an agent's `gh issue create` becomes a raw
// POST /repos/{owner}/{repo}/issues that never touches the watcher — so the
// logic lives here as a single source of truth for both callers. It is a
// stdlib-only Crux ring leaf; pkg/issueshape re-exports it for the watcher
// and pkg/classify (hivecommons/hive#13536).
package issueshape

import (
	"regexp"
	"strings"
)

var (
	angleBracketTokenRE = regexp.MustCompile(`<([^>\n]+)>`)
	// unchosenOptionListRE matches a policy template's pick-one list left
	// verbatim ("Impact: high/medium/low", "**Severity**: critical/high/medium/low").
	unchosenOptionListRE           = regexp.MustCompile(`(?i)\b(?:critical/)?high/medium/low\b`)
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
		// A real placeholder is written tight: <like this>. Requiring no
		// surrounding whitespace is what separates it from a sentence that
		// merely contains two comparison operators -- "holds when a < b and
		// c > d in practice" yields the span "< b and c >", which trimming
		// would turn into the multi-word phrase "b and c" and refuse. That
		// sentence is ordinary prose and must be allowed through.
		if strings.TrimSpace(match[1]) != match[1] {
			continue
		}
		content := match[1]
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

// UnchosenOptionList reports whether text still carries a policy template's
// pick-one option list verbatim — "Impact: high/medium/low", "**Severity**:
// critical/high/medium/low" — which means the author never chose one. The
// hivecommons/hive#7898 issue was the quality template filed untouched:
// prose placeholders ("Detailed explanation of what needs testing and why.")
// carry no angle brackets, so UnsubstitutedTemplatePlaceholder let it
// through; the unpicked priority list is the one template fragment that
// survives any rewording of the prose. The returned string is the matched
// list for use in an error message.
func UnchosenOptionList(text string) (string, bool) {
	m := unchosenOptionListRE.FindString(text)
	return m, m != ""
}

// Every issue the hive files starts with the filing agent in brackets:
// `[scanner] …`, `[reviewer] …`, `[selfreport] …` (the owner's rule: "agents
// that need to open issues need to use [agent name]"). A filer tag is the
// lowercased agent (lane) name as the dashboard shows it, so it has the slug
// shape filerTagRE describes; a bracketed tag of any other shape
// (`[ACMM L0]`, `[Tests]`) is part of the subject, not a filer.
var (
	// leadingTagRE matches a title's leading `[tag]`, optionally preceded by
	// a short run of symbols (an emoji convention such as `🐛 [reviewer] …`)
	// captured in group 1 so the tag can be moved in front of it.
	leadingTagRE = regexp.MustCompile(`^\s*((?:[^\p{L}\p{N}\[\]\s]+\s*)?)\[([^\]\n]+)\]\s*`)
	filerTagRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

// AgentIssueTitle returns title carrying a leading `[<agent>]` tag. It is the
// one function every hive issue-creation path runs a title through, and it
// is idempotent:
//   - a title already tagged with this agent (any case, or after a leading
//     emoji) is normalised to `[agent] …` with the rest unchanged;
//   - a title another hive filer already tagged (`[selfreport] …` relayed by
//     the governor) is kept as it is, so a title is never double-tagged;
//   - a filer-chosen bracket that is not a filer name (`[ACMM L0] …`) is part
//     of the subject and gets the `[agent]` tag in front of it;
//   - the tag always comes first, before any emoji the title opens with.
//
// An empty agent leaves the title as it is.
func AgentIssueTitle(agent, title string) string {
	title = strings.TrimSpace(title)
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" || title == "" {
		return title
	}
	m := leadingTagRE.FindStringSubmatch(title)
	if m == nil {
		return "[" + agent + "] " + title
	}
	lead, tag, rest := strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), title[len(m[0]):]
	switch {
	case strings.EqualFold(tag, agent):
		return joinTitle("["+agent+"]", lead, rest)
	case !IsFilerTag(tag):
		return "[" + agent + "] " + title
	case lead == "":
		return title
	default:
		return joinTitle("["+tag+"]", lead, rest)
	}
}

func joinTitle(parts ...string) string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// IsFilerTag reports whether tag (the text inside a title's leading
// brackets) has the shape of a hive filer name: a lowercase slug.
func IsFilerTag(tag string) bool {
	return filerTagRE.MatchString(strings.TrimSpace(tag))
}

// HasAgentPrefix reports whether title starts with a filer tag (`[scanner] …`).
func HasAgentPrefix(title string) bool {
	return AgentPrefix(title) != ""
}

// AgentPrefix returns the filer tag a title leads with (`[scanner] …` →
// "scanner"), or "" when the title carries none. A leading bracket that is
// not filer-shaped (`[ACMM L0] …`) is not a filer tag.
func AgentPrefix(title string) string {
	m := leadingTagRE.FindStringSubmatch(title)
	if m == nil || strings.TrimSpace(m[1]) != "" {
		return ""
	}
	tag := strings.TrimSpace(m[2])
	if !IsFilerTag(tag) {
		return ""
	}
	return tag
}

// StripAgentPrefix returns title without its leading filer tag, trimmed. It
// is the key dedupe compares on so an issue filed before titles carried the
// filing agent still matches its prefixed re-filing. A non-filer bracket
// (`[ACMM L0] …`) is part of the subject and stays.
func StripAgentPrefix(title string) string {
	title = strings.TrimSpace(title)
	if AgentPrefix(title) == "" {
		return title
	}
	m := leadingTagRE.FindStringSubmatch(title)
	return strings.TrimSpace(title[len(m[0]):])
}

// SameTitle reports whether two issue titles are the same title once the
// leading `[<agent>]` tag is ignored: `Review backlog (security): x` and
// `[reviewer] Review backlog (security): x` are one finding, not two.
func SameTitle(a, b string) bool {
	return StripAgentPrefix(a) == StripAgentPrefix(b)
}
