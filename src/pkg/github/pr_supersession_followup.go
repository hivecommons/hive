package github

import (
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// A PR that builds on the merged closer is not superseded by it.
//
// Supersession means two PRs raced for one issue and the other one won. A PR
// opened after that winner merged, whose own title or body names the winner,
// was written knowing the issue was already closed: it is the follow-up, the
// revert or the next step, not the loser of a race. Danathar/zfs-aurora-complex#313
// removed a pin that #300 had added; its body said "the pin PR is #300 (it
// closed issue #299)", the closing-keyword parser read "closed issue #299" as
// this PR's claim, #300 resolved as the closer, both PRs touched the same
// files, and the sweep closed live work as a duplicate six minutes after it
// opened.
//
// Both halves are required. A PR opened before the closer merged that names
// it was a competing fix and is still superseded; a PR opened after the
// closer merged that does not name it is the agent that lost the race
// without knowing, and is still superseded.

// supersessionPullURLPattern is a pull request URL path: /owner/repo/pull/N.
// The host is not matched, so a GitHub Enterprise URL reads the same way.
var supersessionPullURLPattern = regexp.MustCompile(`/([\w.-]+)/([\w.-]+)/pull/(\d+)\b`)

// supersessionFollowsCloser reports whether pr was opened after closer
// merged and names closer in its title or body, as `#N`, `owner/repo#N` or a
// pull request URL. prRepo supplies the repository for a bare `#N`. A PR or
// closer with no timestamp never qualifies.
func (c *Client) supersessionFollowsCloser(prRepo string, pr *gh.PullRequest, closer supersessionCloser) bool {
	if c == nil || pr == nil || closer.Number <= 0 {
		return false
	}
	created := pr.GetCreatedAt().Time
	if created.IsZero() || closer.MergedAt.IsZero() || !created.After(closer.MergedAt) {
		return false
	}
	closerOwner, closerName := c.splitRepo(closer.Repo)
	sameRepo := func(owner, name string) bool {
		return strings.EqualFold(owner, closerOwner) && strings.EqualFold(name, closerName)
	}
	text := htmlNumericEntityPattern.ReplaceAllString(pr.GetTitle()+"\n"+pr.GetBody(), " ")
	for _, m := range issueRefPattern.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[2]); err != nil || n != closer.Number {
			continue
		}
		repo := prRepo
		if m[1] != "" {
			repo = m[1]
		}
		if sameRepo(c.splitRepo(repo)) {
			return true
		}
	}
	for _, m := range supersessionPullURLPattern.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[3]); err != nil || n != closer.Number {
			continue
		}
		if sameRepo(m[1], m[2]) {
			return true
		}
	}
	return false
}
