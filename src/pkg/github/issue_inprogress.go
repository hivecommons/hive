package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/issueclaim"
)

// The hive/in-progress label: a GitHub-visible, timestamped "an agent has
// started a PR for this issue" signal.
//
// Before this label the only forge-visible evidence that the scanner had
// picked an issue was the pull request itself. A kick lists up to
// governor.kick_limits.max_issues issues and the agent starts on few of them,
// so the hub cannot know which one it chose; the agent's own `hive-open-issue
// claim` request is the first moment anyone can. That request is the start
// signal (#10527) and this file turns it into a label plus a `hive:claim`
// marker comment carrying who, which hive and when, so a maintainer reading
// the issue, a second hive scanning the same repo, and the SLA metrics all
// read ONE record.
//
// Lifecycle:
//   - applied by the issue-request watcher on a `claim` request, next to
//     `hive/claimed-by-<agent>`; the marker comment is posted here only when
//     governor.claims is off — with it on, the ledger's 🔒 comment carries the
//     same marker (ClaimComment includes at=);
//   - removed by the pr-request watcher the moment a PR naming the issue
//     opens (the PR-claim sync then mirrors `🐝 covered by PR`), by the
//     ledger release hook (claims on), and by the enumeration sweep when the
//     marker has lapsed (agent TTL) or the issue carries PR evidence;
//   - while present with a live marker the issue is withheld from every kick
//     on every hive that enumerates the repo, whether or not that hive has
//     governor.claims on.

const (
	// InProgressLabel marks an issue a hive agent has started a PR for.
	InProgressLabel = "hive/in-progress"
	// inProgressLabelColor / inProgressLabelDescription seed the label when a
	// repo does not define it yet.
	inProgressLabelColor       = "fbca04"
	inProgressLabelDescription = "A hive agent has started work on this issue; the hive:claim marker comment says who, which hive and since when"
	// inProgressWorkBucket is the RepoWork detail bucket for a withheld issue.
	inProgressWorkBucket = "in_progress"
)

// InProgressSetting is the live hive/in-progress configuration.
type InProgressSetting struct {
	// Enabled turns the label, marker comment, sweep and kick filter on.
	Enabled bool
	// HiveID names this hive in the marker (hive=<id>).
	HiveID string
	// TTL is how long the start mark stands without a PR before the sweep
	// clears it. Zero → claims.DefaultAgentTTL.
	TTL time.Duration
}

// SetInProgress installs the live setting resolver. Nil disables.
func (c *Client) SetInProgress(fn func() InProgressSetting) {
	if c == nil {
		return
	}
	c.inProgress = fn
}

func (c *Client) inProgressSetting() InProgressSetting {
	if c == nil || c.inProgress == nil {
		return InProgressSetting{}
	}
	set := c.inProgress()
	if set.TTL <= 0 {
		set.TTL = claims.DefaultAgentTTL
	}
	return set
}

// InProgressCommentBody renders the start-mark comment: the canonical
// `hive:claim` marker (kind=agent, with at= and until=) so pkg/issueclaim and
// pkg/claims parse it like any other claim, then a human line that names the
// agent, the hive and both instants so nobody has to decode the marker.
func InProgressCommentBody(agent, hiveID string, started, expires time.Time) string {
	var b strings.Builder
	b.WriteString(claims.ClaimMarker(claims.Claim{
		Holder: agent, HolderID: agent, Kind: claims.KindAgent, Hive: hiveID,
		ClaimedAt: started, ExpiresAt: expires,
	}))
	b.WriteString("\n🛠️ Hive agent `")
	b.WriteString(agent)
	b.WriteString("`")
	if hiveID != "" {
		b.WriteString(" (hive ")
		b.WriteString(hiveID)
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " started work on this issue at %s. A pull request referencing this issue supersedes this mark; it lapses on its own at %s if none appears.",
		started.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339))
	return b.String()
}

// inProgressLabelsForClaim returns the labels a `claim` request applies: the
// per-agent ownership label and, when enabled, hive/in-progress.
func (c *Client) inProgressLabelsForClaim(agent string) []string {
	labels := []string{claimLabelPrefix + sanitizeAgentName(agent)}
	if c.inProgressSetting().Enabled {
		labels = append(labels, InProgressLabel)
	}
	return labels
}

// markInProgressStarted is the post-label half of a `claim` request: it seeds
// the label definition and, when governor.claims is not going to post the 🔒
// comment, posts the start-mark comment. Both are best-effort — the label
// add already succeeded and is the request's result; a failed comment is
// logged, never retried as a failed request (a retry would re-post it).
func (c *Client) markInProgressStarted(ctx context.Context, repo string, number int, agent string, now time.Time) {
	set := c.inProgressSetting()
	if !set.Enabled {
		return
	}
	if err := c.EnsureIssueLabel(ctx, repo, InProgressLabel, inProgressLabelColor, inProgressLabelDescription); err != nil && c.logger != nil {
		c.logger.Warn("in-progress: ensuring label definition failed", "repo", repo, "issue", number, "error", err)
	}
	if claimsOn, _ := c.issueClaimsSetting(); claimsOn {
		return
	}
	body := InProgressCommentBody(agent, set.HiveID, now, now.Add(set.TTL))
	if err := c.CreateIssueComment(ctx, repo, number, body); err != nil && c.logger != nil {
		c.logger.Warn("in-progress: posting start mark failed", "repo", repo, "issue", number, "agent", agent, "error", err)
	}
}

// clearInProgressForPR removes hive/in-progress from every issue a freshly
// opened PR names. Best-effort and off the request path: the PR is the
// stronger signal and the enumeration sweep converges the label anyway.
func (c *Client) clearInProgressForPR(ctx context.Context, repo string, issues []int) {
	if !c.inProgressSetting().Enabled || len(issues) == 0 {
		return
	}
	for _, n := range issues {
		if n <= 0 {
			continue
		}
		if err := c.RemoveLabel(ctx, repo, n, InProgressLabel); err != nil && c.logger != nil {
			c.logger.Warn("in-progress: removing label after PR open failed", "repo", repo, "issue", n, "error", err)
		}
	}
}

// SweepInProgressIssues reconciles hive/in-progress on the actionable issues
// and withholds the ones still legitimately in progress. For each issue
// carrying the label:
//   - PR evidence on the issue (covered-by-pr, likely-done or a linked PR)
//     supersedes the mark: the label comes off, the issue stays as the
//     PR-claim guard left it;
//   - otherwise the latest hive:claim marker is read (through the
//     updated_at-keyed claim cache, so one comment fetch per activity
//     change): a live marker withholds the issue from this kick; a lapsed or
//     missing one takes the label off and leaves the issue actionable.
//
// A comment fetch failure keeps the label and withholds the issue (fail
// closed for one cycle — the mark was asserted; an unreadable one must not
// hand the issue to a second agent). It returns the number withheld.
func SweepInProgressIssues(ctx context.Context, client *Client, result *ActionableResult, now time.Time, logger *slog.Logger) int {
	if client == nil || result == nil || len(result.Issues.Items) == 0 {
		return 0
	}
	set := client.inProgressSetting()
	if !set.Enabled {
		return 0
	}
	kept := make([]Issue, 0, len(result.Issues.Items))
	withheld := 0
	for _, issue := range result.Issues.Items {
		if !issueHasLabel(issue.Labels, InProgressLabel) {
			kept = append(kept, issue)
			continue
		}
		if issueHasPREvidence(issue) {
			client.removeInProgressLabel(ctx, &issue, "pr evidence", logger)
			kept = append(kept, issue)
			continue
		}
		owner, repoName := client.splitRepo(issue.Repo)
		claim, live, err := client.issueClaimFor(ctx, owner, repoName, &issue, set.TTL, now)
		switch {
		case err != nil:
			if logger != nil {
				logger.Warn("in-progress: marker fetch failed; withholding the issue this cycle",
					"repo", issue.Repo, "issue", issue.Number, "error", err)
			}
			withheld++
			appendRepoWorkIssueDetail(result, issue, inProgressWorkBucket, "hive/in-progress; marker unreadable")
		case live:
			withheld++
			appendRepoWorkIssueDetail(result, issue, inProgressWorkBucket, "In progress by "+claim.Identity+" since "+startedLabel(claim))
			if logger != nil {
				logger.Info("withholding issue: hive/in-progress with a live start mark",
					"repo", issue.Repo, "issue", issue.Number, "agent", claim.Identity,
					"started_at", claim.StartedAt, "expires_at", claim.ExpiresAt)
			}
		default:
			client.removeInProgressLabel(ctx, &issue, "start mark lapsed", logger)
			kept = append(kept, issue)
		}
	}
	if withheld == 0 {
		// Label removals may have edited issue.Labels in place; keep them.
		result.Issues.Items = kept
		return 0
	}
	result.Issues = IssueResultFromItems(kept)
	return withheld
}

func startedLabel(claim issueclaim.Claim) string {
	if claim.StartedAt.IsZero() {
		return "an unrecorded time"
	}
	return claim.StartedAt.UTC().Format(time.RFC3339)
}

// issueHasPREvidence reports whether the PR-claim guard already attached a
// pull request to the issue this cycle.
func issueHasPREvidence(issue Issue) bool {
	return len(issue.LinkedPRs) > 0 || issue.ClaimContext != nil ||
		HasCoveredByPRLabel(issue.Labels) || issueHasLabel(issue.Labels, LikelyDoneLabel)
}

func (c *Client) removeInProgressLabel(ctx context.Context, issue *Issue, why string, logger *slog.Logger) {
	issue.Labels = removeIssueLabel(issue.Labels, InProgressLabel)
	if c.client == nil {
		return
	}
	if err := c.RemoveLabel(ctx, issue.Repo, issue.Number, InProgressLabel); err != nil {
		if logger != nil {
			logger.Warn("in-progress: removing label failed", "repo", issue.Repo, "issue", issue.Number, "why", why, "error", err)
		}
		return
	}
	if logger != nil {
		logger.Info("in-progress: label removed", "repo", issue.Repo, "issue", issue.Number, "why", why)
	}
}
