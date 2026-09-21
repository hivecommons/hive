package github

import (
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/logscrub"
)

// Refusing a review whose evidence is a line the reviewer never actually saw.
//
// hivecommons/hive#8067: a reviewer quoted
//
//	stdin_config="$(printf 'header = "Authorization: ******"\n' "$TOKEN")"
//
// and reported that the format string had no `%s`. The file said
// `Authorization: Bearer %s`; a secret scrubber somewhere on the path between
// the repository and the model had masked the credential-shaped literal, and
// the model reasoned correctly about text that was wrong. The review was
// posted as-is, so the mask reached the PR author looking like their own code.
//
// The reviewer prompts now name this failure (pkg/review.groundingSection,
// policies/reviewer-*.md), but a prompt is guidance and this is a wrong-answer
// generator that a maintainer acts on. So the relay refuses it structurally:
// masked text quoted AS CODE is, by construction, not evidence, because the
// scrubber removed exactly the characters the finding turns on.
//
// Two deliberate limits keep this from suppressing legitimate reviews:
//
//  1. Only quoted text counts — a fenced block or an inline code span. Prose
//     ABOUT redaction ("the relay redacts the token before forwarding it") is
//     a real thing to review and passes untouched.
//  2. The marker vocabulary (logscrub.FindRedactionMarker) requires an
//     explicit redaction word or an opaque mask in unmistakable value
//     position, so a quoted C banner comment or a shell glob is not a match.

// fencedBlock matches a ``` or ~~~ delimited block, including an unterminated
// one at the end of the body — an agent that truncates mid-quote is exactly
// the case worth catching.
var fencedBlock = regexp.MustCompile("(?s)(?:^|\n)[ \t]*(?:```|~~~)[^\n]*\n(.*?)(?:\n[ \t]*(?:```|~~~)|$)")

// inlineCodeSpan matches a single-backtick span. Kept to one line on purpose:
// a backtick pair spanning a paragraph break is far more likely to be stray
// punctuation than a quotation.
var inlineCodeSpan = regexp.MustCompile("`([^`\n]+)`")

// findQuotedRedaction returns the redaction marker and the quoted fragment
// carrying it, for the first quotation in body that presents masked text as
// code. ok is false when the body quotes no masked text.
func findQuotedRedaction(body string) (marker, quote string, ok bool) {
	for _, quoted := range quotedFragments(body) {
		if m := logscrub.FindRedactionMarker(quoted); m != "" {
			return m, strings.TrimSpace(quoted), true
		}
	}
	return "", "", false
}

// quotedFragments returns the code-quoted spans of a Markdown body: the lines
// inside fenced blocks first, then inline spans outside them.
func quotedFragments(body string) []string {
	var out []string
	rest := body
	for _, block := range fencedBlock.FindAllStringSubmatchIndex(body, -1) {
		content := body[block[2]:block[3]]
		out = append(out, strings.Split(content, "\n")...)
		// Blank the whole block so its lines are not re-scanned as inline
		// spans; a fence containing an odd number of backticks would
		// otherwise pair them across the block boundary.
		rest = strings.Replace(rest, body[block[0]:block[1]], "\n", 1)
	}
	for _, span := range inlineCodeSpan.FindAllStringSubmatch(rest, -1) {
		out = append(out, span[1])
	}
	return out
}

// redactedQuoteRefusal returns the error text for a review body that quotes
// masked text, or "" when the body is clean. The message names the marker so
// the agent can find the quotation it must re-read, and says what to do — an
// unexplained refusal just gets retried with the same body.
func redactedQuoteRefusal(body string) string {
	marker, quote, ok := findQuotedRedaction(body)
	if !ok {
		return ""
	}
	if r := []rune(quote); len(r) > 160 {
		quote = string(r[:160]) + "…"
	}
	return "review body quotes text a secret scrubber already masked (" + marker +
		"), so the quotation is not what the file says: " + quote +
		" — re-read the line at the reviewed commit and quote it from there." +
		" A mask is a hive artifact, never a defect in the change under review." +
		" If the placeholder itself is genuinely your subject (reviewing a scrubber," +
		" say), describe it in prose instead of quoting it as code."
}
