package advisory

import (
	"regexp"
	"strings"
)

// mentionPattern matches a GitHub @mention at a mention-valid position. The @
// must be preceded by start-of-text or a non-alphanumeric character so email
// addresses (user@example.com) are never touched. A preceding backtick is
// excluded so already-neutralized text stays stable (idempotency), and a
// preceding slash is excluded so an @ inside a URL path is never rewritten.
// The username part follows GitHub's rules: alphanumeric with interior
// hyphens, never starting or ending with a hyphen.
var mentionPattern = regexp.MustCompile("(^|[^A-Za-z0-9`/])@([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)")

const codeFenceMarker = "```"

// NeutralizeMentions rewrites every GitHub @mention in text so it can never
// notify anyone. Advisory output is posted to GitHub and the digest comment is
// REWRITTEN on every update cycle — a raw "@username" anywhere in it re-pings
// that human on every refresh, which reads as notification spam from the hive
// (see llm-d-incubation/llm-d-fast-model-actuation#580).
//
// Outside code, "@username" becomes "`username`": it renders cleanly, stays
// greppable, and never triggers a notification. Inside inline code spans and
// fenced code blocks only the "@" is dropped (backtick-wrapping there would
// corrupt the span), which still guarantees the invariant that the final body
// contains no mention-position "@username" anywhere.
//
// The function is idempotent: running it on already-neutralized text returns
// it unchanged. Email addresses, bare issue/PR refs (#123), and URLs are left
// untouched.
func NeutralizeMentions(text string) string {
	if !strings.Contains(text, "@") {
		return text
	}
	lines := strings.Split(text, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), codeFenceMarker) {
			inFence = !inFence
			continue
		}
		if inFence {
			// Fenced code block: dropping the @ is the only safe rewrite.
			lines[i] = mentionPattern.ReplaceAllString(line, "$1$2")
			continue
		}
		lines[i] = neutralizeLine(line)
	}
	return strings.Join(lines, "\n")
}

// neutralizeLine applies the mention rewrite to a single non-fence line,
// treating inline code spans separately: splitting on backticks alternates
// between text outside spans (even indices — wrap the username in backticks)
// and text inside spans (odd indices — drop the @ only, so the span is not
// double-wrapped).
func neutralizeLine(line string) string {
	if !strings.Contains(line, "@") {
		return line
	}
	segs := strings.Split(line, "`")
	for i, seg := range segs {
		if i%2 == 0 {
			segs[i] = mentionPattern.ReplaceAllString(seg, "$1`$2`")
		} else {
			segs[i] = mentionPattern.ReplaceAllString(seg, "$1$2")
		}
	}
	return strings.Join(segs, "`")
}
