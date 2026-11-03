package review

import (
	"regexp"
	"strings"
)

// noFindingsRe matches the one-line clean-review acknowledgement the
// publish instruction asks for ("**Reviewed** — no findings from <names>."),
// alone in the body. Markdown emphasis, the dash variant, the optional
// review-heads suffix and a trailing period are all tolerated; anything
// else in the body (a real finding) does not match.
var noFindingsRe = regexp.MustCompile(`(?is)\A\s*(?:\*\*|_)?\s*reviewed\s*(?:\*\*|_)?\s*[—–-]+\s*no findings from[^\n]*?\.?\s*\z`)

// NoFindingsAcknowledgement reports whether body is nothing but the clean
// acknowledgement line. In quiet notification mode the relay records such a
// review's verdict without submitting a GitHub review: the submission would
// email the author and every watcher to say nothing happened, while the
// verdict already reaches the evidence bundle and the Activity Monitor.
func NoFindingsAcknowledgement(body string) bool {
	body = strings.TrimSpace(body)
	return body != "" && noFindingsRe.MatchString(body)
}
