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

// noFindingsPhraseRe matches the free-text way a reviewer says it found
// nothing ("No findings.", "no findings from this perspective", "I have no
// findings") anywhere in a body.
var noFindingsPhraseRe = regexp.MustCompile(`(?i)\bno\s+(?:further\s+|other\s+|additional\s+)?findings\b`)

// CleanReview reports whether a review body submitted with reports is a
// clean review in substance, not only in form: either the one-line
// acknowledgement, or a body that says it has no findings while every
// report it rides with approves (or is not applicable) with an empty
// findings list and asks for no decision. The plain reviewer lanes narrate
// their reading ("Read the tree at abc1234 … Looks correct to me. No
// findings.") and so escaped NoFindingsAcknowledgement, costing a review
// email per clean lane per head (hivecommons/hive#16433). A body carrying a
// hidden hive marker (a decision question, a redundant-of pointer) is never
// clean: something downstream reads it.
func CleanReview(body string, reports []PerspectiveReport) bool {
	body = strings.TrimSpace(body)
	if body == "" {
		return false
	}
	if noFindingsRe.MatchString(body) {
		return true
	}
	if len(reports) == 0 || strings.Contains(body, "<!--") || !noFindingsPhraseRe.MatchString(body) {
		return false
	}
	for _, r := range reports {
		if r.DecisionRequested || len(r.Findings) > 0 {
			return false
		}
		if !r.NotApplicable && r.Verdict != VerdictApprove {
			return false
		}
	}
	return true
}
