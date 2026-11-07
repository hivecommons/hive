package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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

// redPRCommentTexts lists a red PR's comment bodies, oldest first, for the
// marker lookups that keep it out of the repair lists: the shared-CI
// incident marker (hivecommons/hive#10528) and the external-CI marker
// (hivecommons/hive#11609). One listing serves both. ok is false when the
// comments cannot be read; the PR then stays listed.
//
// hiveTexts are the bodies the hive's own accounts wrote. The transient-CI
// marker is read from those alone: it re-runs the failed jobs and holds the
// fix loop until they finish, so a comment from anyone else must not be able
// to spend Actions minutes or pause repairs (hivecommons/hive#17002 review).
// A hive that cannot name its own accounts falls back to every body, as the
// other hive-authorship checks do.
func (c *Client) redPRCommentTexts(ctx context.Context, pr *PullRequest) (texts, hiveTexts []string, ok bool) {
	owner, repo := c.splitRepo(pr.Repo)
	ctx = WithRESTCaller(ctx, "hive:shared_ci_incident")
	comments, err := c.listIssueComments(ctx, owner, repo, pr.Number)
	if err != nil {
		c.logger.Debug("CI deferral marker lookup failed; PR stays in the repair list", "repo", pr.Repo, "pr", pr.Number, "error", err)
		return nil, nil, false
	}
	texts = make([]string, 0, len(comments))
	ownOnly := c.knowsOwnAccounts()
	for _, comment := range comments {
		texts = append(texts, comment.GetBody())
		if !ownOnly || c.isHiveAccount(comment.GetUser()) {
			hiveTexts = append(hiveTexts, comment.GetBody())
		}
	}
	return texts, hiveTexts, true
}

// sharedCIRerunAwaitGrace is how long after a [shared-ci] incident closes a
// PR deferred to it stays deferred while it waits for SweepSharedCIReruns
// to re-run its failed jobs. The sweep runs every 15 minutes
// (cmd/hive sharedCIRerunInterval); between the close and that re-run the
// head is still red only because of the incident, so handing it back to
// its author then posted a "the hive will not merge it until the head
// changes" nudge for a check the hive was about to re-run.
const sharedCIRerunAwaitGrace = 30 * time.Minute

// sharedCIIncidentState is one incident issue's state as one enrichment pass
// read it.
type sharedCIIncidentState struct {
	open bool
	// awaitingRerun is set for an incident closed as completed within
	// sharedCIRerunAwaitGrace: the rerun sweep has not had its turn yet.
	awaitingRerun bool
}

// openSharedCIIncident returns the shared-CI incident a red PR has already
// been deferred to, when that incident issue is still open; otherwise 0
// (hivecommons/hive#10528). texts are the PR's comment bodies, oldest first.
// A PR deferred to an open incident has no repair an agent can make, so the
// kick builders keep it out of their repair lists; once the incident closes
// the marker is stale and the PR is listed again.
//
// An incident closed as completed in the last sharedCIRerunAwaitGrace still
// defers the PR until the rerun sweep has stamped its
// `<!-- hive-shared-ci-rerun-<n> -->` marker: the red is the incident's
// until the failed jobs have run again on the repaired base. A
// not-planned close gets no re-run, so it releases the PR at once.
//
// Every lookup failure returns 0: listing a deferred PR costs one redundant
// triage, hiding a PR that is not deferred stalls it, so the error side is
// the listed side. states memoizes incident lookups for one enrichment
// pass, keyed "owner/repo#n", so N PRs deferred to one incident cost one
// issue GET.
func (c *Client) openSharedCIIncident(ctx context.Context, pr *PullRequest, texts []string, states map[string]sharedCIIncidentState) int {
	incident := newestSharedCIMarker(texts)
	if incident == 0 {
		return 0
	}
	owner, repo := c.splitRepo(pr.Repo)
	ctx = WithRESTCaller(ctx, "hive:shared_ci_incident")
	key := fmt.Sprintf("%s/%s#%d", owner, repo, incident)
	state, known := states[key]
	if !known {
		issue, _, err := c.client.Issues.Get(ctx, owner, repo, incident)
		if err != nil {
			c.logger.Debug("shared-CI incident state lookup failed; PR stays in the repair list", "repo", pr.Repo, "pr", pr.Number, "incident", incident, "error", err)
			return 0
		}
		state.open = issue.GetState() == "open"
		if !state.open && issue.ClosedAt != nil && !strings.EqualFold(issue.GetStateReason(), "not_planned") {
			state.awaitingRerun = time.Since(issue.GetClosedAt().Time) < sharedCIRerunAwaitGrace
		}
		states[key] = state
	}
	if state.open {
		return incident
	}
	if state.awaitingRerun && !sharedCIRerunStamped(texts, incident) {
		return incident
	}
	return 0
}

// sharedCIRerunStamped reports whether the rerun sweep has already re-run
// this PR's failed jobs for incident (its `<!-- hive-shared-ci-rerun-<n> -->`
// marker is in texts).
func sharedCIRerunStamped(texts []string, incident int) bool {
	for _, text := range texts {
		for _, m := range sharedCIRerunMarkerRE.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n == incident {
				return true
			}
		}
	}
	return false
}
