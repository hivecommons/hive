package pauseprov

import (
	"strings"
	"unicode"

	"github.com/hivecommons/hive/crux/logscrub"
)

// maxQuoteRunes caps free text kept from a pause or resume reason.
const maxQuoteRunes = 200

// quote makes a reason or login safe to store and show: tokens redacted,
// control characters dropped, whitespace collapsed, length capped. HTML
// escaping is the renderer's job.
func quote(s string) string {
	s = logscrub.RedactPaneTokens(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case unicode.IsControl(r):
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	rs := []rune(b.String())
	if len(rs) > maxQuoteRunes {
		return string(rs[:maxQuoteRunes-1]) + "…"
	}
	return string(rs)
}
