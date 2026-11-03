package dashboard

import (
	"regexp"
	"strconv"
	"strings"
)

// Merge-refusal kinds (StageTrackerMergeGate.MergeFailedKind): what GitHub's
// refusal of the hive's merge call means, in a token the SPA maps to a
// short chip detail. The raw error text never reaches the chip
// (hivecommons/docs#512 drew "PUT https://api.github.com/repos/…" across
// the Merge gate column); it rides the tooltip's Details line only.
const (
	// MergeRefusalChecksPending: 405 "N of M required status checks are
	// expected" — the head's required checks have not reported; the merge
	// call was early, not refused for cause.
	MergeRefusalChecksPending = "checks-pending"
	// MergeRefusalProtection: 405 "not mergeable" / blocked by branch
	// protection or a ruleset.
	MergeRefusalProtection = "protection"
	// MergeRefusalReview: 405 an approving review is required.
	MergeRefusalReview = "review"
	// MergeRefusalMethod: 405 the merge method is switched off.
	MergeRefusalMethod = "method"
	// MergeRefusalMergeQueue: 405 the base only merges through the queue.
	MergeRefusalMergeQueue = "merge-queue"
	// MergeRefusalHeadMoved: 409 the head (or base) moved under the call.
	MergeRefusalHeadMoved = "head-moved"
	// MergeRefusalForbidden: 403 the App may not merge here.
	MergeRefusalForbidden = "forbidden"
	// MergeRefusalOther: anything else.
	MergeRefusalOther = "refused"
)

var (
	// "PUT https://api.github.com/repos/o/r/pulls/1/merge: 405 …" (go-github's
	// ErrorResponse) or a bare "405 …".
	mergeRefusalStatusRE = regexp.MustCompile(`(?:^|[\s:])(\d{3})\s`)
	mergeRefusalChecksRE = regexp.MustCompile(`(?i)(\d+)\s+of\s+(\d+)\s+required status checks? (?:are|is) expected|required status check ["']([^"']+)["'] is expected`)
	mergeRefusalReviewRE = regexp.MustCompile(`(?i)\d+ approving reviews? (?:is|are) required|\d+ reviews? requesting changes|review is required`)
	mergeRefusalMethodRE = regexp.MustCompile(`(?i)(?:merge commits|squash merges|rebase merges) are not allowed`)
	mergeRefusalQueueRE  = regexp.MustCompile(`(?i)through the merge queue`)
	mergeRefusalMovedRE  = regexp.MustCompile(`(?i)head branch was modified|base branch was modified|sha .* does not match|was modified\. review and try`)
)

// stageTrackerMergeRefusal translates GitHub's refusal of a merge call into
// a kind, a plain sentence for the tooltip, and — for checks-pending — how
// many required checks are still expected.
func stageTrackerMergeRefusal(raw string) (kind, text string, checks int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0
	}
	status := 0
	if m := mergeRefusalStatusRE.FindStringSubmatch(raw); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	lower := strings.ToLower(raw)
	switch {
	case mergeRefusalChecksRE.MatchString(raw):
		m := mergeRefusalChecksRE.FindStringSubmatch(raw)
		if m[1] != "" {
			checks, _ = strconv.Atoi(m[1])
			if checks == 1 {
				return MergeRefusalChecksPending, "1 required check hasn't finished", 1
			}
			return MergeRefusalChecksPending, m[1] + " required checks haven't finished", checks
		}
		return MergeRefusalChecksPending, "required check " + m[3] + " hasn't finished", 1
	case mergeRefusalQueueRE.MatchString(raw):
		return MergeRefusalMergeQueue, "this branch only merges through GitHub's merge queue", 0
	case mergeRefusalReviewRE.MatchString(raw):
		return MergeRefusalReview, "branch protection wants an approving review hive can't give", 0
	case mergeRefusalMethodRE.MatchString(raw):
		return MergeRefusalMethod, "the repository has this merge method switched off", 0
	case status == 409 || mergeRefusalMovedRE.MatchString(raw):
		return MergeRefusalHeadMoved, "the branch moved under the merge call; it needs a rebase", 0
	case status == 403 || strings.Contains(lower, "resource not accessible") || strings.Contains(lower, "forbidden"):
		return MergeRefusalForbidden, "hive isn't allowed to merge here", 0
	case status == 405 || strings.Contains(lower, "not mergeable") || strings.Contains(lower, "blocked") || strings.Contains(lower, "protected branch") || strings.Contains(lower, "rule violations"):
		return MergeRefusalProtection, "branch protection blocks it", 0
	}
	return MergeRefusalOther, "GitHub refused the merge", 0
}
