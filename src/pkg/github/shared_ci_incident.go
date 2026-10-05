package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// sharedCIMarkerRE matches the hidden marker a fix lane stamps on the single
// comment it leaves when hive-baseline-check.sh returns DEFER_TO_INCIDENT for
// a red PR: `<!-- hive-shared-ci-<n> -->`, where <n> is the [shared-ci]
// incident issue in the same repository (src/policies/defaults/*, #10441).
// .github/scripts/pr-auto-update.sh greps for the same marker when the
// incident closes.
var sharedCIMarkerRE = regexp.MustCompile(`<!-- hive-shared-ci-([1-9][0-9]*) -->`)

// newestSharedCIMarker returns the incident number named by the newest
// shared-CI marker in texts, which must be ordered oldest first (PR comments
// in creation order). Zero means no marker.
func newestSharedCIMarker(texts []string) int {
	newest := 0
	for _, text := range texts {
		for _, m := range sharedCIMarkerRE.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				newest = n
			}
		}
	}
	return newest
}

// openSharedCIIncident returns the shared-CI incident a red PR has already
// been deferred to, when that incident issue is still open; otherwise 0
// (hivecommons/hive#10528). A PR deferred to an open incident has no repair
// an agent can make, so the kick builders keep it out of their repair lists;
// once the incident closes the marker is stale and the PR is listed again.
//
// Every lookup failure returns 0: listing a deferred PR costs one redundant
// triage, hiding a PR that is not deferred stalls it, so the error side is
// the listed side. states memoizes incident-open lookups for one enrichment
// pass, keyed "owner/repo#n", so N PRs deferred to one incident cost one
// issue GET.
func (c *Client) openSharedCIIncident(ctx context.Context, pr *PullRequest, states map[string]bool) int {
	owner, repo := c.splitRepo(pr.Repo)
	ctx = WithRESTCaller(ctx, "hive:shared_ci_incident")
	comments, err := c.listIssueComments(ctx, owner, repo, pr.Number)
	if err != nil {
		c.logger.Debug("shared-CI marker lookup failed; PR stays in the repair list", "repo", pr.Repo, "pr", pr.Number, "error", err)
		return 0
	}
	texts := make([]string, 0, len(comments))
	for _, comment := range comments {
		texts = append(texts, comment.GetBody())
	}
	incident := newestSharedCIMarker(texts)
	if incident == 0 {
		return 0
	}
	key := fmt.Sprintf("%s/%s#%d", owner, repo, incident)
	open, known := states[key]
	if !known {
		issue, _, err := c.client.Issues.Get(ctx, owner, repo, incident)
		if err != nil {
			c.logger.Debug("shared-CI incident state lookup failed; PR stays in the repair list", "repo", pr.Repo, "pr", pr.Number, "incident", incident, "error", err)
			return 0
		}
		open = issue.GetState() == "open"
		states[key] = open
	}
	if !open {
		return 0
	}
	return incident
}
