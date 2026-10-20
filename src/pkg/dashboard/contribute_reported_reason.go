package dashboard

import (
	"strings"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/advisory"
)

// sanitizeReportedReason prepares a relay-supplied VerdictReason for a comment
// the hub posts on GitHub under the App identity. The field is scraped from
// arbitrary agent output over the contributor-tier socket, so it is treated
// as untrusted text: it is capped at noWorkReasonMaxLen bytes without splitting
// a rune, flattened onto one line so it cannot add lines of its own to the
// comment, stripped of HTML comment delimiters so it cannot forge the
// `<!-- hive:... -->` markers the hub keys idempotency on, and run through
// advisory.NeutralizeMentions so it can never notify anyone.
func sanitizeReportedReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > noWorkReasonMaxLen {
		cut := noWorkReasonMaxLen
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	reason = strings.NewReplacer("<!--", "", "-->", "").Replace(reason)
	reason = strings.Join(strings.Fields(reason), " ")
	return strings.TrimSpace(advisory.NeutralizeMentions(reason))
}
