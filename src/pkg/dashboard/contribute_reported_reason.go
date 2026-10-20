package dashboard

import (
	"strings"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/advisory"
)

// truncateReasonBytes caps a reason at noWorkReasonMaxLen bytes without splitting
// a UTF-8 rune. It preserves the retained text verbatim for ledger auditing;
// GitHub comments apply their additional sanitization in sanitizeReportedReason.
func truncateReasonBytes(reason string) string {
	if len(reason) <= noWorkReasonMaxLen {
		return reason
	}
	cut := noWorkReasonMaxLen
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// sanitizeReportedReason prepares a relay-supplied VerdictReason for a comment
// the hub posts on GitHub under the App identity. The field is scraped from
// arbitrary agent output over the contributor-tier socket, so it is treated
// as untrusted text: it is capped at noWorkReasonMaxLen bytes without splitting
// a rune, stripped of backticks so no code fence or span can shield a mention
// from advisory.NeutralizeMentions, stripped of HTML comment delimiters (to a
// fixed point, so nested delimiters cannot reassemble one) so it cannot forge
// the `<!-- hive:... -->` markers the hub keys idempotency on, flattened onto
// one line so it cannot add lines of its own to the comment, and run through
// advisory.NeutralizeMentions so it can never notify anyone.
//
// Nothing after the delimiter strip may delete a character: an earlier
// version removed backticks after it, so "<!`--" survived the strip and became
// "<!--" once the backtick went. Whitespace flattening always leaves one space
// where a run was, and NeutralizeMentions never drops a character without
// putting a backtick in its place ("@name" -> "`name`"), so neither can splice
// "<!" and "--" back together.
func sanitizeReportedReason(reason string) string {
	reason = truncateReasonBytes(strings.TrimSpace(reason))
	reason = strings.ReplaceAll(reason, "`", "")
	reason = stripHTMLCommentDelimiters(reason)
	reason = strings.Join(strings.Fields(reason), " ")
	return strings.TrimSpace(advisory.NeutralizeMentions(reason))
}

// stripHTMLCommentDelimiters removes every "<!--" and "-->" from s. A single
// strings.Replacer pass is not enough: removing an inner delimiter can splice
// its neighbours into a new one ("<!<!---->--" -> "<!--"), so the replacer is
// run until the string stops changing. Each pass strictly shrinks the string,
// so the loop terminates.
func stripHTMLCommentDelimiters(s string) string {
	r := strings.NewReplacer("<!--", "", "-->", "")
	for {
		next := r.Replace(s)
		if next == s {
			return s
		}
		s = next
	}
}
