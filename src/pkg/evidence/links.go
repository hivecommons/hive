package evidence

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Change-failure link rules (hivecommons/hive#12067). A merged PR's title and
// body are scanned for references to earlier PRs of the same repo; a
// referenced PR that merged within LinkWindow before this one is linked both
// ways (revert_of / followup_of on the new bundle, reverted_by /
// followed_up_by on the old). The live recorder in pkg/github and the
// history backfill in hivectl share these rules so their links agree.

// LinkWindow is how far back a referenced PR's merge may lie to be linked.
const LinkWindow = 14 * 24 * time.Hour

// MaxLinkRefs bounds how many referenced PRs one merge looks up.
const MaxLinkRefs = 10

// A reference is a keyword, then "#N" or "owner/repo#N" (optionally preceded
// by "PR" or "pull request"). GitHub's Revert button writes "Reverts
// owner/repo#N" into the body, which the revert pattern matches.
var (
	revertRef   = regexp.MustCompile(`(?i)\b(?:reverts?|reverting|revert\s+of)\b[\s:]+(?:(?:pr|pull\s+request)\s+)?([\w.-]+/[\w.-]+)?#(\d+)\b`)
	followupRef = regexp.MustCompile(`(?i)\b(?:fix(?:es|ed)?|follow[\s-]?up\s+(?:to|of|for)|regression\s+(?:from|in|of))\b[\s:]+(?:(?:pr|pull\s+request)\s+)?([\w.-]+/[\w.-]+)?#(\d+)\b`)
)

// ChangeRefs returns the PR numbers text names as reverted and as followed
// up, each in first-mention order. References to another repo and to the PR
// itself are dropped, and a PR named as both is a revert.
func ChangeRefs(repo string, number int, text string) (reverts, followups []int) {
	collect := func(re *regexp.Regexp, skip []int) []int {
		var out []int
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if m[1] != "" && !strings.EqualFold(m[1], strings.TrimSpace(repo)) {
				continue
			}
			n, err := strconv.Atoi(m[2])
			if err != nil || n <= 0 || n == number || slices.Contains(out, n) || slices.Contains(skip, n) {
				continue
			}
			out = append(out, n)
		}
		return out
	}
	reverts = collect(revertRef, nil)
	followups = collect(followupRef, reverts)
	return reverts, followups
}

// WithinLinkWindow reports whether a PR merged at earlier may be linked from
// one merged at later: earlier is not after later and at most LinkWindow
// before it.
func WithinLinkWindow(earlier, later time.Time) bool {
	age := later.Sub(earlier)
	return age >= 0 && age <= LinkWindow
}
